package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// ErrDuplicateWebhook is returned by SaveWebhook when a webhook with the same
// (token, dedup key) already exists. The caller can look the original up with
// WebhookByDedupKey and replay its response instead of enqueuing a duplicate.
var ErrDuplicateWebhook = errors.New("duplicate webhook")

// sqliteConstraintUnique is SQLITE_CONSTRAINT_UNIQUE: the partial unique index
// on (token_id, dedup_key) rejected a duplicate delivery.
const sqliteConstraintUnique = 2067

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
			id, token_id, method, path, headers, body, response_body, status_code, received_at, delivered_at, dedup_key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`,
		webhook.ID,
		webhook.TokenID,
		webhook.Method,
		webhook.Path,
		string(headers),
		webhook.Body,
		webhook.ResponseBody,
		webhook.StatusCode,
		formatTime(webhook.ReceivedAt),
		nullableString(webhook.DedupKey),
	)
	if isUniqueViolation(err) {
		return ErrDuplicateWebhook
	}
	return err
}

// nullableString stores the empty string as SQL NULL so the partial unique
// index (which only covers non-null keys) never collides keyless rows.
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func isUniqueViolation(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqliteConstraintUnique
}

// WebhookByDedupKey returns the earliest webhook stored for tokenID under
// dedupKey, or sql.ErrNoRows if none. Used to replay the original response to a
// duplicate delivery.
func (s *Store) WebhookByDedupKey(tokenID, dedupKey string) (Webhook, error) {
	rows, err := s.db.Query(
		`SELECT id, token_id, method, path, headers, body, response_body, status_code, received_at, delivered_at
		 FROM webhooks
		 WHERE token_id = ? AND dedup_key = ?
		 ORDER BY received_at ASC
		 LIMIT 1`,
		tokenID,
		dedupKey,
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

// DeleteWebhooksOlderThan removes webhooks received before cutoff and returns
// the number deleted. Lexical comparison of the fixed-width timestamp format
// matches chronological order.
func (s *Store) DeleteWebhooksOlderThan(cutoff time.Time) (int64, error) {
	result, err := s.db.Exec(`DELETE FROM webhooks WHERE received_at < ?`, formatTime(cutoff.UTC()))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// DeleteWebhooksOverCountPerToken keeps the most recent max webhooks for each
// token and removes the rest, returning the number deleted.
func (s *Store) DeleteWebhooksOverCountPerToken(max int) (int64, error) {
	if max <= 0 {
		return 0, nil
	}
	result, err := s.db.Exec(
		`DELETE FROM webhooks WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (
					PARTITION BY token_id ORDER BY received_at DESC, id DESC
				) AS rn
				FROM webhooks
			) WHERE rn > ?
		)`,
		max,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
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
