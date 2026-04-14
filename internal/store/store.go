package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
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
		db.Close()
		return nil, fmt.Errorf("store: enable WAL: %w", err)
	}

	s := &Store{db: db}
	if err := s.createTables(); err != nil {
		db.Close()
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
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("exec %q: %w", stmt[:40], err)
		}
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
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var ev Event
		var ts string
		if err := rows.Scan(&ev.ID, &ts, &ev.Type, &ev.Repo, &ev.IssueNum, &ev.Payload); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		ev.TS, _ = time.Parse("2006-01-02 15:04:05", ts)
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
	defer rows.Close()

	var out []TaskRecord
	for rows.Next() {
		var t TaskRecord
		var createdAt, updatedAt string
		if err := rows.Scan(&t.ID, &t.Repo, &t.IssueNum, &t.AgentName, &t.WorkerID, &t.Status, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("store: scan task: %w", err)
		}
		t.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		t.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt)
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
		`INSERT INTO workers (id, repo, roles, hostname, status) VALUES (?, ?, ?, ?, ?)`,
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
	defer rows.Close()

	var out []WorkerRecord
	for rows.Next() {
		var w WorkerRecord
		var hb, ra string
		if err := rows.Scan(&w.ID, &w.Repo, &w.Roles, &w.Hostname, &w.Status, &hb, &ra); err != nil {
			return nil, fmt.Errorf("store: scan worker: %w", err)
		}
		w.LastHeartbeat, _ = time.Parse("2006-01-02 15:04:05", hb)
		w.RegisteredAt, _ = time.Parse("2006-01-02 15:04:05", ra)
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
func (s *Store) IncrementTransition(repo string, issueNum int, fromState, toState string) (int, error) {
	_, err := s.db.Exec(
		`INSERT INTO transition_counts (repo, issue_num, from_state, to_state, count)
		 VALUES (?, ?, ?, ?, 1)
		 ON CONFLICT (repo, issue_num, from_state, to_state)
		 DO UPDATE SET count = count + 1`,
		repo, issueNum, fromState, toState,
	)
	if err != nil {
		return 0, fmt.Errorf("store: increment transition: %w", err)
	}

	var count int
	err = s.db.QueryRow(
		`SELECT count FROM transition_counts WHERE repo = ? AND issue_num = ? AND from_state = ? AND to_state = ?`,
		repo, issueNum, fromState, toState,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: read transition count: %w", err)
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
	defer rows.Close()

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
		`INSERT INTO issue_cache (repo, issue_num, labels, state, updated_at)
		 VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT (repo, issue_num)
		 DO UPDATE SET labels = excluded.labels, state = excluded.state, updated_at = CURRENT_TIMESTAMP`,
		ic.Repo, ic.IssueNum, ic.Labels, ic.State,
	)
	if err != nil {
		return fmt.Errorf("store: upsert issue cache: %w", err)
	}
	return nil
}

// QueryIssueCache returns the cached issue, or nil if not found.
func (s *Store) QueryIssueCache(repo string, issueNum int) (*IssueCache, error) {
	var ic IssueCache
	var updatedAt string
	err := s.db.QueryRow(
		`SELECT repo, issue_num, labels, state, updated_at FROM issue_cache WHERE repo = ? AND issue_num = ?`,
		repo, issueNum,
	).Scan(&ic.Repo, &ic.IssueNum, &ic.Labels, &ic.State, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: query issue cache: %w", err)
	}
	ic.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt)
	return &ic, nil
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
	defer rows.Close()

	var out []AgentSession
	for rows.Next() {
		var sess AgentSession
		var createdAt string
		if err := rows.Scan(&sess.ID, &sess.SessionID, &sess.TaskID, &sess.Repo, &sess.IssueNum,
			&sess.AgentName, &sess.Summary, &sess.RawPath, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan agent session: %w", err)
		}
		sess.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		out = append(out, sess)
	}
	return out, rows.Err()
}
