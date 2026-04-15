package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ErrInvalidWorkerToken indicates the bearer token was missing, malformed, or revoked.
var ErrInvalidWorkerToken = fmt.Errorf("store: invalid worker token")

// IssuedWorkerToken is the one-time token material returned on creation.
type IssuedWorkerToken struct {
	WorkerID string
	KID      string
	Token    string
}

// WorkerTokenRecord stores worker token metadata without exposing the secret.
type WorkerTokenRecord struct {
	WorkerID     string
	Repo         string
	Roles        string
	Hostname     string
	Status       string
	KID          string
	RevokedAt    *time.Time
	RegisteredAt time.Time
}

// WorkerAuthRecord is the authenticated worker identity derived from a bearer token.
type WorkerAuthRecord struct {
	WorkerID string
	Repo     string
	Roles    string
	Hostname string
	Status   string
	KID      string
}

// IssueWorkerToken creates or rotates a worker token. The full bearer token is
// returned once; only its hash is persisted.
func (s *Store) IssueWorkerToken(workerID, repo string, roles []string, hostname string) (*IssuedWorkerToken, error) {
	rolesJSON, err := json.Marshal(roles)
	if err != nil {
		return nil, fmt.Errorf("store: marshal roles: %w", err)
	}
	kid, err := randomHex(6)
	if err != nil {
		return nil, fmt.Errorf("store: generate kid: %w", err)
	}
	secret, err := randomHex(24)
	if err != nil {
		return nil, fmt.Errorf("store: generate secret: %w", err)
	}
	token := kid + "." + secret
	tokenHash := hashWorkerToken(token)

	_, err = s.db.Exec(
		`INSERT INTO workers (id, repo, roles, hostname, status, token_kid, token_hash, token_revoked_at)
		 VALUES (?, ?, ?, ?, 'online', ?, ?, NULL)
		 ON CONFLICT(id) DO UPDATE SET
		 repo = excluded.repo,
		 roles = excluded.roles,
		 hostname = excluded.hostname,
		 status = excluded.status,
		 token_kid = excluded.token_kid,
		 token_hash = excluded.token_hash,
		 token_revoked_at = NULL`,
		workerID, repo, string(rolesJSON), hostname, kid, tokenHash,
	)
	if err != nil {
		return nil, fmt.Errorf("store: issue worker token: %w", err)
	}

	return &IssuedWorkerToken{
		WorkerID: workerID,
		KID:      kid,
		Token:    token,
	}, nil
}

// ListWorkerTokens returns persisted worker token metadata.
func (s *Store) ListWorkerTokens(repo string) ([]WorkerTokenRecord, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if repo == "" {
		rows, err = s.db.Query(
			`SELECT id, repo, roles, hostname, status, token_kid, token_revoked_at, registered_at
			 FROM workers
			 WHERE token_kid IS NOT NULL
			 ORDER BY id`,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT id, repo, roles, hostname, status, token_kid, token_revoked_at, registered_at
			 FROM workers
			 WHERE repo = ? AND token_kid IS NOT NULL
			 ORDER BY id`,
			repo,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("store: list worker tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WorkerTokenRecord
	for rows.Next() {
		var (
			rec        WorkerTokenRecord
			revokedRaw sql.NullString
			regRaw     string
		)
		if err := rows.Scan(&rec.WorkerID, &rec.Repo, &rec.Roles, &rec.Hostname, &rec.Status, &rec.KID, &revokedRaw, &regRaw); err != nil {
			return nil, fmt.Errorf("store: scan worker token: %w", err)
		}
		rec.RegisteredAt, _ = parseTimestamp(regRaw, "worker.registered_at")
		if revokedRaw.Valid {
			if ts, ok := parseTimestamp(revokedRaw.String, "worker.token_revoked_at"); ok {
				rec.RevokedAt = &ts
			}
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// RevokeWorkerToken invalidates a worker token immediately.
func (s *Store) RevokeWorkerToken(workerID, kid string) error {
	query := `UPDATE workers SET token_hash = NULL, token_revoked_at = CURRENT_TIMESTAMP WHERE id = ?`
	args := []any{workerID}
	if strings.TrimSpace(kid) != "" {
		query += ` AND token_kid = ?`
		args = append(args, kid)
	}
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("store: revoke worker token: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("store: revoke worker token: worker %q not found", workerID)
	}
	return nil
}

// AuthenticateWorkerToken validates a bearer token against the stored worker token hash.
func (s *Store) AuthenticateWorkerToken(token string) (*WorkerAuthRecord, error) {
	kid, ok := parseWorkerTokenKID(token)
	if !ok {
		return nil, ErrInvalidWorkerToken
	}

	var (
		rec        WorkerAuthRecord
		revokedRaw sql.NullString
	)
	err := s.db.QueryRow(
		`SELECT id, repo, roles, hostname, status, token_kid, token_revoked_at
		 FROM workers
		 WHERE token_kid = ? AND token_hash = ?`,
		kid, hashWorkerToken(token),
	).Scan(&rec.WorkerID, &rec.Repo, &rec.Roles, &rec.Hostname, &rec.Status, &rec.KID, &revokedRaw)
	if err == sql.ErrNoRows {
		return nil, ErrInvalidWorkerToken
	}
	if err != nil {
		return nil, fmt.Errorf("store: authenticate worker token: %w", err)
	}
	if revokedRaw.Valid {
		return nil, ErrInvalidWorkerToken
	}
	return &rec, nil
}

func hashWorkerToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func parseWorkerTokenKID(token string) (string, bool) {
	token = strings.TrimSpace(token)
	kid, _, ok := strings.Cut(token, ".")
	if !ok || kid == "" {
		return "", false
	}
	return kid, true
}

func randomHex(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
