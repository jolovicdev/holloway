package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Token struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

type Webhook struct {
	ID           string
	TokenID      string
	Method       string
	Path         string
	Headers      map[string][]string
	Body         string
	ResponseBody string
	StatusCode   int
	ReceivedAt   time.Time
	DeliveredAt  time.Time
}

type WebhookStatusFilter string

const (
	WebhookStatusPending   WebhookStatusFilter = "pending"
	WebhookStatusDelivered WebhookStatusFilter = "delivered"
	WebhookStatus2xx       WebhookStatusFilter = "2xx"
	WebhookStatus3xx       WebhookStatusFilter = "3xx"
	WebhookStatus4xx       WebhookStatusFilter = "4xx"
	WebhookStatus5xx       WebhookStatusFilter = "5xx"
)

type WebhookQuery struct {
	Search       string
	PathContains string
	Status       WebhookStatusFilter
	ReceivedFrom time.Time
	ReceivedTo   time.Time
	Limit        int
	Offset       int
}

type WebhookPage struct {
	Webhooks []Webhook
	Total    int
	Limit    int
	Offset   int
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	statements := []string{
		`PRAGMA journal_mode = WAL`,
		`CREATE TABLE IF NOT EXISTS tokens (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			tunnel_secret_hash TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS webhooks (
			id TEXT PRIMARY KEY,
			token_id TEXT NOT NULL,
			method TEXT NOT NULL,
			path TEXT NOT NULL,
			headers JSON NOT NULL,
			body TEXT NOT NULL,
			response_body TEXT NOT NULL DEFAULT '',
			status_code INTEGER NOT NULL DEFAULT 0,
			received_at DATETIME NOT NULL,
			delivered_at DATETIME
		)`,
		`CREATE INDEX IF NOT EXISTS webhooks_token_pending_idx
			ON webhooks(token_id, status_code, received_at)`,
		`CREATE INDEX IF NOT EXISTS webhooks_received_idx
			ON webhooks(received_at DESC)`,
	}

	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return err
		}
	}
	if err := s.ensureColumn("tokens", "tunnel_secret_hash", `ALTER TABLE tokens ADD COLUMN tunnel_secret_hash TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ensureColumn("webhooks", "response_body", `ALTER TABLE webhooks ADD COLUMN response_body TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	return nil
}

func (s *Store) CreateToken(id, name string) (string, error) {
	secret, err := newTunnelSecret()
	if err != nil {
		return "", err
	}
	if err := s.CreateTokenWithTunnelSecret(id, name, secret); err != nil {
		return "", err
	}
	return secret, nil
}

func (s *Store) CreateTokenWithTunnelSecret(id, name, tunnelSecret string) error {
	if id == "" {
		return errors.New("token id is required")
	}
	if tunnelSecret == "" {
		return errors.New("tunnel secret is required")
	}
	if name == "" {
		name = id
	}

	_, err := s.db.Exec(
		`INSERT INTO tokens (id, name, tunnel_secret_hash, created_at) VALUES (?, ?, ?, ?)`,
		id,
		name,
		hashTunnelSecret(tunnelSecret),
		formatTime(time.Now().UTC()),
	)
	return err
}

func (s *Store) EnsureToken(id, name, tunnelSecret string) error {
	if id == "" {
		return errors.New("token id is required")
	}
	if tunnelSecret == "" {
		return errors.New("tunnel secret is required")
	}
	if name == "" {
		name = id
	}

	_, err := s.db.Exec(
		`INSERT INTO tokens (id, name, tunnel_secret_hash, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			tunnel_secret_hash = excluded.tunnel_secret_hash`,
		id,
		name,
		hashTunnelSecret(tunnelSecret),
		formatTime(time.Now().UTC()),
	)
	return err
}

func (s *Store) DeleteToken(id string) error {
	_, err := s.db.Exec(`DELETE FROM tokens WHERE id = ?`, id)
	return err
}

func (s *Store) TokenExists(id string) (bool, error) {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM tokens WHERE id = ?`, id).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *Store) TokenMatchesTunnelSecret(id, tunnelSecret string) (bool, error) {
	if tunnelSecret == "" {
		return false, nil
	}

	var storedHash string
	if err := s.db.QueryRow(`SELECT tunnel_secret_hash FROM tokens WHERE id = ?`, id).Scan(&storedHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return constantTimeStringEqual(storedHash, hashTunnelSecret(tunnelSecret)), nil
}

func (s *Store) ListTokens() ([]Token, error) {
	rows, err := s.db.Query(`SELECT id, name, created_at FROM tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tokens []Token
	for rows.Next() {
		token, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

func (s *Store) SaveWebhook(webhook Webhook) error {
	if webhook.ID == "" {
		return errors.New("webhook id is required")
	}
	if webhook.TokenID == "" {
		return errors.New("token id is required")
	}
	if webhook.ReceivedAt.IsZero() {
		webhook.ReceivedAt = time.Now().UTC()
	}
	if webhook.Headers == nil {
		webhook.Headers = map[string][]string{}
	}

	headers, err := json.Marshal(webhook.Headers)
	if err != nil {
		return fmt.Errorf("marshal headers: %w", err)
	}

	_, err = s.db.Exec(
		`INSERT INTO webhooks (
			id, token_id, method, path, headers, body, response_body, status_code, received_at, delivered_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		webhook.ID,
		webhook.TokenID,
		webhook.Method,
		webhook.Path,
		string(headers),
		webhook.Body,
		webhook.ResponseBody,
		webhook.StatusCode,
		formatTime(webhook.ReceivedAt),
	)
	return err
}

func (s *Store) MarkDelivered(id string, statusCode int, responseBody string) error {
	_, err := s.db.Exec(
		`UPDATE webhooks SET status_code = ?, response_body = ?, delivered_at = ? WHERE id = ?`,
		statusCode,
		responseBody,
		formatTime(time.Now().UTC()),
		id,
	)
	return err
}

func (s *Store) PendingWebhooks(tokenID string) ([]Webhook, error) {
	return s.queryWebhooks(
		`SELECT id, token_id, method, path, headers, body, response_body, status_code, received_at, delivered_at
		 FROM webhooks
		 WHERE token_id = ? AND status_code = 0
		 ORDER BY received_at ASC`,
		tokenID,
	)
}

func (s *Store) LastWebhooks(limit int) ([]Webhook, error) {
	if limit <= 0 {
		limit = 50
	}
	page, err := s.ListWebhooks(WebhookQuery{Limit: limit})
	if err != nil {
		return nil, err
	}
	return page.Webhooks, nil
}

func (s *Store) ListWebhooks(query WebhookQuery) (WebhookPage, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := query.Offset
	if offset < 0 {
		offset = 0
	}

	where, args := webhookWhere(query)
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM webhooks`+where, args...).Scan(&total); err != nil {
		return WebhookPage{}, err
	}

	queryArgs := append(append([]any(nil), args...), limit, offset)
	webhooks, err := s.queryWebhooks(
		`SELECT id, token_id, method, path, headers, body, response_body, status_code, received_at, delivered_at
		 FROM webhooks`+where+`
		 ORDER BY received_at DESC
		 LIMIT ? OFFSET ?`,
		queryArgs...,
	)
	if err != nil {
		return WebhookPage{}, err
	}
	return WebhookPage{
		Webhooks: webhooks,
		Total:    total,
		Limit:    limit,
		Offset:   offset,
	}, nil
}

func (s *Store) LastWebhooksForToken(tokenID string, limit int) ([]Webhook, error) {
	if limit <= 0 {
		return nil, nil
	}
	return s.queryWebhooks(
		`SELECT id, token_id, method, path, headers, body, response_body, status_code, received_at, delivered_at
		 FROM webhooks
		 WHERE token_id = ?
		 ORDER BY received_at DESC
		 LIMIT ?`,
		tokenID,
		limit,
	)
}

func (s *Store) GetWebhook(id string) (Webhook, error) {
	rows, err := s.db.Query(
		`SELECT id, token_id, method, path, headers, body, response_body, status_code, received_at, delivered_at
		 FROM webhooks
		 WHERE id = ?`,
		id,
	)
	if err != nil {
		return Webhook{}, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Webhook{}, err
		}
		return Webhook{}, sql.ErrNoRows
	}
	return scanWebhook(rows)
}

func (s *Store) LastWebhooksForTokenExcluding(tokenID string, limit int, excludedIDs map[string]struct{}) ([]Webhook, error) {
	webhooks, err := s.LastWebhooksForToken(tokenID, limit+len(excludedIDs))
	if err != nil {
		return nil, err
	}

	kept := webhooks[:0]
	for _, webhook := range webhooks {
		if _, excluded := excludedIDs[webhook.ID]; excluded {
			continue
		}
		kept = append(kept, webhook)
		if len(kept) == limit {
			break
		}
	}
	return kept, nil
}

func webhookWhere(query WebhookQuery) (string, []any) {
	var clauses []string
	var args []any

	if search := strings.TrimSpace(query.Search); search != "" {
		pattern := "%" + strings.ToLower(search) + "%"
		clauses = append(clauses, `(LOWER(id) LIKE ? OR LOWER(token_id) LIKE ? OR LOWER(method) LIKE ? OR LOWER(path) LIKE ? OR LOWER(body) LIKE ? OR LOWER(response_body) LIKE ?)`)
		args = append(args, pattern, pattern, pattern, pattern, pattern, pattern)
	}

	if path := strings.TrimSpace(query.PathContains); path != "" {
		clauses = append(clauses, `LOWER(path) LIKE ?`)
		args = append(args, "%"+strings.ToLower(path)+"%")
	}

	switch query.Status {
	case WebhookStatusPending:
		clauses = append(clauses, `status_code = 0`)
	case WebhookStatusDelivered:
		clauses = append(clauses, `status_code > 0`)
	case WebhookStatus2xx:
		clauses = append(clauses, `status_code >= 200 AND status_code < 300`)
	case WebhookStatus3xx:
		clauses = append(clauses, `status_code >= 300 AND status_code < 400`)
	case WebhookStatus4xx:
		clauses = append(clauses, `status_code >= 400 AND status_code < 500`)
	case WebhookStatus5xx:
		clauses = append(clauses, `status_code >= 500 AND status_code < 600`)
	}

	if !query.ReceivedFrom.IsZero() {
		clauses = append(clauses, `received_at >= ?`)
		args = append(args, formatTime(query.ReceivedFrom))
	}
	if !query.ReceivedTo.IsZero() {
		clauses = append(clauses, `received_at < ?`)
		args = append(args, formatTime(query.ReceivedTo))
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func sqliteDSN(path string) string {
	if strings.Contains(path, "?") {
		return path + "&_pragma=busy_timeout%3d5000&_pragma=foreign_keys(1)"
	}
	values := url.Values{}
	values.Add("_pragma", "busy_timeout=5000")
	values.Add("_pragma", "foreign_keys(1)")
	return path + "?" + values.Encode()
}

func (s *Store) ensureColumn(table, column, statement string) error {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(statement)
	return err
}

func (s *Store) queryWebhooks(query string, args ...any) ([]Webhook, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var webhooks []Webhook
	for rows.Next() {
		webhook, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		webhooks = append(webhooks, webhook)
	}
	return webhooks, rows.Err()
}

type tokenScanner interface {
	Scan(dest ...any) error
}

func scanToken(row tokenScanner) (Token, error) {
	var token Token
	var createdAt string
	if err := row.Scan(&token.ID, &token.Name, &createdAt); err != nil {
		return Token{}, err
	}
	parsed, err := parseTime(createdAt)
	if err != nil {
		return Token{}, err
	}
	token.CreatedAt = parsed
	return token, nil
}

func scanWebhook(row tokenScanner) (Webhook, error) {
	var webhook Webhook
	var headers string
	var receivedAt string
	var deliveredAt sql.NullString
	if err := row.Scan(
		&webhook.ID,
		&webhook.TokenID,
		&webhook.Method,
		&webhook.Path,
		&headers,
		&webhook.Body,
		&webhook.ResponseBody,
		&webhook.StatusCode,
		&receivedAt,
		&deliveredAt,
	); err != nil {
		return Webhook{}, err
	}
	if err := json.Unmarshal([]byte(headers), &webhook.Headers); err != nil {
		return Webhook{}, fmt.Errorf("unmarshal headers: %w", err)
	}
	parsedReceivedAt, err := parseTime(receivedAt)
	if err != nil {
		return Webhook{}, err
	}
	webhook.ReceivedAt = parsedReceivedAt
	if deliveredAt.Valid {
		parsedDeliveredAt, err := parseTime(deliveredAt.String)
		if err != nil {
			return Webhook{}, err
		}
		webhook.DeliveredAt = parsedDeliveredAt
	}
	return webhook, nil
}

const dbTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(dbTimeFormat, value)
	if err == nil {
		return parsed, nil
	}
	return time.Parse(time.RFC3339Nano, value)
}

func formatTime(t time.Time) string {
	return t.UTC().Format(dbTimeFormat)
}

func newTunnelSecret() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return "tsec_" + hex.EncodeToString(bytes[:]), nil
}

func hashTunnelSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func constantTimeStringEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
