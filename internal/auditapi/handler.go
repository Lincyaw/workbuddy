// Package auditapi serves the read-only HTTP audit API for external tooling.
package auditapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// Handler serves the JSON audit API.
type Handler struct {
	store       *store.Store
	events      *eventlog.EventLogger
	sessionsDir string
}

// NewHandler constructs a Handler backed by the given store.
func NewHandler(st *store.Store) *Handler {
	return &Handler{
		store:  st,
		events: eventlog.NewEventLogger(st),
	}
}

// SetSessionsDir configures the directory where per-session artifacts live.
func (h *Handler) SetSessionsDir(dir string) {
	h.sessionsDir = dir
}

// Register mounts the audit API routes.
func (h *Handler) Register(mux *http.ServeMux) {
	h.RegisterCore(mux)
	h.RegisterDashboard(mux)
	mux.HandleFunc("/sessions/", h.handleSession)
}

// RegisterCore mounts the audit routes that do not conflict with the existing
// session HTML UI.
func (h *Handler) RegisterCore(mux *http.ServeMux) {
	mux.HandleFunc("/events", h.handleEvents)
	mux.HandleFunc("/issues/", h.handleIssueState)
}

// RegisterDashboard mounts the v1 JSON dashboard API.
func (h *Handler) RegisterDashboard(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/status", h.handleAPIStatus)
	mux.HandleFunc("/api/v1/sessions", h.handleAPISessions)
	mux.HandleFunc("/api/v1/sessions/", h.handleAPISession)
	mux.HandleFunc("/api/v1/events", h.handleAPIEvents)
	mux.HandleFunc("/api/v1/metrics", h.handleAPIMetrics)
	mux.HandleFunc("/api/v1/workers", h.handleAPIWorkers)
}

// RegisterSessionsOnly mounts only the /sessions/ JSON endpoint without the
// core /events and /issues/ routes (use when those are already registered by
// another handler such as audit.HTTPHandler).
func (h *Handler) RegisterSessionsOnly(mux *http.ServeMux) {
	mux.HandleFunc("/sessions/", h.handleSession)
}

type eventsResponse struct {
	Events  []eventResponse `json:"events"`
	Filters eventFilterEcho `json:"filters"`
}

type eventResponse struct {
	ID       int64     `json:"id"`
	TS       time.Time `json:"ts"`
	Type     string    `json:"type"`
	Repo     string    `json:"repo"`
	IssueNum int       `json:"issue_num,omitempty"`
	Payload  any       `json:"payload,omitempty"`
}

type eventFilterEcho struct {
	Repo  string `json:"repo,omitempty"`
	Issue int    `json:"issue,omitempty"`
	Type  string `json:"type,omitempty"`
	Since string `json:"since,omitempty"`
}

type issueStateResponse struct {
	Repo              string                    `json:"repo"`
	IssueNum          int                       `json:"issue_num"`
	State             string                    `json:"state,omitempty"`
	Labels            []string                  `json:"labels"`
	CycleCount        int                       `json:"cycle_count"`
	DependencyVerdict string                    `json:"dependency_verdict,omitempty"`
	DependencyState   *dependencyStateResponse  `json:"dependency_state,omitempty"`
	TransitionCounts  []transitionCountResponse `json:"transition_counts,omitempty"`
}

type dependencyStateResponse struct {
	Verdict             string    `json:"verdict"`
	ResumeLabel         string    `json:"resume_label,omitempty"`
	BlockedReasonHash   string    `json:"blocked_reason_hash,omitempty"`
	OverrideActive      bool      `json:"override_active"`
	GraphVersion        int64     `json:"graph_version"`
	LastReactionBlocked bool      `json:"last_reaction_blocked"`
	LastEvaluatedAt     time.Time `json:"last_evaluated_at"`
}

type transitionCountResponse struct {
	FromState string `json:"from_state"`
	ToState   string `json:"to_state"`
	Count     int    `json:"count"`
}

// SessionResponse is the read-only JSON shape served for /sessions/:id.
type SessionResponse struct {
	SessionID     string               `json:"session_id"`
	TaskID        string               `json:"task_id,omitempty"`
	Repo          string               `json:"repo"`
	IssueNum      int                  `json:"issue_num"`
	AgentName     string               `json:"agent_name"`
	CreatedAt     time.Time            `json:"created_at"`
	Summary       string               `json:"summary,omitempty"`
	ArtifactPaths SessionArtifactPaths `json:"artifact_paths"`
}

// SessionArtifactPaths points callers to persisted session artifacts.
type SessionArtifactPaths struct {
	SessionDir string `json:"session_dir,omitempty"`
	EventsV1   string `json:"events_v1,omitempty"`
	Raw        string `json:"raw,omitempty"`
}

type statusResponse struct {
	ActiveSessions int        `json:"active_sessions"`
	Workers        int        `json:"workers"`
	LastPoll       *time.Time `json:"last_poll"`
}

type sessionListResponse struct {
	SessionID  string     `json:"session_id"`
	TaskID     string     `json:"task_id,omitempty"`
	Repo       string     `json:"repo"`
	IssueNum   int        `json:"issue_num"`
	AgentName  string     `json:"agent_name"`
	Runtime    string     `json:"runtime,omitempty"`
	WorkerID   string     `json:"worker_id,omitempty"`
	Attempt    int        `json:"attempt"`
	Status     string     `json:"status"`
	ExitCode   int        `json:"exit_code"`
	Duration   int64      `json:"duration"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Summary    string     `json:"summary,omitempty"`
}

type sessionDetailResponse struct {
	SessionID     string               `json:"session_id"`
	TaskID        string               `json:"task_id,omitempty"`
	Repo          string               `json:"repo"`
	IssueNum      int                  `json:"issue_num"`
	AgentName     string               `json:"agent_name"`
	Runtime       string               `json:"runtime,omitempty"`
	WorkerID      string               `json:"worker_id,omitempty"`
	Attempt       int                  `json:"attempt"`
	Status        string               `json:"status"`
	ExitCode      int                  `json:"exit_code"`
	Duration      int64                `json:"duration"`
	CreatedAt     time.Time            `json:"created_at"`
	FinishedAt    *time.Time           `json:"finished_at"`
	Summary       string               `json:"summary,omitempty"`
	StdoutSummary string               `json:"stdout_summary,omitempty"`
	StderrSummary string               `json:"stderr_summary,omitempty"`
	ArtifactPaths SessionArtifactPaths `json:"artifact_paths"`
}

type metricsResponse struct {
	SuccessRate     float64        `json:"success_rate"`
	AvgDuration     float64        `json:"avg_duration"`
	RetryRate       float64        `json:"retry_rate"`
	AgentExecutions map[string]int `json:"agent_executions"`
}

type workerResponse struct {
	ID            string    `json:"id"`
	Repo          string    `json:"repo"`
	Roles         []string  `json:"roles"`
	Hostname      string    `json:"hostname,omitempty"`
	Status        string    `json:"status"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	RegisteredAt  time.Time `json:"registered_at"`
}

func (h *Handler) handleEvents(w http.ResponseWriter, r *http.Request) {
	events, filter, err := h.queryEvents(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp := eventsResponse{
		Events: make([]eventResponse, 0, len(events)),
		Filters: eventFilterEcho{
			Repo:  filter.Repo,
			Issue: filter.IssueNum,
			Type:  filter.Type,
			Since: r.URL.Query().Get("since"),
		},
	}
	for _, ev := range events {
		resp.Events = append(resp.Events, eventResponse{
			ID:       ev.ID,
			TS:       ev.TS,
			Type:     ev.Type,
			Repo:     ev.Repo,
			IssueNum: ev.IssueNum,
			Payload:  decodeJSONOrString(ev.Payload),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	resp, err := h.queryStatus()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query status")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleAPISessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit, offset, err := parseLimitOffset(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sessions, err := h.listSessions(r.URL.Query().Get("repo"), limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query sessions")
		return
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (h *Handler) handleAPISession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sessionID := strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/")
	if !isValidSessionID(sessionID) || strings.Contains(sessionID, "/") {
		http.NotFound(w, r)
		return
	}
	record, err := h.store.GetSession(sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query session")
		return
	}
	if record == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	resp, err := h.buildSessionDetail(*record)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build session detail")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleAPIEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	events, _, err := h.queryEvents(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp := make([]eventResponse, 0, len(events))
	for _, ev := range events {
		resp = append(resp, eventResponse{
			ID:       ev.ID,
			TS:       ev.TS,
			Type:     ev.Type,
			Repo:     ev.Repo,
			IssueNum: ev.IssueNum,
			Payload:  decodeJSONOrString(ev.Payload),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleAPIMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	resp, err := h.queryMetrics()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query metrics")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleAPIWorkers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	workers, err := h.store.QueryWorkers(r.URL.Query().Get("repo"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query workers")
		return
	}
	resp := make([]workerResponse, 0, len(workers))
	for _, worker := range workers {
		resp = append(resp, workerResponse{
			ID:            worker.ID,
			Repo:          worker.Repo,
			Roles:         decodeRoles(worker.Roles),
			Hostname:      worker.Hostname,
			Status:        worker.Status,
			LastHeartbeat: worker.LastHeartbeat,
			RegisteredAt:  worker.RegisteredAt,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleIssueState(w http.ResponseWriter, r *http.Request) {
	repo, issueNum, ok := parseIssueStatePath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	cached, err := h.store.QueryIssueCache(repo, issueNum)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query issue cache")
		return
	}
	if cached == nil {
		http.NotFound(w, r)
		return
	}

	counts, err := h.store.QueryTransitionCounts(repo, issueNum)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query transition counts")
		return
	}
	depState, err := h.store.QueryIssueDependencyState(repo, issueNum)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query dependency state")
		return
	}

	resp := issueStateResponse{
		Repo:     repo,
		IssueNum: issueNum,
		State:    cached.State,
		Labels:   decodeLabels(cached.Labels),
	}
	for _, tc := range counts {
		resp.CycleCount += tc.Count
		resp.TransitionCounts = append(resp.TransitionCounts, transitionCountResponse{
			FromState: tc.FromState,
			ToState:   tc.ToState,
			Count:     tc.Count,
		})
	}
	if depState != nil {
		resp.DependencyVerdict = depState.Verdict
		resp.DependencyState = &dependencyStateResponse{
			Verdict:             depState.Verdict,
			ResumeLabel:         depState.ResumeLabel,
			BlockedReasonHash:   depState.BlockedReasonHash,
			OverrideActive:      depState.OverrideActive,
			GraphVersion:        depState.GraphVersion,
			LastReactionBlocked: depState.LastReactionBlocked,
			LastEvaluatedAt:     depState.LastEvaluatedAt,
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleSession(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimPrefix(r.URL.Path, "/sessions/")
	if !isValidSessionID(sessionID) || strings.Contains(sessionID, "/") {
		http.NotFound(w, r)
		return
	}

	session, err := h.store.GetAgentSession(sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query session")
		return
	}
	if session == nil {
		http.NotFound(w, r)
		return
	}

	resp := BuildSessionResponse(session, h.sessionsDir)
	writeJSON(w, http.StatusOK, resp)
}

// BuildSessionResponse constructs the JSON payload shared by the audit route
// and the existing /sessions/{id} HTML endpoint when callers request JSON.
func BuildSessionResponse(session *store.AgentSession, sessionsDir string) SessionResponse {
	resp := SessionResponse{
		SessionID: session.SessionID,
		TaskID:    session.TaskID,
		Repo:      session.Repo,
		IssueNum:  session.IssueNum,
		AgentName: session.AgentName,
		CreatedAt: session.CreatedAt,
		Summary:   session.Summary,
		ArtifactPaths: SessionArtifactPaths{
			Raw: session.RawPath,
		},
	}
	if sessionsDir != "" {
		resp.ArtifactPaths.SessionDir = filepath.Join(sessionsDir, session.SessionID)
		resp.ArtifactPaths.EventsV1 = filepath.Join(sessionsDir, session.SessionID, "events-v1.jsonl")
	}
	if resp.ArtifactPaths.SessionDir == "" && session.RawPath != "" {
		resp.ArtifactPaths.SessionDir = filepath.Dir(session.RawPath)
	}
	return resp
}

func (h *Handler) queryEvents(r *http.Request) ([]store.Event, eventlog.EventFilter, error) {
	q := r.URL.Query()
	filter := eventlog.EventFilter{
		Repo: strings.TrimSpace(q.Get("repo")),
		Type: strings.TrimSpace(q.Get("type")),
	}

	if issueStr := strings.TrimSpace(q.Get("issue")); issueStr != "" {
		issueNum, err := strconv.Atoi(issueStr)
		if err != nil {
			return nil, filter, fmt.Errorf("invalid issue query parameter")
		}
		filter.IssueNum = issueNum
	}

	if sinceStr := strings.TrimSpace(q.Get("since")); sinceStr != "" {
		since, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			return nil, filter, fmt.Errorf("invalid since query parameter; use RFC3339")
		}
		filter.Since = &since
	}

	if untilStr := strings.TrimSpace(q.Get("until")); untilStr != "" {
		until, err := time.Parse(time.RFC3339, untilStr)
		if err != nil {
			return nil, filter, fmt.Errorf("invalid until query parameter; use RFC3339")
		}
		filter.Until = &until
	}

	events, err := h.events.Query(filter)
	if err != nil {
		return nil, filter, fmt.Errorf("failed to query events")
	}
	return events, filter, nil
}

func (h *Handler) queryStatus() (statusResponse, error) {
	var resp statusResponse
	if err := h.store.DB().QueryRow(
		`SELECT COUNT(*)
		 FROM sessions s
		 LEFT JOIN task_queue t ON t.id = s.task_id
		 WHERE COALESCE(t.status, s.status) IN (?, ?)`,
		store.TaskStatusPending, store.TaskStatusRunning,
	).Scan(&resp.ActiveSessions); err != nil {
		return resp, fmt.Errorf("count active sessions: %w", err)
	}
	if err := h.store.DB().QueryRow(`SELECT COUNT(*) FROM workers`).Scan(&resp.Workers); err != nil {
		return resp, fmt.Errorf("count workers: %w", err)
	}
	var raw sql.NullString
	if err := h.store.DB().QueryRow(
		`SELECT ts FROM events WHERE type = ? ORDER BY id DESC LIMIT 1`,
		poller.EventPollCycleDone,
	).Scan(&raw); err != nil && err != sql.ErrNoRows {
		return resp, fmt.Errorf("query last poll: %w", err)
	}
	if raw.Valid {
		if ts, ok := parseSQLiteTimestamp(raw.String); ok {
			ts = ts.UTC()
			resp.LastPoll = &ts
		}
	}
	return resp, nil
}

func (h *Handler) listSessions(repo string, limit, offset int) ([]sessionListResponse, error) {
	query := `SELECT s.id, s.session_id, s.task_id, s.repo, s.issue_num, s.agent_name, s.runtime, s.worker_id, s.attempt,
	                 COALESCE(t.status, s.status),
	                 s.dir, s.stdout_path, s.stderr_path, s.tool_calls_path, s.metadata_path, s.summary, s.raw_path, s.created_at, s.closed_at
	          FROM sessions s
	          LEFT JOIN task_queue t ON t.id = s.task_id
	          WHERE 1=1`
	args := make([]any, 0, 3)
	if strings.TrimSpace(repo) != "" {
		query += ` AND s.repo = ?`
		args = append(args, strings.TrimSpace(repo))
	}
	query += ` ORDER BY s.id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := h.store.DB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []sessionListResponse
	for rows.Next() {
		record, err := scanSessionRecord(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		exitCode, _, finishedAt, err := h.sessionTaskStats(*record)
		if err != nil {
			return nil, err
		}
		out = append(out, sessionListResponse{
			SessionID:  record.SessionID,
			TaskID:     record.TaskID,
			Repo:       record.Repo,
			IssueNum:   record.IssueNum,
			AgentName:  record.AgentName,
			Runtime:    record.Runtime,
			WorkerID:   record.WorkerID,
			Attempt:    record.Attempt,
			Status:     record.Status,
			ExitCode:   exitCode,
			Duration:   sessionDuration(record.CreatedAt, finishedAt),
			CreatedAt:  record.CreatedAt,
			FinishedAt: finishedAt,
			Summary:    record.Summary,
		})
	}
	return out, rows.Err()
}

func (h *Handler) buildSessionDetail(record store.SessionRecord) (sessionDetailResponse, error) {
	exitCode, _, finishedAt, err := h.sessionTaskStats(record)
	if err != nil {
		return sessionDetailResponse{}, err
	}
	artifactPaths := SessionArtifactPaths{
		SessionDir: record.Dir,
		Raw:        record.RawPath,
	}
	if artifactPaths.SessionDir == "" && record.RawPath != "" {
		artifactPaths.SessionDir = filepath.Dir(record.RawPath)
	}
	if artifactPaths.SessionDir == "" && h.sessionsDir != "" {
		artifactPaths.SessionDir = filepath.Join(h.sessionsDir, record.SessionID)
	}
	if artifactPaths.EventsV1 == "" && artifactPaths.SessionDir != "" {
		artifactPaths.EventsV1 = filepath.Join(artifactPaths.SessionDir, "events-v1.jsonl")
	}
	return sessionDetailResponse{
		SessionID:     record.SessionID,
		TaskID:        record.TaskID,
		Repo:          record.Repo,
		IssueNum:      record.IssueNum,
		AgentName:     record.AgentName,
		Runtime:       record.Runtime,
		WorkerID:      record.WorkerID,
		Attempt:       record.Attempt,
		Status:        record.Status,
		ExitCode:      exitCode,
		Duration:      sessionDuration(record.CreatedAt, finishedAt),
		CreatedAt:     record.CreatedAt,
		FinishedAt:    finishedAt,
		Summary:       record.Summary,
		StdoutSummary: readArtifactSummary(record.StdoutPath, 4096),
		StderrSummary: readArtifactSummary(record.StderrPath, 4096),
		ArtifactPaths: artifactPaths,
	}, nil
}

func (h *Handler) sessionTaskStats(record store.SessionRecord) (int, string, *time.Time, error) {
	var exitCode int
	status := record.Status
	var finishedAt *time.Time
	if !record.ClosedAt.IsZero() {
		ts := record.ClosedAt.UTC()
		finishedAt = &ts
	}
	if record.TaskID == "" {
		return exitCode, status, finishedAt, nil
	}
	task, err := h.store.GetTask(record.TaskID)
	if err != nil {
		return 0, "", nil, fmt.Errorf("get task %s: %w", record.TaskID, err)
	}
	if task == nil {
		return exitCode, status, finishedAt, nil
	}
	exitCode = task.ExitCode
	status = task.Status
	if finishedAt == nil && !task.CompletedAt.IsZero() {
		ts := task.CompletedAt.UTC()
		finishedAt = &ts
	}
	return exitCode, status, finishedAt, nil
}

func (h *Handler) queryMetrics() (metricsResponse, error) {
	resp := metricsResponse{AgentExecutions: map[string]int{}}
	var total, successful, retried int
	var avg sql.NullFloat64
	if err := h.store.DB().QueryRow(
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
		store.TaskStatusCompleted,
		store.TaskStatusCompleted, store.TaskStatusFailed, store.TaskStatusTimeout,
	).Scan(&total, &successful, &avg, &retried); err != nil {
		return resp, fmt.Errorf("aggregate metrics: %w", err)
	}
	if total > 0 {
		resp.SuccessRate = float64(successful) / float64(total)
		resp.RetryRate = float64(retried) / float64(total)
	}
	if avg.Valid {
		resp.AvgDuration = avg.Float64
	}

	rows, err := h.store.DB().Query(`SELECT agent_name, COUNT(*) FROM sessions GROUP BY agent_name ORDER BY agent_name`)
	if err != nil {
		return resp, fmt.Errorf("aggregate per-agent counts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var agentName string
		var count int
		if err := rows.Scan(&agentName, &count); err != nil {
			return resp, fmt.Errorf("scan per-agent counts: %w", err)
		}
		resp.AgentExecutions[agentName] = count
	}
	return resp, rows.Err()
}

func scanSessionRecord(scan func(dest ...any) error) (*store.SessionRecord, error) {
	var record store.SessionRecord
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
	record.CreatedAt, _ = parseSQLiteTimestamp(createdAt)
	if closedAt.Valid {
		record.ClosedAt, _ = parseSQLiteTimestamp(closedAt.String)
	}
	return &record, nil
}

func parseSQLiteTimestamp(raw string) (time.Time, bool) {
	for _, layout := range []string{
		"2006-01-02 15:04:05",
		time.RFC3339,
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
	} {
		if ts, err := time.Parse(layout, raw); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}

func parseLimitOffset(r *http.Request) (int, int, error) {
	limit := 50
	offset := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return 0, 0, fmt.Errorf("invalid limit query parameter")
		}
		limit = value
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("offset")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return 0, 0, fmt.Errorf("invalid offset query parameter")
		}
		offset = value
	}
	return limit, offset, nil
}

func decodeRoles(raw string) []string {
	if raw == "" {
		return nil
	}
	var roles []string
	if err := json.Unmarshal([]byte(raw), &roles); err != nil {
		return nil
	}
	return roles
}

func sessionDuration(createdAt time.Time, finishedAt *time.Time) int64 {
	if createdAt.IsZero() || finishedAt == nil || finishedAt.IsZero() {
		return 0
	}
	return finishedAt.Sub(createdAt).Milliseconds()
}

func readArtifactSummary(path string, maxBytes int) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) > maxBytes {
		data = data[:maxBytes]
	}
	return string(data)
}

func parseIssueStatePath(path string) (string, int, bool) {
	trimmed := strings.TrimPrefix(path, "/issues/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 3 || parts[len(parts)-1] != "state" {
		return "", 0, false
	}
	issueNum, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil {
		return "", 0, false
	}
	repo := strings.Join(parts[:len(parts)-2], "/")
	if repo == "" {
		return "", 0, false
	}
	return repo, issueNum, true
}

func decodeLabels(raw string) []string {
	if raw == "" {
		return nil
	}
	var labels []string
	if err := json.Unmarshal([]byte(raw), &labels); err != nil {
		return nil
	}
	return labels
}

func decodeJSONOrString(raw string) any {
	if raw == "" {
		return nil
	}
	var out any
	if err := json.Unmarshal([]byte(raw), &out); err == nil {
		return out
	}
	return raw
}

func isValidSessionID(sessionID string) bool {
	if sessionID == "" || sessionID == "." || sessionID == ".." {
		return false
	}
	return !strings.ContainsAny(sessionID, `/\`)
}

func writeJSON(w http.ResponseWriter, statusCode int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, statusCode int, message string) {
	writeJSON(w, statusCode, map[string]string{"error": message})
}

// ArtifactExists is kept package-local for future health/debug additions.
func artifactExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func (h *Handler) String() string {
	return fmt.Sprintf("auditapi(sessions_dir=%s)", h.sessionsDir)
}
