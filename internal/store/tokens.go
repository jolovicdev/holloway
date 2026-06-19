package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

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
