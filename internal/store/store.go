// Package store provides SQLite-backed persistence for workbuddy state.
package store

import (
	"database/sql"
	"errors"
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

var (
	ErrTaskNotFound           = errors.New("task not found")
	ErrTaskClaimConflict      = errors.New("task claim conflict")
	ErrTaskAlreadyCompleted   = errors.New("task already completed")
	ErrTaskNotClaimedByWorker = errors.New("task not claimed by worker")
)

// Store provides typed CRUD access to the workbuddy SQLite database.
type Store struct {
	db *sql.DB
}

type TaskFilter struct {
	Repo   string
	Status string
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
			role TEXT NOT NULL DEFAULT '',
			runtime TEXT NOT NULL DEFAULT '',
			workflow TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL DEFAULT '',
			worker_id TEXT,
			claim_token TEXT,
			status TEXT NOT NULL DEFAULT 'pending',
			lease_expires_at DATETIME,
			acked_at DATETIME,
			heartbeat_at DATETIME,
			completed_at DATETIME,
			exit_code INTEGER NOT NULL DEFAULT 0,
			session_refs TEXT NOT NULL DEFAULT '[]',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS workers (
			id TEXT PRIMARY KEY,
			repo TEXT NOT NULL,
			roles TEXT NOT NULL,
			hostname TEXT,
			status TEXT NOT NULL DEFAULT 'online',
			token_kid TEXT,
			token_hash TEXT,
			token_revoked_at DATETIME,
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
		`CREATE TABLE IF NOT EXISTS sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL UNIQUE,
			task_id TEXT,
			repo TEXT NOT NULL,
			issue_num INTEGER NOT NULL,
			agent_name TEXT NOT NULL,
			runtime TEXT NOT NULL DEFAULT '',
			worker_id TEXT NOT NULL DEFAULT '',
			attempt INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'pending',
			dir TEXT NOT NULL DEFAULT '',
			stdout_path TEXT NOT NULL DEFAULT '',
			stderr_path TEXT NOT NULL DEFAULT '',
			tool_calls_path TEXT NOT NULL DEFAULT '',
			metadata_path TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			raw_path TEXT NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			closed_at DATETIME
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_repo_issue ON sessions(repo, issue_num, id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_agent ON sessions(agent_name, id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_worker_status ON sessions(worker_id, status, id)`,
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
	if _, err := s.db.Exec(`ALTER TABLE workers ADD COLUMN token_kid TEXT`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("store: alter workers add token_kid: %w", err)
	}
	if _, err := s.db.Exec(`ALTER TABLE workers ADD COLUMN token_hash TEXT`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("store: alter workers add token_hash: %w", err)
	}
	if _, err := s.db.Exec(`ALTER TABLE workers ADD COLUMN token_revoked_at DATETIME`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("store: alter workers add token_revoked_at: %w", err)
	}
	taskQueueMigrations := []string{
		`ALTER TABLE task_queue ADD COLUMN role TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE task_queue ADD COLUMN runtime TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE task_queue ADD COLUMN workflow TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE task_queue ADD COLUMN state TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE task_queue ADD COLUMN claim_token TEXT`,
		`ALTER TABLE task_queue ADD COLUMN lease_expires_at DATETIME`,
		`ALTER TABLE task_queue ADD COLUMN acked_at DATETIME`,
		`ALTER TABLE task_queue ADD COLUMN heartbeat_at DATETIME`,
		`ALTER TABLE task_queue ADD COLUMN completed_at DATETIME`,
		`ALTER TABLE task_queue ADD COLUMN exit_code INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE task_queue ADD COLUMN session_refs TEXT NOT NULL DEFAULT '[]'`,
	}
	for _, stmt := range taskQueueMigrations {
		if _, err := s.db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("store: migrate task_queue: %w", err)
		}
	}
	// Forward-migrate any pre-existing issue_dependency_state rows: drop the
	// old managed-comment anchor columns by adding the new reaction column if
	// the table was created by an earlier schema. SQLite has no DROP COLUMN
	// in older versions, but the unused columns are harmless to leave; we
	// just need last_reaction_blocked to exist.
	if _, err := s.db.Exec(`ALTER TABLE issue_dependency_state ADD COLUMN last_reaction_blocked INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("store: alter issue_dependency_state add last_reaction_blocked: %w", err)
	}
	if err := s.migrateLegacySessions(); err != nil {
		return err
	}
	return nil
}

func (s *Store) migrateLegacySessions() error {
	_, err := s.db.Exec(`
		INSERT OR IGNORE INTO sessions (
			session_id, task_id, repo, issue_num, agent_name,
			status, summary, raw_path, created_at
		)
		SELECT
			a.session_id,
			a.task_id,
			a.repo,
			a.issue_num,
			a.agent_name,
			COALESCE(t.status, 'completed'),
			COALESCE(a.summary, ''),
			COALESCE(a.raw_path, ''),
			a.created_at
		FROM agent_sessions a
		LEFT JOIN task_queue t ON t.id = a.task_id
	`)
	if err != nil {
		return fmt.Errorf("store: migrate legacy sessions: %w", err)
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

func taskLeaseOffset(lease time.Duration) string {
	seconds := int(lease.Seconds())
	if seconds <= 0 {
		seconds = 1
	}
	return fmt.Sprintf("+%d seconds", seconds)
}

func scanTaskRecord(scan func(dest ...any) error) (TaskRecord, error) {
	var t TaskRecord
	var createdAt, updatedAt string
	var leaseExpiresAt, ackedAt, heartbeatAt, completedAt sql.NullString
	var workerID, claimToken, labels sql.NullString
	if err := scan(
		&t.ID,
		&t.Repo,
		&t.IssueNum,
		&t.AgentName,
		&labels,
		&t.Role,
		&t.Runtime,
		&t.Workflow,
		&t.State,
		&workerID,
		&claimToken,
		&t.Status,
		&leaseExpiresAt,
		&ackedAt,
		&heartbeatAt,
		&completedAt,
		&t.ExitCode,
		&t.SessionRefs,
		&createdAt,
		&updatedAt,
	); err != nil {
		return TaskRecord{}, err
	}
	if workerID.Valid {
		t.WorkerID = workerID.String
	}
	if claimToken.Valid {
		t.ClaimToken = claimToken.String
	}
	if labels.Valid {
		t.Labels = labels.String
	}
	t.CreatedAt, _ = parseTimestamp(createdAt, "task.created_at")
	t.UpdatedAt, _ = parseTimestamp(updatedAt, "task.updated_at")
	if leaseExpiresAt.Valid {
		t.LeaseExpiresAt, _ = parseTimestamp(leaseExpiresAt.String, "task.lease_expires_at")
	}
	if ackedAt.Valid {
		t.AckedAt, _ = parseTimestamp(ackedAt.String, "task.acked_at")
	}
	if heartbeatAt.Valid {
		t.HeartbeatAt, _ = parseTimestamp(heartbeatAt.String, "task.heartbeat_at")
	}
	if completedAt.Valid {
		t.CompletedAt, _ = parseTimestamp(completedAt.String, "task.completed_at")
	}
	return t, nil
}

// InsertTask inserts a new task into the task_queue.
func (s *Store) InsertTask(t TaskRecord) error {
	if t.Status == "" {
		t.Status = TaskStatusPending
	}
	if t.SessionRefs == "" {
		t.SessionRefs = "[]"
	}
	_, err := s.db.Exec(
		`INSERT INTO task_queue (
			id, repo, issue_num, agent_name, role, runtime, workflow, state,
			worker_id, claim_token, status, exit_code, session_refs
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Repo, t.IssueNum, t.AgentName, t.Role, t.Runtime, t.Workflow, t.State,
		t.WorkerID, t.ClaimToken, t.Status, t.ExitCode, t.SessionRefs,
	)
	if err != nil {
		return fmt.Errorf("store: insert task: %w", err)
	}
	return nil
}

// QueryTasks returns tasks filtered by status (empty string = all).
func (s *Store) QueryTasks(status string) ([]TaskRecord, error) {
	return s.QueryTasksFiltered(TaskFilter{Status: status})
}

// QueryTasksFiltered returns tasks matching the given filter.
func (s *Store) QueryTasksFiltered(filter TaskFilter) ([]TaskRecord, error) {
	const selectTasks = `SELECT
		tq.id, tq.repo, tq.issue_num, tq.agent_name, ic.labels, tq.role, tq.runtime, tq.workflow, tq.state,
		tq.worker_id, tq.claim_token, tq.status, tq.lease_expires_at, tq.acked_at, tq.heartbeat_at,
		tq.completed_at, tq.exit_code, tq.session_refs, tq.created_at, tq.updated_at
		FROM task_queue tq
		LEFT JOIN issue_cache ic
		  ON ic.repo = tq.repo AND ic.issue_num = tq.issue_num`
	query := selectTasks + ` WHERE 1=1`
	args := make([]any, 0, 2)
	if filter.Repo != "" {
		query += ` AND tq.repo = ?`
		args = append(args, filter.Repo)
	}
	if filter.Status != "" {
		query += ` AND tq.status = ?`
		args = append(args, filter.Status)
	}
	query += ` ORDER BY tq.created_at`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TaskRecord
	for rows.Next() {
		t, err := scanTaskRecord(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: scan task: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTask loads a task by ID.
func (s *Store) GetTask(taskID string) (*TaskRecord, error) {
	row := s.db.QueryRow(`SELECT
		tq.id, tq.repo, tq.issue_num, tq.agent_name, ic.labels, tq.role, tq.runtime, tq.workflow, tq.state,
		tq.worker_id, tq.claim_token, tq.status, tq.lease_expires_at, tq.acked_at, tq.heartbeat_at,
		tq.completed_at, tq.exit_code, tq.session_refs, tq.created_at, tq.updated_at
		FROM task_queue tq
		LEFT JOIN issue_cache ic
		  ON ic.repo = tq.repo AND ic.issue_num = tq.issue_num
		WHERE tq.id = ?`, taskID)
	t, err := scanTaskRecord(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrTaskNotFound
		}
		return nil, fmt.Errorf("store: get task: %w", err)
	}
	return &t, nil
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

// ClaimTask atomically assigns a pending task to a worker and marks it running.
// It returns true when the claim succeeded, or false when the task was no longer pending.
func (s *Store) ClaimTask(taskID, workerID string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE task_queue
		 SET worker_id = ?, status = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND status = ?`,
		workerID, TaskStatusRunning, taskID, TaskStatusPending,
	)
	if err != nil {
		return false, fmt.Errorf("store: claim task: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: claim task rows affected: %w", err)
	}
	return rows > 0, nil
}

// ReleaseTask atomically requeues a running task owned by the given worker.
// It returns true when the task was released back to pending.
func (s *Store) ReleaseTask(taskID, workerID string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE task_queue
		 SET worker_id = NULL, status = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND worker_id = ? AND status = ?`,
		TaskStatusPending, taskID, workerID, TaskStatusRunning,
	)
	if err != nil {
		return false, fmt.Errorf("store: release task: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: release task rows affected: %w", err)
	}
	return rows > 0, nil
}

// ClaimNextTask assigns the next dispatchable task to workerID. If the same
// worker repeats the request with the same non-empty claimToken before the
// lease expires, the previously claimed task is returned.
func (s *Store) ClaimNextTask(workerID string, roles []string, claimToken string, lease time.Duration) (*TaskRecord, error) {
	if lease <= 0 {
		lease = 30 * time.Second
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: begin claim tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if claimToken != "" {
		row := tx.QueryRow(`SELECT
			id, repo, issue_num, agent_name, NULL, role, runtime, workflow, state,
			worker_id, claim_token, status, lease_expires_at, acked_at, heartbeat_at,
			completed_at, exit_code, session_refs, created_at, updated_at
			FROM task_queue
			WHERE worker_id = ? AND claim_token = ? AND status = ?
			  AND lease_expires_at IS NOT NULL AND lease_expires_at >= CURRENT_TIMESTAMP
			ORDER BY updated_at DESC LIMIT 1`, workerID, claimToken, TaskStatusRunning)
		existing, err := scanTaskRecord(row.Scan)
		switch {
		case err == nil:
			if err := tx.Commit(); err != nil {
				return nil, fmt.Errorf("store: commit idempotent claim: %w", err)
			}
			return &existing, nil
		case !errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("store: idempotent claim lookup: %w", err)
		}
	}

	conds := make([]string, 0, len(roles))
	roleArgs := make([]any, 0, len(roles))
	for _, role := range roles {
		conds = append(conds, "role = ?")
		roleArgs = append(roleArgs, role)
	}
	query := `SELECT
		id, repo, issue_num, agent_name, NULL, role, runtime, workflow, state,
		worker_id, claim_token, status, lease_expires_at, acked_at, heartbeat_at,
		completed_at, exit_code, session_refs, created_at, updated_at
		FROM task_queue
		WHERE status IN (?, ?)
		  AND (lease_expires_at IS NULL OR lease_expires_at < CURRENT_TIMESTAMP)`
	args := []any{TaskStatusPending, TaskStatusRunning}
	if len(conds) > 0 {
		query += ` AND (` + strings.Join(conds, " OR ") + `)`
		args = append(args, roleArgs...)
	}
	query += ` ORDER BY created_at, id LIMIT 1`

	row := tx.QueryRow(query, args...)
	task, err := scanTaskRecord(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if err := tx.Commit(); err != nil {
				return nil, fmt.Errorf("store: commit empty claim: %w", err)
			}
			return nil, nil
		}
		return nil, fmt.Errorf("store: select task for claim: %w", err)
	}

	res, err := tx.Exec(
		`UPDATE task_queue
		 SET worker_id = ?, claim_token = ?, status = ?, lease_expires_at = datetime('now', ?),
		     heartbeat_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?
		   AND status IN (?, ?)
		   AND (lease_expires_at IS NULL OR lease_expires_at < CURRENT_TIMESTAMP)`,
		workerID, claimToken, TaskStatusRunning, taskLeaseOffset(lease), task.ID,
		TaskStatusPending, TaskStatusRunning,
	)
	if err != nil {
		return nil, fmt.Errorf("store: claim task update: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return nil, ErrTaskClaimConflict
	}

	task.WorkerID = workerID
	task.ClaimToken = claimToken
	task.Status = TaskStatusRunning
	task.HeartbeatAt = time.Now().UTC()
	task.LeaseExpiresAt = task.HeartbeatAt.Add(lease)

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit claim: %w", err)
	}
	return &task, nil
}

func (s *Store) ensureTaskOwnership(tx *sql.Tx, taskID, workerID string) (*TaskRecord, error) {
	row := tx.QueryRow(`SELECT
		id, repo, issue_num, agent_name, NULL, role, runtime, workflow, state,
		worker_id, claim_token, status, lease_expires_at, acked_at, heartbeat_at,
		completed_at, exit_code, session_refs, created_at, updated_at
		FROM task_queue WHERE id = ?`, taskID)
	task, err := scanTaskRecord(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrTaskNotFound
		}
		return nil, fmt.Errorf("store: load task %s: %w", taskID, err)
	}
	if task.Status == TaskStatusCompleted || task.Status == TaskStatusFailed || task.Status == TaskStatusTimeout {
		return nil, ErrTaskAlreadyCompleted
	}
	if task.WorkerID != workerID {
		return nil, ErrTaskNotClaimedByWorker
	}
	return &task, nil
}

// AckTask records that a claimed task has started.
func (s *Store) AckTask(taskID, workerID string, lease time.Duration) error {
	if lease <= 0 {
		lease = 30 * time.Second
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin ack tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := s.ensureTaskOwnership(tx, taskID, workerID); err != nil {
		return err
	}
	res, err := tx.Exec(
		`UPDATE task_queue
		 SET acked_at = COALESCE(acked_at, CURRENT_TIMESTAMP),
		     heartbeat_at = CURRENT_TIMESTAMP,
		     lease_expires_at = datetime('now', ?),
		     updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND worker_id = ?`,
		taskLeaseOffset(lease), taskID, workerID,
	)
	if err != nil {
		return fmt.Errorf("store: ack task: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTaskNotClaimedByWorker
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit ack: %w", err)
	}
	return nil
}

// HeartbeatTask updates liveness for a claimed task.
func (s *Store) HeartbeatTask(taskID, workerID string, lease time.Duration) error {
	if lease <= 0 {
		lease = 30 * time.Second
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin heartbeat tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := s.ensureTaskOwnership(tx, taskID, workerID); err != nil {
		return err
	}
	res, err := tx.Exec(
		`UPDATE task_queue
		 SET heartbeat_at = CURRENT_TIMESTAMP,
		     lease_expires_at = datetime('now', ?),
		     updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND worker_id = ?
		   AND lease_expires_at IS NOT NULL
		   AND lease_expires_at > CURRENT_TIMESTAMP`,
		taskLeaseOffset(lease), taskID, workerID,
	)
	if err != nil {
		return fmt.Errorf("store: heartbeat task: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: heartbeat task: task %s is no longer owned by worker %s or lease has expired", taskID, workerID)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit heartbeat: %w", err)
	}
	return nil
}

// CompleteTask finalizes a claimed task and stores result metadata.
func (s *Store) CompleteTask(taskID, workerID string, exitCode int, sessionRefs string) error {
	if sessionRefs == "" {
		sessionRefs = "[]"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin complete tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := s.ensureTaskOwnership(tx, taskID, workerID); err != nil {
		return err
	}
	status := TaskStatusCompleted
	if exitCode != 0 {
		status = TaskStatusFailed
	}
	res, err := tx.Exec(
		`UPDATE task_queue
		 SET status = ?, exit_code = ?, session_refs = ?, completed_at = CURRENT_TIMESTAMP,
		     heartbeat_at = CURRENT_TIMESTAMP, lease_expires_at = NULL, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND worker_id = ?
		   AND status NOT IN (?, ?)
		   AND lease_expires_at IS NOT NULL
		   AND lease_expires_at > CURRENT_TIMESTAMP`,
		status, exitCode, sessionRefs, taskID, workerID, TaskStatusCompleted, TaskStatusFailed,
	)
	if err != nil {
		return fmt.Errorf("store: complete task: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("store: complete task: task is no longer owned by worker or lease expired")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit complete: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Workers
// ---------------------------------------------------------------------------

// InsertWorker registers a worker.
func (s *Store) InsertWorker(w WorkerRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO workers (id, repo, roles, hostname, status)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		 repo = excluded.repo,
		 roles = excluded.roles,
		 hostname = excluded.hostname,
		 status = excluded.status`,
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
// Sessions
// ---------------------------------------------------------------------------

type SessionFilter struct {
	Repo      string
	IssueNum  int
	AgentName string
	WorkerID  string
	Status    string
}

func (s *Store) CreateSession(sess SessionRecord) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO sessions (
			session_id, task_id, repo, issue_num, agent_name, runtime, worker_id, attempt, status,
			dir, stdout_path, stderr_path, tool_calls_path, metadata_path, summary, raw_path
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.SessionID, sess.TaskID, sess.Repo, sess.IssueNum, sess.AgentName, sess.Runtime, sess.WorkerID, sess.Attempt, sess.Status,
		sess.Dir, sess.StdoutPath, sess.StderrPath, sess.ToolCallsPath, sess.MetadataPath, sess.Summary, sess.RawPath,
	)
	if err != nil {
		return 0, fmt.Errorf("store: create session: %w", err)
	}
	return res.LastInsertId()
}

func (s *Store) UpdateSession(record SessionRecord) error {
	res, err := s.db.Exec(
		`UPDATE sessions
		 SET task_id = ?, runtime = ?, worker_id = ?, attempt = ?, status = ?, dir = ?,
		     stdout_path = ?, stderr_path = ?, tool_calls_path = ?, metadata_path = ?,
		     summary = ?, raw_path = ?, closed_at = ?
		 WHERE session_id = ?`,
		record.TaskID, record.Runtime, record.WorkerID, record.Attempt, record.Status, record.Dir,
		record.StdoutPath, record.StderrPath, record.ToolCallsPath, record.MetadataPath,
		record.Summary, record.RawPath, nullableTime(record.ClosedAt), record.SessionID,
	)
	if err != nil {
		return fmt.Errorf("store: update session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: update session: session %q not found", record.SessionID)
	}
	return nil
}

func (s *Store) GetSession(sessionID string) (*SessionRecord, error) {
	row := s.db.QueryRow(
		`SELECT s.id, s.session_id, s.task_id, s.repo, s.issue_num, s.agent_name, s.runtime, s.worker_id, s.attempt,
		        COALESCE(t.status, s.status),
		        s.dir, s.stdout_path, s.stderr_path, s.tool_calls_path, s.metadata_path, s.summary, s.raw_path, s.created_at, s.closed_at
		 FROM sessions s
		 LEFT JOIN task_queue t ON t.id = s.task_id
		 WHERE s.session_id = ?`,
		sessionID,
	)
	record, err := scanSessionRow(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get session: %w", err)
	}
	return record, nil
}

func (s *Store) ListSessions(f SessionFilter) ([]SessionRecord, error) {
	q := `SELECT s.id, s.session_id, s.task_id, s.repo, s.issue_num, s.agent_name, s.runtime, s.worker_id, s.attempt,
	             COALESCE(t.status, s.status),
	             s.dir, s.stdout_path, s.stderr_path, s.tool_calls_path, s.metadata_path, s.summary, s.raw_path, s.created_at, s.closed_at
	      FROM sessions s
	      LEFT JOIN task_queue t ON t.id = s.task_id
	      WHERE 1=1`
	var args []any
	if f.Repo != "" {
		q += " AND s.repo = ?"
		args = append(args, f.Repo)
	}
	if f.IssueNum != 0 {
		q += " AND s.issue_num = ?"
		args = append(args, f.IssueNum)
	}
	if f.AgentName != "" {
		q += " AND s.agent_name = ?"
		args = append(args, f.AgentName)
	}
	if f.WorkerID != "" {
		q += " AND s.worker_id = ?"
		args = append(args, f.WorkerID)
	}
	if f.Status != "" {
		q += " AND COALESCE(t.status, s.status) = ?"
		args = append(args, f.Status)
	}
	q += " ORDER BY s.id DESC"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SessionRecord
	for rows.Next() {
		record, err := scanSessionRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: scan session: %w", err)
		}
		out = append(out, *record)
	}
	return out, rows.Err()
}

func scanSessionRow(scan func(dest ...any) error) (*SessionRecord, error) {
	var record SessionRecord
	var createdAt string
	var closedAt sql.NullString
	if err := scan(
		&record.ID, &record.SessionID, &record.TaskID, &record.Repo, &record.IssueNum, &record.AgentName,
		&record.Runtime, &record.WorkerID, &record.Attempt, &record.Status, &record.Dir, &record.StdoutPath,
		&record.StderrPath, &record.ToolCallsPath, &record.MetadataPath, &record.Summary, &record.RawPath,
		&createdAt, &closedAt,
	); err != nil {
		return nil, err
	}
	record.CreatedAt, _ = parseTimestamp(createdAt, "session.created_at")
	if closedAt.Valid {
		record.ClosedAt, _ = parseTimestamp(closedAt.String, "session.closed_at")
	}
	return &record, nil
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

// AgentSession is the compatibility projection used by the audit/web UI layer.
type AgentSession struct {
	ID         int64
	SessionID  string
	TaskID     string
	Repo       string
	IssueNum   int
	AgentName  string
	Summary    string
	RawPath    string
	TaskStatus string
	CreatedAt  time.Time
}

func sessionRecordToAgent(record SessionRecord) AgentSession {
	return AgentSession{
		ID:         record.ID,
		SessionID:  record.SessionID,
		TaskID:     record.TaskID,
		Repo:       record.Repo,
		IssueNum:   record.IssueNum,
		AgentName:  record.AgentName,
		Summary:    record.Summary,
		RawPath:    record.RawPath,
		TaskStatus: record.Status,
		CreatedAt:  record.CreatedAt,
	}
}

// InsertAgentSession records an agent session via the additive sessions table.
func (s *Store) InsertAgentSession(sess AgentSession) (int64, error) {
	return s.CreateSession(SessionRecord{
		SessionID: sess.SessionID,
		TaskID:    sess.TaskID,
		Repo:      sess.Repo,
		IssueNum:  sess.IssueNum,
		AgentName: sess.AgentName,
		Status:    firstNonEmpty(sess.TaskStatus, TaskStatusPending),
		Summary:   sess.Summary,
		RawPath:   sess.RawPath,
	})
}

// QueryAgentSessions returns sessions for a repo+issue.
func (s *Store) QueryAgentSessions(repo string, issueNum int) ([]AgentSession, error) {
	records, err := s.ListSessions(SessionFilter{Repo: repo, IssueNum: issueNum})
	if err != nil {
		return nil, err
	}
	out := make([]AgentSession, 0, len(records))
	for _, record := range records {
		out = append(out, sessionRecordToAgent(record))
	}
	return out, nil
}

// ListAgentSessions returns sessions matching the given filter.
func (s *Store) ListAgentSessions(f SessionFilter) ([]AgentSession, error) {
	records, err := s.ListSessions(f)
	if err != nil {
		return nil, err
	}
	out := make([]AgentSession, 0, len(records))
	for _, record := range records {
		out = append(out, sessionRecordToAgent(record))
	}
	return out, nil
}

// UpdateAgentSession updates summary and raw_path for an existing session.
func (s *Store) UpdateAgentSession(sessionID, summary, rawPath string) error {
	record, err := s.GetSession(sessionID)
	if err != nil {
		return err
	}
	if record == nil {
		return fmt.Errorf("store: update agent session: session %q not found", sessionID)
	}
	record.Summary = summary
	record.RawPath = rawPath
	return s.UpdateSession(*record)
}

// GetAgentSession returns a single session by session_id, or nil if not found.
func (s *Store) GetAgentSession(sessionID string) (*AgentSession, error) {
	record, err := s.GetSession(sessionID)
	if err != nil || record == nil {
		return nil, err
	}
	sess := sessionRecordToAgent(*record)
	return &sess, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
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

// DeleteIssueDependencyState removes the dependency verdict cache for one issue.
func (s *Store) DeleteIssueDependencyState(repo string, issueNum int) (bool, error) {
	res, err := s.db.Exec(
		`DELETE FROM issue_dependency_state WHERE repo = ? AND issue_num = ?`,
		repo, issueNum,
	)
	if err != nil {
		return false, fmt.Errorf("store: delete issue dependency state: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: delete issue dependency state rows affected: %w", err)
	}
	return rows > 0, nil
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

func (s *Store) HasAnyActiveTask(repo string, issueNum int) (bool, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(1)
		 FROM task_queue
		 WHERE repo = ? AND issue_num = ? AND status IN (?, ?)`,
		repo, issueNum, TaskStatusPending, TaskStatusRunning,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("store: has any active task: %w", err)
	}
	return count > 0, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
