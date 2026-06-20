package store

import (
	"crypto/subtle"
	"database/sql"
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
	DedupKey     string
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
			delivered_at DATETIME,
			dedup_key TEXT
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
	if err := s.ensureColumn("webhooks", "dedup_key", `ALTER TABLE webhooks ADD COLUMN dedup_key TEXT`); err != nil {
		return err
	}
	// Partial unique index: only deliveries that carry a dedup key are
	// constrained, so keyless rows (dedup disabled, or pre-upgrade) never
	// collide. Created after the column exists on upgraded databases.
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS webhooks_dedup_idx ON webhooks(token_id, dedup_key) WHERE dedup_key IS NOT NULL`); err != nil {
		return err
	}
	return nil
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

type tokenScanner interface {
	Scan(dest ...any) error
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

func constantTimeStringEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
