package store

import (
	"fmt"
	"time"
)

// ResetTables truncates the given runtime tables in a single transaction.
// Used by the recover command's state reset path (REQ-036). Failing a
// single delete rolls the whole reset back.
func (s *dbStore) ResetTables(tables []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin reset tx: %w", err)
	}
	for _, table := range tables {
		if _, err := tx.Exec("DELETE FROM " + table); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: clear %s: %w", table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit reset tx: %w", err)
	}
	return nil
}

// CountTasksByIssue returns the number of task_queue rows for the given
// (repo, issueNum). Used by the operator detector to gate alerts on
// whether any task ever existed for an issue.
func (s *dbStore) CountTasksByIssue(repo string, issueNum int) (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(1) FROM task_queue WHERE repo = ? AND issue_num = ?`,
		repo, issueNum,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: count tasks for %s#%d: %w", repo, issueNum, err)
	}
	return count, nil
}

// RecentAlertPayloads returns the payload column of the most recent
// `limit` events with the given type, newest first. Used by the
// operator detector's duplicate-suppression window.
func (s *dbStore) RecentAlertPayloads(eventType string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 256
	}
	rows, err := s.db.Query(
		`SELECT payload FROM events WHERE type = ? ORDER BY id DESC LIMIT ?`,
		eventType, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query recent alerts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]string, 0, limit)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: scan recent alert: %w", err)
		}
		out = append(out, payload)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate recent alerts: %w", err)
	}
	return out, nil
}

// MarkStaleWorkersOffline flips any worker whose status is "online" but
// whose last_heartbeat is older than `threshold` to "offline". Used by
// the registry to detect dead workers. Returns the first error
// encountered; partial progress (some workers flipped) is not rolled
// back, mirroring the previous registry behaviour.
func (s *dbStore) MarkStaleWorkersOffline(threshold time.Duration) error {
	rows, err := s.db.Query(
		`SELECT id FROM workers WHERE status = 'online' AND last_heartbeat < datetime('now', ?)`,
		fmt.Sprintf("-%d seconds", int(threshold.Seconds())),
	)
	if err != nil {
		return fmt.Errorf("store: query stale workers: %w", err)
	}
	var staleIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: scan stale worker: %w", err)
		}
		staleIDs = append(staleIDs, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("store: iterate stale workers: %w", err)
	}
	_ = rows.Close()

	for _, id := range staleIDs {
		if err := s.UpdateWorkerStatus(id, "offline"); err != nil {
			return fmt.Errorf("store: mark worker %q offline: %w", id, err)
		}
	}
	return nil
}
