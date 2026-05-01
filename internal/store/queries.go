package store

// This file contains narrow, purpose-built query methods used by higher-level
// packages (eventlog, metrics, auditapi, audit, workflow) so those consumers
// no longer need to reach through Store.DB() and issue raw SQL against storage
// internals. See issue #145 finding #9.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Event query (eventlog)
// ---------------------------------------------------------------------------

// EventQueryFilter specifies optional criteria for QueryEventsFiltered.
// All fields are optional; zero values are ignored.
type EventQueryFilter struct {
	Since    *time.Time
	Until    *time.Time
	Type     string
	Repo     string
	IssueNum int
}

// IssueEventMeta is the compact latest-event view used by audit/status
// surfaces when they only need the last event's timestamp and type.
type IssueEventMeta struct {
	Type string
	TS   time.Time
}

// QueryEventsFiltered returns events matching the provided filter. It exists
// so eventlog.EventLogger can run filtered queries without needing a raw
// *sql.DB handle.
func (s *Store) QueryEventsFiltered(filter EventQueryFilter) ([]Event, error) {
	query := `SELECT id, ts, type, repo, issue_num, payload FROM events WHERE 1=1`
	var args []interface{}

	if filter.Repo != "" {
		query += ` AND repo = ?`
		args = append(args, filter.Repo)
	}
	if filter.Type != "" {
		query += ` AND type = ?`
		args = append(args, filter.Type)
	}
	if filter.IssueNum != 0 {
		query += ` AND issue_num = ?`
		args = append(args, filter.IssueNum)
	}
	if filter.Since != nil {
		query += ` AND ts >= ?`
		args = append(args, filter.Since.UTC().Format("2006-01-02 15:04:05"))
	}
	if filter.Until != nil {
		query += ` AND ts <= ?`
		args = append(args, filter.Until.UTC().Format("2006-01-02 15:04:05"))
	}

	query += ` ORDER BY id`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query events filtered: %w", err)
	}
	defer func() { _ = rows.Close() }()

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

// LatestEventAt returns the timestamp of the most recent event for the given
// repo/issue pair, or nil if there are no events. Used by audit handlers to
// compute "last activity" surfaces.
func (s *Store) LatestEventAt(repo string, issueNum int) (*time.Time, error) {
	var raw string
	err := s.db.QueryRow(
		`SELECT ts FROM events WHERE repo = ? AND issue_num = ? ORDER BY ts DESC, id DESC LIMIT 1`,
		repo, issueNum,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: latest event at: %w", err)
	}
	ts, ok := ParseTimestamp(raw, "event.ts")
	if !ok {
		return nil, nil
	}
	ts = ts.UTC()
	return &ts, nil
}

// LatestIssueEvent returns the type and timestamp of the most recent event for
// the given repo/issue pair, or nil if there are no events.
func (s *Store) LatestIssueEvent(repo string, issueNum int) (*IssueEventMeta, error) {
	var rawTS, eventType string
	err := s.db.QueryRow(
		`SELECT ts, type FROM events WHERE repo = ? AND issue_num = ? ORDER BY ts DESC, id DESC LIMIT 1`,
		repo, issueNum,
	).Scan(&rawTS, &eventType)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: latest issue event: %w", err)
	}
	ts, ok := ParseTimestamp(rawTS, "event.ts")
	if !ok {
		return nil, nil
	}
	return &IssueEventMeta{Type: eventType, TS: ts.UTC()}, nil
}

// IssueTransitionEvent captures the {from,to,at,by} fields stored on a
// `transition` event payload. Used by the dashboard issue-detail endpoint.
type IssueTransitionEvent struct {
	From string
	To   string
	At   time.Time
	By   string
}

// LatestIssueTransition returns the most recent `transition` event for the
// given issue, or nil if none exists. Used for last_transition_at on the
// in-flight dashboard.
func (s *Store) LatestIssueTransition(repo string, issueNum int) (*IssueTransitionEvent, error) {
	var rawTS, payload string
	err := s.db.QueryRow(
		`SELECT ts, payload FROM events
		 WHERE repo = ? AND issue_num = ? AND type = 'transition'
		 ORDER BY ts DESC, id DESC LIMIT 1`,
		repo, issueNum,
	).Scan(&rawTS, &payload)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: latest issue transition: %w", err)
	}
	ts, ok := ParseTimestamp(rawTS, "event.ts")
	if !ok {
		return nil, nil
	}
	out := &IssueTransitionEvent{At: ts.UTC()}
	parseTransitionPayload(payload, out)
	return out, nil
}

// QueryIssueTransitions returns every `transition` event for the issue in
// chronological order. Used by the issue-detail endpoint.
func (s *Store) QueryIssueTransitions(repo string, issueNum int) ([]IssueTransitionEvent, error) {
	rows, err := s.db.Query(
		`SELECT ts, payload FROM events
		 WHERE repo = ? AND issue_num = ? AND type = 'transition'
		 ORDER BY ts ASC, id ASC`,
		repo, issueNum,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query issue transitions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []IssueTransitionEvent
	for rows.Next() {
		var rawTS, payload string
		if err := rows.Scan(&rawTS, &payload); err != nil {
			return nil, fmt.Errorf("store: scan issue transition: %w", err)
		}
		ts, ok := ParseTimestamp(rawTS, "event.ts")
		if !ok {
			continue
		}
		ev := IssueTransitionEvent{At: ts.UTC()}
		parseTransitionPayload(payload, &ev)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// LatestSessionForIssue returns the newest session row for the (repo,issue)
// pair, or nil when no session has been created. Used to populate
// last_session_id / last_session_url on the in-flight dashboard.
func (s *Store) LatestSessionForIssue(repo string, issueNum int) (*SessionRecord, error) {
	row := s.db.QueryRow(
		`SELECT s.id, s.session_id, s.task_id, s.repo, s.issue_num, s.agent_name, s.runtime, s.worker_id, s.attempt,
		        COALESCE(t.status, s.status),
		        s.dir, s.stdout_path, s.stderr_path, s.tool_calls_path, s.metadata_path, s.summary, s.raw_path, s.created_at, s.closed_at
		 FROM sessions s
		 LEFT JOIN task_queue t ON t.id = s.task_id
		 WHERE s.repo = ? AND s.issue_num = ?
		 ORDER BY s.id DESC LIMIT 1`,
		repo, issueNum,
	)
	record, err := scanSessionRecordForAPI(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: latest session for issue: %w", err)
	}
	return record, nil
}

// CountTerminalSessionsSince counts sessions that have transitioned into the
// given terminal status since the cutoff time. Used for done_24h / failed_24h
// summary fields on /api/v1/status.
func (s *Store) CountTerminalSessionsSince(status string, since time.Time) (int, error) {
	if s.coordinatorMode {
		// Phase 3 (REQ-122): coordinator's `sessions` table is dropped.
		// Status counters now report 0 from the coordinator side; the
		// per-worker audit endpoints carry the truth and a future
		// metrics rollup may aggregate them. Returning 0 keeps the
		// /api/v1/status response shape valid.
		return 0, nil
	}
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sessions s
		 LEFT JOIN task_queue t ON t.id = s.task_id
		 WHERE COALESCE(t.status, s.status) = ?
		   AND s.closed_at IS NOT NULL
		   AND s.closed_at >= ?`,
		status, since.UTC().Format("2006-01-02 15:04:05"),
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count terminal sessions: %w", err)
	}
	return n, nil
}

// InFlightTaskForWorker is a compact view of a running task that a stateless
// worker needs in order to resume tracking of its supervisor-managed
// subprocess after a worker restart. EventsV1Path is the on-disk events file
// the resume goroutine appends to; SupervisorAgentID identifies the agent
// inside the local supervisor's IPC service. SessionID is convenience for
// logging — it lets the resumer name what it's resuming without a separate
// query.
type InFlightTaskForWorker struct {
	TaskID            string
	SessionID         string
	SupervisorAgentID string
	EventsV1Path      string
}

// InFlightTasksForWorker returns every running task currently claimed by
// workerID together with the latest session row so the worker can rebuild
// its event-stream tail on boot. Tasks without a recorded
// supervisor_agent_id (the foundational #234 slice landed before all
// dispatch paths were wired) are omitted: the worker resume path needs the
// agent id to call the supervisor IPC. Issue #245.
func (s *Store) InFlightTasksForWorker(workerID string) ([]InFlightTaskForWorker, error) {
	rows, err := s.db.Query(
		`SELECT tq.id,
		        COALESCE(tq.supervisor_agent_id, ''),
		        COALESCE(s.session_id, ''),
		        COALESCE(s.dir, '')
		 FROM task_queue tq
		 LEFT JOIN sessions s
		        ON s.id = (
		           SELECT s2.id FROM sessions s2
		           WHERE s2.task_id = tq.id
		           ORDER BY s2.id DESC LIMIT 1)
		 WHERE tq.status = ? AND tq.worker_id = ?
		 ORDER BY tq.updated_at`,
		TaskStatusRunning, workerID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: in-flight tasks for worker: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []InFlightTaskForWorker
	for rows.Next() {
		var rec InFlightTaskForWorker
		var dir string
		if err := rows.Scan(&rec.TaskID, &rec.SupervisorAgentID, &rec.SessionID, &dir); err != nil {
			return nil, fmt.Errorf("store: scan in-flight task: %w", err)
		}
		if rec.SupervisorAgentID == "" {
			// Pre-supervisor task rows: nothing for the resumer to attach to.
			continue
		}
		if dir != "" {
			rec.EventsV1Path = filepath.Join(dir, "events-v1.jsonl")
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// WorkerCurrentTaskID returns the running task ID currently owned by the
// worker, or "" when the worker has nothing in flight.
func (s *Store) WorkerCurrentTaskID(workerID string) (string, error) {
	var taskID sql.NullString
	err := s.db.QueryRow(
		`SELECT id FROM task_queue WHERE worker_id = ? AND status = ? ORDER BY updated_at DESC LIMIT 1`,
		workerID, TaskStatusRunning,
	).Scan(&taskID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: worker current task: %w", err)
	}
	if !taskID.Valid {
		return "", nil
	}
	return taskID.String, nil
}

func parseTransitionPayload(payload string, ev *IssueTransitionEvent) {
	type rawTransition struct {
		From string `json:"from"`
		To   string `json:"to"`
		By   string `json:"by"`
	}
	if payload == "" {
		return
	}
	var raw rawTransition
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return
	}
	ev.From = raw.From
	ev.To = raw.To
	ev.By = raw.By
}

// ---------------------------------------------------------------------------
// Metrics aggregates
// ---------------------------------------------------------------------------

// EventCountByRepoType is one row of the (repo,type,count) aggregation used by
// the Prometheus metrics handler.
type EventCountByRepoType struct {
	Repo  string
	Type  string
	Count int64
}

// TokenUsagePayload is a single token_usage event payload surfaced to metrics.
type TokenUsagePayload struct {
	Repo    string
	Payload string
}

// TaskCountByRepoStatus aggregates task_queue rows by (repo,status).
type TaskCountByRepoStatus struct {
	Repo   string
	Status string
	Count  int64
}

// WorkerCountByRepo aggregates workers by repo including online totals.
type WorkerCountByRepo struct {
	Repo   string
	Total  int64
	Online int64
}

// TransitionMaxCount is the MAX(count) per (repo,from_state,to_state).
type TransitionMaxCount struct {
	Repo      string
	FromState string
	ToState   string
	MaxCount  int64
}

// IssueActivityRow is one issue_cache row joined with its last event time and
// active-task count, used to compute open/stuck issue gauges.
type IssueActivityRow struct {
	Repo            string
	IssueNum        int
	LabelsJSON      string
	State           string
	LastEventAt     sql.NullString
	ActiveTaskCount int
}

// CountEventsByRepoType returns the (repo,type,count) aggregation for all
// lifecycle events. Used by the metrics handler.
func (s *Store) CountEventsByRepoType() ([]EventCountByRepoType, error) {
	rows, err := s.db.Query(`SELECT COALESCE(repo, ''), COALESCE(type, ''), COUNT(1) FROM events GROUP BY repo, type`)
	if err != nil {
		return nil, fmt.Errorf("store: events by repo/type: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []EventCountByRepoType
	for rows.Next() {
		var m EventCountByRepoType
		if err := rows.Scan(&m.Repo, &m.Type, &m.Count); err != nil {
			return nil, fmt.Errorf("store: scan events agg: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// TokenUsageEvents returns the raw payload of every token_usage event along
// with its repo. Parsing is left to the caller.
func (s *Store) TokenUsageEvents(eventType string) ([]TokenUsagePayload, error) {
	rows, err := s.db.Query(`SELECT COALESCE(repo, ''), COALESCE(payload, '') FROM events WHERE type = ?`, eventType)
	if err != nil {
		return nil, fmt.Errorf("store: token usage events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TokenUsagePayload
	for rows.Next() {
		var t TokenUsagePayload
		if err := rows.Scan(&t.Repo, &t.Payload); err != nil {
			return nil, fmt.Errorf("store: scan token usage: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CountTasksByRepoStatus returns task_queue counts grouped by (repo,status).
func (s *Store) CountTasksByRepoStatus() ([]TaskCountByRepoStatus, error) {
	rows, err := s.db.Query(`SELECT COALESCE(repo, ''), COALESCE(status, ''), COUNT(1) FROM task_queue GROUP BY repo, status`)
	if err != nil {
		return nil, fmt.Errorf("store: task counts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TaskCountByRepoStatus
	for rows.Next() {
		var m TaskCountByRepoStatus
		if err := rows.Scan(&m.Repo, &m.Status, &m.Count); err != nil {
			return nil, fmt.Errorf("store: scan task count: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountWorkersByRepo returns total and online worker counts grouped by repo.
func (s *Store) CountWorkersByRepo() ([]WorkerCountByRepo, error) {
	rows, err := s.db.Query(`
		SELECT COALESCE(repo, ''), COUNT(1),
		       SUM(CASE WHEN status = 'online' THEN 1 ELSE 0 END)
		FROM workers
		GROUP BY repo`)
	if err != nil {
		return nil, fmt.Errorf("store: worker counts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []WorkerCountByRepo
	for rows.Next() {
		var m WorkerCountByRepo
		if err := rows.Scan(&m.Repo, &m.Total, &m.Online); err != nil {
			return nil, fmt.Errorf("store: scan worker count: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MaxTransitionCounts returns the MAX(count) per transition.
func (s *Store) MaxTransitionCounts() ([]TransitionMaxCount, error) {
	rows, err := s.db.Query(`SELECT COALESCE(repo, ''), COALESCE(from_state, ''), COALESCE(to_state, ''), MAX(count) FROM transition_counts GROUP BY repo, from_state, to_state`)
	if err != nil {
		return nil, fmt.Errorf("store: transition max: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TransitionMaxCount
	for rows.Next() {
		var m TransitionMaxCount
		if err := rows.Scan(&m.Repo, &m.FromState, &m.ToState, &m.MaxCount); err != nil {
			return nil, fmt.Errorf("store: scan transition max: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListOpenIssueActivity returns, for every open issue, the activity row needed
// to compute open/stuck issue gauges. pendingStatus and runningStatus bound
// the "active task" count used to determine whether the issue is idle.
func (s *Store) ListOpenIssueActivity(pendingStatus, runningStatus string) ([]IssueActivityRow, error) {
	rows, err := s.db.Query(`
		SELECT
			ic.repo,
			ic.issue_num,
			ic.labels,
			COALESCE(ic.state, ''),
			(
				SELECT MAX(e.ts) FROM events e
				WHERE e.repo = ic.repo AND e.issue_num = ic.issue_num
			),
			(
				SELECT COUNT(1) FROM task_queue tq
				WHERE tq.repo = ic.repo AND tq.issue_num = ic.issue_num
					AND tq.status IN (?, ?)
			)
		FROM issue_cache ic
		WHERE (ic.state = 'open' OR ic.state IS NULL)
			AND (ic.state IS NULL OR ic.state NOT LIKE 'pr:%')`,
		pendingStatus, runningStatus)
	if err != nil {
		return nil, fmt.Errorf("store: open issue activity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []IssueActivityRow
	for rows.Next() {
		var r IssueActivityRow
		if err := rows.Scan(&r.Repo, &r.IssueNum, &r.LabelsJSON, &r.State, &r.LastEventAt, &r.ActiveTaskCount); err != nil {
			return nil, fmt.Errorf("store: scan open issue: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Audit API (auditapi + audit/http)
// ---------------------------------------------------------------------------

// CountActiveSessions returns the number of sessions whose joined task status
// (falling back to session status) is pending or running.
func (s *Store) CountActiveSessions() (int, error) {
	if s.coordinatorMode {
		// Phase 3 (REQ-122): coordinator's `sessions` table is dropped.
		// See CountTerminalSessionsSince for the same tradeoff. Returns
		// 0 so /api/v1/status keeps its shape; per-worker counts must
		// come from an aggregator that doesn't exist yet.
		return 0, nil
	}
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*)
		 FROM sessions s
		 LEFT JOIN task_queue t ON t.id = s.task_id
		 WHERE COALESCE(t.status, s.status) IN (?, ?)`,
		TaskStatusPending, TaskStatusRunning,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count active sessions: %w", err)
	}
	return n, nil
}

// CountWorkers returns the total number of rows in the workers table.
func (s *Store) CountWorkers() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM workers`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count workers: %w", err)
	}
	return n, nil
}

// LastEventTimestampByType returns the timestamp of the most recent event of
// the given type, or nil if none exists.
func (s *Store) LastEventTimestampByType(eventType string) (*time.Time, error) {
	var raw sql.NullString
	err := s.db.QueryRow(
		`SELECT ts FROM events WHERE type = ? ORDER BY id DESC LIMIT 1`,
		eventType,
	).Scan(&raw)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("store: last event by type: %w", err)
	}
	if !raw.Valid {
		return nil, nil
	}
	ts, ok := ParseTimestamp(raw.String, "event.ts")
	if !ok {
		return nil, nil
	}
	ts = ts.UTC()
	return &ts, nil
}

// SessionListFilter narrows ListSessionsForAPI.
type SessionListFilter struct {
	Repo      string
	AgentName string
	IssueNum  int
	Limit     int
	Offset    int
}

// ListSessionsForAPI returns session rows with joined task status, ordered
// newest first, for the audit API. This preserves the exact JOIN semantics
// previously inlined in auditapi.
func (s *Store) ListSessionsForAPI(filter SessionListFilter) ([]SessionRecord, error) {
	if s.coordinatorMode {
		// Phase 3 (REQ-122): coordinator's `sessions` table is dropped.
		// The /api/v1/sessions endpoint is served by sessionproxy fan-
		// out, not by this method, on a coordinator. Returning an empty
		// slice keeps test fixtures and any orphan caller (the legacy
		// sessionproxy local fallback) cleanly empty.
		return nil, nil
	}
	query := `SELECT s.id, s.session_id, s.task_id, s.repo, s.issue_num, s.agent_name, s.runtime, s.worker_id, s.attempt,
	                 COALESCE(t.status, s.status),
	                 s.dir, s.stdout_path, s.stderr_path, s.tool_calls_path, s.metadata_path, s.summary, s.raw_path, s.created_at, s.closed_at
	          FROM sessions s
	          LEFT JOIN task_queue t ON t.id = s.task_id
	          WHERE 1=1`
	args := make([]any, 0, 5)
	if trimmed := strings.TrimSpace(filter.Repo); trimmed != "" {
		query += ` AND s.repo = ?`
		args = append(args, trimmed)
	}
	if trimmed := strings.TrimSpace(filter.AgentName); trimmed != "" {
		query += ` AND s.agent_name = ?`
		args = append(args, trimmed)
	}
	if filter.IssueNum > 0 {
		query += ` AND s.issue_num = ?`
		args = append(args, filter.IssueNum)
	}
	query += ` ORDER BY s.id DESC LIMIT ? OFFSET ?`
	args = append(args, filter.Limit, filter.Offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions for api: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SessionRecord
	for rows.Next() {
		record, err := scanSessionRecordForAPI(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: scan session for api: %w", err)
		}
		out = append(out, *record)
	}
	return out, rows.Err()
}

func scanSessionRecordForAPI(scan func(dest ...any) error) (*SessionRecord, error) {
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
	record.CreatedAt, _ = ParseTimestamp(createdAt, "session.created_at")
	if closedAt.Valid {
		record.ClosedAt, _ = ParseTimestamp(closedAt.String, "session.closed_at")
	}
	return &record, nil
}

// SessionAggregateMetrics captures the aggregate session metrics returned to
// the audit API.
type SessionAggregateMetrics struct {
	Total       int
	Successful  int
	Retried     int
	AvgDuration sql.NullFloat64
}

// AggregateSessionMetrics returns the counts/averages used by the audit API's
// /api/v1/metrics endpoint.
func (s *Store) AggregateSessionMetrics() (SessionAggregateMetrics, error) {
	if s.coordinatorMode {
		// Phase 3 (REQ-122): coordinator's `sessions` table is dropped.
		// /api/v1/metrics on the coordinator now reflects only its own
		// surface (issue cache, transitions, tasks); session-derived
		// success/retry rates would require fanning out to every
		// worker, which the metrics handler does not yet model.
		return SessionAggregateMetrics{}, nil
	}
	var m SessionAggregateMetrics
	var successful, retried sql.NullInt64
	err := s.db.QueryRow(
		`SELECT
			 COUNT(*),
			 SUM(CASE WHEN COALESCE(t.status, s.status) = ? THEN 1 ELSE 0 END),
			 AVG(CASE
				 WHEN s.closed_at IS NOT NULL THEN (julianday(s.closed_at) - julianday(s.created_at)) * 86400.0
				 ELSE NULL
			 END),
			 SUM(CASE WHEN s.attempt > 1 THEN 1 ELSE 0 END)
		 FROM sessions s
		 LEFT JOIN task_queue t ON t.id = s.task_id
		 WHERE COALESCE(t.status, s.status) IN (?, ?, ?)`,
		TaskStatusCompleted,
		TaskStatusCompleted, TaskStatusFailed, TaskStatusTimeout,
	).Scan(&m.Total, &successful, &m.AvgDuration, &retried)
	if err != nil {
		return m, fmt.Errorf("store: aggregate session metrics: %w", err)
	}
	if successful.Valid {
		m.Successful = int(successful.Int64)
	}
	if retried.Valid {
		m.Retried = int(retried.Int64)
	}
	return m, nil
}

// SessionCountByAgent is one row of the per-agent session count aggregation.
type SessionCountByAgent struct {
	AgentName string
	Count     int
}

// CountSessionsByAgent returns per-agent session counts ordered by agent name.
func (s *Store) CountSessionsByAgent() ([]SessionCountByAgent, error) {
	if s.coordinatorMode {
		// Phase 3 (REQ-122): coordinator's `sessions` table is dropped.
		// See AggregateSessionMetrics comment for the same tradeoff.
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT agent_name, COUNT(*) FROM sessions GROUP BY agent_name ORDER BY agent_name`)
	if err != nil {
		return nil, fmt.Errorf("store: sessions by agent: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SessionCountByAgent
	for rows.Next() {
		var r SessionCountByAgent
		if err := rows.Scan(&r.AgentName, &r.Count); err != nil {
			return nil, fmt.Errorf("store: scan session by agent: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Workflow (workflow.Manager)
// ---------------------------------------------------------------------------

// WorkflowInstanceRow is the raw workflow_instances row returned to the
// workflow package. Keeping it string-typed for timestamps lets workflow
// parse them with its own helpers and keeps store.go time-format agnostic.
type WorkflowInstanceRow struct {
	ID           string
	WorkflowName string
	Repo         string
	IssueNum     int
	CurrentState string
	CreatedAt    string
	UpdatedAt    string
}

// WorkflowTransitionRow is the raw workflow_transitions row.
type WorkflowTransitionRow struct {
	FromState    string
	ToState      string
	TriggerAgent string
	CreatedAt    string
}

// ErrWorkflowInstanceNotFound is returned when UpdateWorkflowInstanceState
// fails to match an existing instance.
var ErrWorkflowInstanceNotFound = fmt.Errorf("workflow instance not found")

// CreateWorkflowInstanceIfMissing inserts a new workflow_instances row if it
// does not already exist.
func (s *Store) CreateWorkflowInstanceIfMissing(id, workflowName, repo string, issueNum int, currentState string) error {
	_, err := s.db.Exec(
		`INSERT INTO workflow_instances (id, workflow_name, repo, issue_num, current_state)
		 VALUES (?, ?, ?, ?, ?) ON CONFLICT (repo, issue_num, workflow_name) DO NOTHING`,
		id, workflowName, repo, issueNum, currentState,
	)
	if err != nil {
		return fmt.Errorf("store: create workflow instance: %w", err)
	}
	return nil
}

// AdvanceWorkflowInstance writes a new transition and updates CurrentState in
// a single transaction. Returns ErrWorkflowInstanceNotFound if no row matched.
func (s *Store) AdvanceWorkflowInstance(id, fromState, toState, triggerAgent string, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin advance workflow: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ts := at.UTC().Format(time.RFC3339)
	if _, err := tx.Exec(
		`INSERT INTO workflow_transitions (workflow_instance_id, from_state, to_state, trigger_agent, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		id, fromState, toState, triggerAgent, ts,
	); err != nil {
		return fmt.Errorf("store: insert workflow transition: %w", err)
	}

	result, err := tx.Exec(
		`UPDATE workflow_instances SET current_state = ?, updated_at = ? WHERE id = ?`,
		toState, ts, id,
	)
	if err != nil {
		return fmt.Errorf("store: update workflow instance: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: workflow rows affected: %w", err)
	}
	if rows == 0 {
		return ErrWorkflowInstanceNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit advance workflow: %w", err)
	}
	return nil
}

// QueryWorkflowInstancesByRepoIssue returns all workflow_instances rows for
// one issue ordered by creation time.
func (s *Store) QueryWorkflowInstancesByRepoIssue(repo string, issueNum int) ([]WorkflowInstanceRow, error) {
	rows, err := s.db.Query(
		`SELECT id, workflow_name, repo, issue_num, current_state, created_at, updated_at
		 FROM workflow_instances
		 WHERE repo = ? AND issue_num = ?
		 ORDER BY created_at, id`,
		repo, issueNum,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query workflow instances: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WorkflowInstanceRow
	for rows.Next() {
		var r WorkflowInstanceRow
		if err := rows.Scan(&r.ID, &r.WorkflowName, &r.Repo, &r.IssueNum, &r.CurrentState, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan workflow instance: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetWorkflowInstanceByID loads a single workflow_instances row.
func (s *Store) GetWorkflowInstanceByID(id string) (*WorkflowInstanceRow, error) {
	row := s.db.QueryRow(
		`SELECT id, workflow_name, repo, issue_num, current_state, created_at, updated_at
		 FROM workflow_instances
		 WHERE id = ?`,
		id,
	)
	var r WorkflowInstanceRow
	err := row.Scan(&r.ID, &r.WorkflowName, &r.Repo, &r.IssueNum, &r.CurrentState, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrWorkflowInstanceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get workflow instance: %w", err)
	}
	return &r, nil
}

// QueryWorkflowTransitions returns the ordered transition history for an
// instance.
func (s *Store) QueryWorkflowTransitions(instanceID string) ([]WorkflowTransitionRow, error) {
	rows, err := s.db.Query(
		`SELECT from_state, to_state, trigger_agent, created_at
		 FROM workflow_transitions
		 WHERE workflow_instance_id = ?
		 ORDER BY created_at ASC, id ASC`,
		instanceID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query workflow transitions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]WorkflowTransitionRow, 0)
	for rows.Next() {
		var r WorkflowTransitionRow
		if err := rows.Scan(&r.FromState, &r.ToState, &r.TriggerAgent, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan workflow transition: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Issue cycle state — orchestrator-level dev↔review counter and timing
// used by the max_review_cycles cap and the long-flight stuck detector.
// ---------------------------------------------------------------------------

// IncrementDevReviewCycleCount atomically increments the per-issue
// dev_review_cycle_count and returns the new value. The row is created on
// first call. updated_at is bumped to CURRENT_TIMESTAMP.
func (s *Store) IncrementDevReviewCycleCount(repo string, issueNum int) (int, error) {
	var count int
	err := s.db.QueryRow(
		`INSERT INTO issue_cycle_state (repo, issue_num, dev_review_cycle_count, updated_at)
		 VALUES (?, ?, 1, CURRENT_TIMESTAMP)
		 ON CONFLICT (repo, issue_num)
		 DO UPDATE SET dev_review_cycle_count = dev_review_cycle_count + 1,
		               updated_at = CURRENT_TIMESTAMP
		 RETURNING dev_review_cycle_count`,
		repo, issueNum,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: increment dev_review_cycle_count: %w", err)
	}
	return count, nil
}

func (s *Store) IncrementSynthCycleCount(repo string, issueNum int) (int, error) {
	var count int
	err := s.db.QueryRow(
		`INSERT INTO issue_cycle_state (repo, issue_num, synth_cycle_count, updated_at)
		 VALUES (?, ?, 1, CURRENT_TIMESTAMP)
		 ON CONFLICT (repo, issue_num)
		 DO UPDATE SET synth_cycle_count = synth_cycle_count + 1,
		               updated_at = CURRENT_TIMESTAMP
		 RETURNING synth_cycle_count`,
		repo, issueNum,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: increment synth_cycle_count: %w", err)
	}
	return count, nil
}

// TouchIssueFirstDispatch records the first time an agent was dispatched for
// (repo, issueNum). Subsequent calls are no-ops. Used by the long-flight
// stuck detector to measure total in-flight time independent of per-state
// dwell time.
func (s *Store) TouchIssueFirstDispatch(repo string, issueNum int) error {
	_, err := s.db.Exec(
		`INSERT INTO issue_cycle_state (repo, issue_num, first_dispatch_at, updated_at)
		 VALUES (?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		 ON CONFLICT (repo, issue_num)
		 DO UPDATE SET first_dispatch_at = COALESCE(issue_cycle_state.first_dispatch_at, CURRENT_TIMESTAMP),
		               updated_at = CURRENT_TIMESTAMP`,
		repo, issueNum,
	)
	if err != nil {
		return fmt.Errorf("store: touch issue first_dispatch_at: %w", err)
	}
	return nil
}

// MarkIssueCycleCapHit records the moment the issue tripped the
// max_review_cycles cap. Idempotent.
func (s *Store) MarkIssueCycleCapHit(repo string, issueNum int) error {
	_, err := s.db.Exec(
		`INSERT INTO issue_cycle_state (repo, issue_num, cap_hit_at, updated_at)
		 VALUES (?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		 ON CONFLICT (repo, issue_num)
		 DO UPDATE SET cap_hit_at = COALESCE(issue_cycle_state.cap_hit_at, CURRENT_TIMESTAMP),
		               updated_at = CURRENT_TIMESTAMP`,
		repo, issueNum,
	)
	if err != nil {
		return fmt.Errorf("store: mark issue cycle cap hit: %w", err)
	}
	return nil
}

func (s *Store) MarkIssueSynthCycleCapHit(repo string, issueNum int) error {
	_, err := s.db.Exec(
		`INSERT INTO issue_cycle_state (repo, issue_num, synth_cap_hit_at, updated_at)
		 VALUES (?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		 ON CONFLICT (repo, issue_num)
		 DO UPDATE SET synth_cap_hit_at = COALESCE(issue_cycle_state.synth_cap_hit_at, CURRENT_TIMESTAMP),
		               updated_at = CURRENT_TIMESTAMP`,
		repo, issueNum,
	)
	if err != nil {
		return fmt.Errorf("store: mark issue synth cycle cap hit: %w", err)
	}
	return nil
}

// QueryIssueCycleState returns the per-issue cycle counter and timing. Returns
// (nil, nil) when no row exists for the issue.
func (s *Store) QueryIssueCycleState(repo string, issueNum int) (*IssueCycleState, error) {
	row := s.db.QueryRow(
		`SELECT repo, issue_num, dev_review_cycle_count, synth_cycle_count,
		        first_dispatch_at, cap_hit_at, synth_cap_hit_at, updated_at
		 FROM issue_cycle_state
		 WHERE repo = ? AND issue_num = ?`,
		repo, issueNum,
	)
	var rec IssueCycleState
	var firstDispatch, capHit, synthCapHit, updated sql.NullString
	err := row.Scan(&rec.Repo, &rec.IssueNum, &rec.DevReviewCycleCount, &rec.SynthCycleCount,
		&firstDispatch, &capHit, &synthCapHit, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: query issue cycle state: %w", err)
	}
	if firstDispatch.Valid {
		rec.FirstDispatchAt, _ = ParseTimestamp(firstDispatch.String, "issue_cycle_state.first_dispatch_at")
	}
	if capHit.Valid {
		rec.CapHitAt, _ = ParseTimestamp(capHit.String, "issue_cycle_state.cap_hit_at")
	}
	if synthCapHit.Valid {
		rec.SynthCapHitAt, _ = ParseTimestamp(synthCapHit.String, "issue_cycle_state.synth_cap_hit_at")
	}
	if updated.Valid {
		rec.UpdatedAt, _ = ParseTimestamp(updated.String, "issue_cycle_state.updated_at")
	}
	return &rec, nil
}

// ResetIssueCycleState removes the per-issue cycle counter row. Used by
// `workbuddy issue restart` so that an explicit restart clears the cycle
// counter alongside other recovery state.
func (s *Store) ResetIssueCycleState(repo string, issueNum int) error {
	_, err := s.db.Exec(
		`DELETE FROM issue_cycle_state WHERE repo = ? AND issue_num = ?`,
		repo, issueNum,
	)
	if err != nil {
		return fmt.Errorf("store: reset issue cycle state: %w", err)
	}
	return nil
}
