// Package store provides SQLite-backed persistence for workbuddy state.
package store

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // sqlite driver
)

// parseTimestamp attempts to parse a SQLite timestamp string using common formats.
// Returns the parsed time and true on success, or zero time and false on failure
// (with a warning logged including the field name for debugging).
func parseTimestamp(raw, fieldName string) (time.Time, bool) {
	// SQLite CURRENT_TIMESTAMP may return different formats depending on driver.
	for _, layout := range []string{
		"2006-01-02 15:04:05",
		time.RFC3339,
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	log.Printf("[store] warning: failed to parse %s timestamp %q", fieldName, raw)
	return time.Time{}, false
}

// Task status constants.
const (
	TaskStatusPending   = "pending"
	TaskStatusRunning   = "running"
	TaskStatusCompleted = "completed"
	TaskStatusFailed    = "failed"
	TaskStatusTimeout   = "timeout"
)

// Store provides typed CRUD access to the workbuddy SQLite database.
type Store struct {
	db *sql.DB
}

// NewStore opens (or creates) the SQLite database at dbPath,
// creates the parent directory if needed, enables WAL mode,
// and ensures all tables exist.
func NewStore(dbPath string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: create dir: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("store: open db: %w", err)
	}

	// Enable WAL mode.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: enable WAL: %w", err)
	}

	s := &Store{db: db}
	if err := s.createTables(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: create tables: %w", err)
	}
	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB returns the underlying *sql.DB for advanced use cases.
func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) createTables() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts DATETIME DEFAULT CURRENT_TIMESTAMP,
			type TEXT NOT NULL,
			repo TEXT NOT NULL,
			issue_num INTEGER,
			payload TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS task_queue (
			id TEXT PRIMARY KEY,
			repo TEXT NOT NULL,
			issue_num INTEGER NOT NULL,
			agent_name TEXT NOT NULL,
			worker_id TEXT,
			status TEXT NOT NULL DEFAULT 'pending',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS workers (
			id TEXT PRIMARY KEY,
			repo TEXT NOT NULL,
			roles TEXT NOT NULL,
			hostname TEXT,
			status TEXT NOT NULL DEFAULT 'online',
			last_heartbeat DATETIME DEFAULT CURRENT_TIMESTAMP,
			registered_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS transition_counts (
			repo TEXT NOT NULL,
			issue_num INTEGER NOT NULL,
			from_state TEXT NOT NULL,
			to_state TEXT NOT NULL,
			count INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (repo, issue_num, from_state, to_state)
		)`,
		`CREATE TABLE IF NOT EXISTS issue_cache (
			repo TEXT NOT NULL,
			issue_num INTEGER NOT NULL,
			labels TEXT,
			body TEXT NOT NULL DEFAULT '',
			state TEXT,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (repo, issue_num)
		)`,
		`CREATE TABLE IF NOT EXISTS agent_sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			task_id TEXT,
			repo TEXT NOT NULL,
			issue_num INTEGER NOT NULL,
			agent_name TEXT NOT NULL,
			summary TEXT,
			raw_path TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS issue_dependencies (
			repo TEXT NOT NULL,
			issue_num INTEGER NOT NULL,
			depends_on_repo TEXT NOT NULL,
			depends_on_issue_num INTEGER NOT NULL,
			source_hash TEXT NOT NULL,
			status TEXT NOT NULL,
			PRIMARY KEY (repo, issue_num, depends_on_repo, depends_on_issue_num)
		)`,
		`CREATE TABLE IF NOT EXISTS issue_dependency_state (
			repo TEXT NOT NULL,
			issue_num INTEGER NOT NULL,
			verdict TEXT NOT NULL,
			resume_label TEXT,
			blocked_reason_hash TEXT,
			override_active INTEGER NOT NULL DEFAULT 0,
			graph_version INTEGER NOT NULL DEFAULT 0,
			last_reaction_blocked INTEGER NOT NULL DEFAULT 0,
			last_evaluated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (repo, issue_num)
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("exec %q: %w", stmt[:40], err)
		}
	}
	if _, err := s.db.Exec(`ALTER TABLE issue_cache ADD COLUMN body TEXT NOT NULL DEFAULT ''`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("store: alter issue_cache add body: %w", err)
	}
	// Forward-migrate any pre-existing issue_dependency_state rows: drop the
	// old managed-comment anchor columns by adding the new reaction column if
	// the table was created by an earlier schema. SQLite has no DROP COLUMN
	// in older versions, but the unused columns are harmless to leave; we
	// just need last_reaction_blocked to exist.
	if _, err := s.db.Exec(`ALTER TABLE issue_dependency_state ADD COLUMN last_reaction_blocked INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("store: alter issue_dependency_state add last_reaction_blocked: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// InsertEvent records an event and returns the auto-generated ID.
func (s *Store) InsertEvent(e Event) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO events (type, repo, issue_num, payload) VALUES (?, ?, ?, ?)`,
		e.Type, e.Repo, e.IssueNum, e.Payload,
	)
	if err != nil {
		return 0, fmt.Errorf("store: insert event: %w", err)
	}
	return res.LastInsertId()
}

// QueryEvents returns events matching the given repo (empty string = all repos).
func (s *Store) QueryEvents(repo string) ([]Event, error) {
	var rows *sql.Rows
	var err error
	if repo == "" {
		rows, err = s.db.Query(`SELECT id, ts, type, repo, issue_num, payload FROM events ORDER BY id`)
	} else {
		rows, err = s.db.Query(`SELECT id, ts, type, repo, issue_num, payload FROM events WHERE repo = ? ORDER BY id`, repo)
	}
	if err != nil {
		return nil, fmt.Errorf("store: query events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Event
	for rows.Next() {
		var ev Event
		var ts string
		if err := rows.Scan(&ev.ID, &ts, &ev.Type, &ev.Repo, &ev.IssueNum, &ev.Payload); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		ev.TS, _ = parseTimestamp(ts, "event.ts")
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Task Queue
// ---------------------------------------------------------------------------

// InsertTask inserts a new task into the task_queue.
func (s *Store) InsertTask(t TaskRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO task_queue (id, repo, issue_num, agent_name, worker_id, status) VALUES (?, ?, ?, ?, ?, ?)`,
		t.ID, t.Repo, t.IssueNum, t.AgentName, t.WorkerID, t.Status,
	)
	if err != nil {
		return fmt.Errorf("store: insert task: %w", err)
	}
	return nil
}

// QueryTasks returns tasks filtered by status (empty string = all).
func (s *Store) QueryTasks(status string) ([]TaskRecord, error) {
	var rows *sql.Rows
	var err error
	if status == "" {
		rows, err = s.db.Query(`SELECT id, repo, issue_num, agent_name, worker_id, status, created_at, updated_at FROM task_queue ORDER BY created_at`)
	} else {
		rows, err = s.db.Query(`SELECT id, repo, issue_num, agent_name, worker_id, status, created_at, updated_at FROM task_queue WHERE status = ? ORDER BY created_at`, status)
	}
	if err != nil {
		return nil, fmt.Errorf("store: query tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TaskRecord
	for rows.Next() {
		var t TaskRecord
		var createdAt, updatedAt string
		if err := rows.Scan(&t.ID, &t.Repo, &t.IssueNum, &t.AgentName, &t.WorkerID, &t.Status, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("store: scan task: %w", err)
		}
		t.CreatedAt, _ = parseTimestamp(createdAt, "task.created_at")
		t.UpdatedAt, _ = parseTimestamp(updatedAt, "task.updated_at")
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateTaskStatus updates the status and updated_at of a task.
func (s *Store) UpdateTaskStatus(taskID, status string) error {
	res, err := s.db.Exec(
		`UPDATE task_queue SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		status, taskID,
	)
	if err != nil {
		return fmt.Errorf("store: update task status: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("store: update task status: task %q not found", taskID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Workers
// ---------------------------------------------------------------------------

// InsertWorker registers a worker.
func (s *Store) InsertWorker(w WorkerRecord) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO workers (id, repo, roles, hostname, status) VALUES (?, ?, ?, ?, ?)`,
		w.ID, w.Repo, w.Roles, w.Hostname, w.Status,
	)
	if err != nil {
		return fmt.Errorf("store: insert worker: %w", err)
	}
	return nil
}

// QueryWorkers returns workers filtered by repo (empty = all).
func (s *Store) QueryWorkers(repo string) ([]WorkerRecord, error) {
	var rows *sql.Rows
	var err error
	if repo == "" {
		rows, err = s.db.Query(`SELECT id, repo, roles, hostname, status, last_heartbeat, registered_at FROM workers ORDER BY id`)
	} else {
		rows, err = s.db.Query(`SELECT id, repo, roles, hostname, status, last_heartbeat, registered_at FROM workers WHERE repo = ? ORDER BY id`, repo)
	}
	if err != nil {
		return nil, fmt.Errorf("store: query workers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WorkerRecord
	for rows.Next() {
		var w WorkerRecord
		var hb, ra string
		if err := rows.Scan(&w.ID, &w.Repo, &w.Roles, &w.Hostname, &w.Status, &hb, &ra); err != nil {
			return nil, fmt.Errorf("store: scan worker: %w", err)
		}
		w.LastHeartbeat, _ = parseTimestamp(hb, "worker.last_heartbeat")
		w.RegisteredAt, _ = parseTimestamp(ra, "worker.registered_at")
		out = append(out, w)
	}
	return out, rows.Err()
}

// UpdateWorkerHeartbeat updates the last_heartbeat timestamp.
func (s *Store) UpdateWorkerHeartbeat(workerID string) error {
	res, err := s.db.Exec(
		`UPDATE workers SET last_heartbeat = CURRENT_TIMESTAMP WHERE id = ?`,
		workerID,
	)
	if err != nil {
		return fmt.Errorf("store: update worker heartbeat: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("store: update worker heartbeat: worker %q not found", workerID)
	}
	return nil
}

// UpdateWorkerStatus updates a worker's status (online/offline).
func (s *Store) UpdateWorkerStatus(workerID, status string) error {
	res, err := s.db.Exec(
		`UPDATE workers SET status = ? WHERE id = ?`,
		status, workerID,
	)
	if err != nil {
		return fmt.Errorf("store: update worker status: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("store: update worker status: worker %q not found", workerID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Transition Counts
// ---------------------------------------------------------------------------

// IncrementTransition increments the counter for a state transition,
// inserting the row if it does not exist. Returns the new count.
// The upsert and read are performed in a single statement using RETURNING
// to avoid a race where another goroutine increments between the two.
func (s *Store) IncrementTransition(repo string, issueNum int, fromState, toState string) (int, error) {
	var count int
	err := s.db.QueryRow(
		`INSERT INTO transition_counts (repo, issue_num, from_state, to_state, count)
		 VALUES (?, ?, ?, ?, 1)
		 ON CONFLICT (repo, issue_num, from_state, to_state)
		 DO UPDATE SET count = count + 1
		 RETURNING count`,
		repo, issueNum, fromState, toState,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: increment transition: %w", err)
	}
	return count, nil
}

// QueryTransitionCounts returns all transition counts for a repo+issue.
func (s *Store) QueryTransitionCounts(repo string, issueNum int) ([]TransitionCount, error) {
	rows, err := s.db.Query(
		`SELECT repo, issue_num, from_state, to_state, count FROM transition_counts WHERE repo = ? AND issue_num = ?`,
		repo, issueNum,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query transition counts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TransitionCount
	for rows.Next() {
		var tc TransitionCount
		if err := rows.Scan(&tc.Repo, &tc.IssueNum, &tc.FromState, &tc.ToState, &tc.Count); err != nil {
			return nil, fmt.Errorf("store: scan transition count: %w", err)
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Issue Cache
// ---------------------------------------------------------------------------

// UpsertIssueCache inserts or updates the cached state of an issue.
func (s *Store) UpsertIssueCache(ic IssueCache) error {
	_, err := s.db.Exec(
		`INSERT INTO issue_cache (repo, issue_num, labels, body, state, updated_at)
		 VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT (repo, issue_num)
		 DO UPDATE SET labels = excluded.labels, body = excluded.body, state = excluded.state, updated_at = CURRENT_TIMESTAMP`,
		ic.Repo, ic.IssueNum, ic.Labels, ic.Body, ic.State,
	)
	if err != nil {
		return fmt.Errorf("store: upsert issue cache: %w", err)
	}
	return nil
}

// ListCachedIssueNums returns all issue numbers in the cache for a repo,
// excluding PR entries (those with state starting with "pr:").
func (s *Store) ListCachedIssueNums(repo string) ([]int, error) {
	rows, err := s.db.Query(
		`SELECT issue_num FROM issue_cache WHERE repo = ? AND (state IS NULL OR state NOT LIKE 'pr:%')`,
		repo,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list cached issue nums: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var nums []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("store: scan issue num: %w", err)
		}
		nums = append(nums, n)
	}
	return nums, rows.Err()
}

// DeleteIssueCache removes a cached issue entry.
func (s *Store) DeleteIssueCache(repo string, issueNum int) error {
	_, err := s.db.Exec(
		`DELETE FROM issue_cache WHERE repo = ? AND issue_num = ?`,
		repo, issueNum,
	)
	if err != nil {
		return fmt.Errorf("store: delete issue cache: %w", err)
	}
	return nil
}

// QueryIssueCache returns the cached issue, or nil if not found.
func (s *Store) QueryIssueCache(repo string, issueNum int) (*IssueCache, error) {
	var ic IssueCache
	var updatedAt string
	err := s.db.QueryRow(
		`SELECT repo, issue_num, labels, body, state, updated_at FROM issue_cache WHERE repo = ? AND issue_num = ?`,
		repo, issueNum,
	).Scan(&ic.Repo, &ic.IssueNum, &ic.Labels, &ic.Body, &ic.State, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: query issue cache: %w", err)
	}
	ic.UpdatedAt, _ = parseTimestamp(updatedAt, "issue_cache.updated_at")
	return &ic, nil
}

// ListIssueCaches returns all non-PR cached issues for a repo.
func (s *Store) ListIssueCaches(repo string) ([]IssueCache, error) {
	rows, err := s.db.Query(
		`SELECT repo, issue_num, labels, body, state, updated_at
		 FROM issue_cache
		 WHERE repo = ? AND (state IS NULL OR state NOT LIKE 'pr:%')
		 ORDER BY issue_num`,
		repo,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list issue caches: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []IssueCache
	for rows.Next() {
		var ic IssueCache
		var updatedAt string
		if err := rows.Scan(&ic.Repo, &ic.IssueNum, &ic.Labels, &ic.Body, &ic.State, &updatedAt); err != nil {
			return nil, fmt.Errorf("store: scan issue cache: %w", err)
		}
		ic.UpdatedAt, _ = parseTimestamp(updatedAt, "issue_cache.updated_at")
		out = append(out, ic)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Agent Sessions
// ---------------------------------------------------------------------------

// AgentSession represents a row in the agent_sessions table.
type AgentSession struct {
	ID        int64
	SessionID string
	TaskID    string
	Repo      string
	IssueNum  int
	AgentName string
	Summary   string
	RawPath   string
	CreatedAt time.Time
}

// InsertAgentSession records an agent session.
func (s *Store) InsertAgentSession(sess AgentSession) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO agent_sessions (session_id, task_id, repo, issue_num, agent_name, summary, raw_path)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sess.SessionID, sess.TaskID, sess.Repo, sess.IssueNum, sess.AgentName, sess.Summary, sess.RawPath,
	)
	if err != nil {
		return 0, fmt.Errorf("store: insert agent session: %w", err)
	}
	return res.LastInsertId()
}

// QueryAgentSessions returns sessions for a repo+issue.
func (s *Store) QueryAgentSessions(repo string, issueNum int) ([]AgentSession, error) {
	rows, err := s.db.Query(
		`SELECT id, session_id, task_id, repo, issue_num, agent_name, summary, raw_path, created_at
		 FROM agent_sessions WHERE repo = ? AND issue_num = ? ORDER BY id`,
		repo, issueNum,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query agent sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AgentSession
	for rows.Next() {
		var sess AgentSession
		var createdAt string
		if err := rows.Scan(&sess.ID, &sess.SessionID, &sess.TaskID, &sess.Repo, &sess.IssueNum,
			&sess.AgentName, &sess.Summary, &sess.RawPath, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan agent session: %w", err)
		}
		sess.CreatedAt, _ = parseTimestamp(createdAt, "agent_session.created_at")
		out = append(out, sess)
	}
	return out, rows.Err()
}

// SessionFilter specifies optional query predicates for listing sessions.
// Zero-value fields are ignored.
type SessionFilter struct {
	Repo      string
	IssueNum  int
	AgentName string
}

// ListAgentSessions returns sessions matching the given filter, ordered by
// creation time descending. Zero-value filter fields are ignored.
func (s *Store) ListAgentSessions(f SessionFilter) ([]AgentSession, error) {
	q := `SELECT id, session_id, task_id, repo, issue_num, agent_name, summary, raw_path, created_at
	      FROM agent_sessions WHERE 1=1`
	var args []any

	if f.Repo != "" {
		q += " AND repo = ?"
		args = append(args, f.Repo)
	}
	if f.IssueNum != 0 {
		q += " AND issue_num = ?"
		args = append(args, f.IssueNum)
	}
	if f.AgentName != "" {
		q += " AND agent_name = ?"
		args = append(args, f.AgentName)
	}
	q += " ORDER BY id DESC"

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list agent sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AgentSession
	for rows.Next() {
		var sess AgentSession
		var createdAt string
		if err := rows.Scan(&sess.ID, &sess.SessionID, &sess.TaskID, &sess.Repo, &sess.IssueNum,
			&sess.AgentName, &sess.Summary, &sess.RawPath, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan agent session: %w", err)
		}
		sess.CreatedAt, _ = parseTimestamp(createdAt, "agent_session.created_at")
		out = append(out, sess)
	}
	return out, rows.Err()
}

// GetAgentSession returns a single session by session_id, or nil if not found.
func (s *Store) GetAgentSession(sessionID string) (*AgentSession, error) {
	var sess AgentSession
	var createdAt string
	err := s.db.QueryRow(
		`SELECT id, session_id, task_id, repo, issue_num, agent_name, summary, raw_path, created_at
		 FROM agent_sessions WHERE session_id = ?`,
		sessionID,
	).Scan(&sess.ID, &sess.SessionID, &sess.TaskID, &sess.Repo, &sess.IssueNum,
		&sess.AgentName, &sess.Summary, &sess.RawPath, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get agent session: %w", err)
	}
	sess.CreatedAt, _ = parseTimestamp(createdAt, "agent_session.created_at")
	return &sess, nil
}

// ---------------------------------------------------------------------------
// Issue Dependencies
// ---------------------------------------------------------------------------

func (s *Store) ReplaceIssueDependencies(repo string, issueNum int, deps []IssueDependency) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin replace issue dependencies: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`UPDATE issue_dependencies SET status = ? WHERE repo = ? AND issue_num = ? AND status != ?`,
		DependencyStatusRemoved, repo, issueNum, DependencyStatusRemoved,
	); err != nil {
		return fmt.Errorf("store: mark dependencies removed: %w", err)
	}

	for _, dep := range deps {
		if _, err := tx.Exec(
			`INSERT INTO issue_dependencies (repo, issue_num, depends_on_repo, depends_on_issue_num, source_hash, status)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT (repo, issue_num, depends_on_repo, depends_on_issue_num)
			 DO UPDATE SET source_hash = excluded.source_hash, status = excluded.status`,
			dep.Repo, dep.IssueNum, dep.DependsOnRepo, dep.DependsOnIssueNum, dep.SourceHash, dep.Status,
		); err != nil {
			return fmt.Errorf("store: upsert dependency: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit replace issue dependencies: %w", err)
	}
	return nil
}

func (s *Store) ListIssueDependencies(repo string, issueNum int) ([]IssueDependency, error) {
	rows, err := s.db.Query(
		`SELECT repo, issue_num, depends_on_repo, depends_on_issue_num, source_hash, status
		 FROM issue_dependencies
		 WHERE repo = ? AND issue_num = ?
		 ORDER BY depends_on_repo, depends_on_issue_num`,
		repo, issueNum,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list issue dependencies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []IssueDependency
	for rows.Next() {
		var dep IssueDependency
		if err := rows.Scan(&dep.Repo, &dep.IssueNum, &dep.DependsOnRepo, &dep.DependsOnIssueNum, &dep.SourceHash, &dep.Status); err != nil {
			return nil, fmt.Errorf("store: scan issue dependency: %w", err)
		}
		out = append(out, dep)
	}
	return out, rows.Err()
}

// UpsertIssueDependencyState writes the verdict-state row.
//
// Note: LastReactionBlocked is preserved across upserts via COALESCE so that
// EvaluateOpenIssues (which doesn't know whether the reaction was applied yet)
// cannot accidentally clear the reaction-state tracker. The Coordinator's
// reaction reconciler uses MarkDependencyReactionApplied to update it.
func (s *Store) UpsertIssueDependencyState(state IssueDependencyState) error {
	_, err := s.db.Exec(
		`INSERT INTO issue_dependency_state
		 (repo, issue_num, verdict, resume_label, blocked_reason_hash, override_active, graph_version, last_reaction_blocked, last_evaluated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT (repo, issue_num)
		 DO UPDATE SET
		   verdict = excluded.verdict,
		   resume_label = excluded.resume_label,
		   blocked_reason_hash = excluded.blocked_reason_hash,
		   override_active = excluded.override_active,
		   graph_version = excluded.graph_version,
		   last_evaluated_at = CURRENT_TIMESTAMP`,
		state.Repo, state.IssueNum, state.Verdict, state.ResumeLabel, state.BlockedReasonHash,
		boolToInt(state.OverrideActive), state.GraphVersion, boolToInt(state.LastReactionBlocked),
	)
	if err != nil {
		return fmt.Errorf("store: upsert issue dependency state: %w", err)
	}
	return nil
}

// MarkDependencyReactionApplied records the reaction state we just applied
// (or removed) on GitHub, so subsequent cycles can detect a flip cheaply.
func (s *Store) MarkDependencyReactionApplied(repo string, issueNum int, blocked bool) error {
	_, err := s.db.Exec(
		`UPDATE issue_dependency_state
		 SET last_reaction_blocked = ?
		 WHERE repo = ? AND issue_num = ?`,
		boolToInt(blocked), repo, issueNum,
	)
	if err != nil {
		return fmt.Errorf("store: mark dependency reaction applied: %w", err)
	}
	return nil
}

func (s *Store) QueryIssueDependencyState(repo string, issueNum int) (*IssueDependencyState, error) {
	var state IssueDependencyState
	var overrideActive, lastReactionBlocked int
	var evaluatedAt string
	err := s.db.QueryRow(
		`SELECT repo, issue_num, verdict, resume_label, blocked_reason_hash, override_active, graph_version, last_reaction_blocked, last_evaluated_at
		 FROM issue_dependency_state
		 WHERE repo = ? AND issue_num = ?`,
		repo, issueNum,
	).Scan(
		&state.Repo, &state.IssueNum, &state.Verdict, &state.ResumeLabel, &state.BlockedReasonHash, &overrideActive,
		&state.GraphVersion, &lastReactionBlocked, &evaluatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: query issue dependency state: %w", err)
	}
	state.OverrideActive = overrideActive != 0
	state.LastReactionBlocked = lastReactionBlocked != 0
	state.LastEvaluatedAt, _ = parseTimestamp(evaluatedAt, "issue_dependency_state.last_evaluated_at")
	return &state, nil
}

func (s *Store) HasActiveTask(repo string, issueNum int, agentName string) (bool, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(1)
		 FROM task_queue
		 WHERE repo = ? AND issue_num = ? AND agent_name = ? AND status IN (?, ?)`,
		repo, issueNum, agentName, TaskStatusPending, TaskStatusRunning,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("store: has active task: %w", err)
	}
	return count > 0, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
