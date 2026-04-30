// Package auditapi serves the read-only HTTP audit API for external tooling.
package auditapi

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/operator"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/sessionref"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// stuckThresholdSeconds is the elapsed-since-last-transition threshold beyond
// which an issue is reported as `stuck` on /api/v1/status. Mirrors the audit
// HTTP handler's 1-hour rule.
const stuckThresholdSeconds = int64(3600)

// StatusAbortedBeforeStart is the synthesized status reported for sessions
// where the runtime aborted before any events were recorded, so neither the
// agent_sessions DB row nor the events-v1.jsonl file ever materialized. The
// API surfaces it (instead of "completed"/"running") whenever degraded
// fallback to disk metadata is the only available signal.
const StatusAbortedBeforeStart = "aborted_before_start"

// degradedReason values populate the SessionDetail.DegradedReason field.
const (
	degradedReasonNoDBRow      = "no_db_row"
	degradedReasonNoEventsFile = "no_events_file"
)

// badRequestError wraps parameter-validation errors so callers can distinguish
// them from backend/DB errors returned by queryEvents.
type badRequestError struct {
	msg string
}

func (e *badRequestError) Error() string { return e.msg }

// Handler serves the JSON audit API.
type Handler struct {
	store           *store.Store
	events          *eventlog.EventLogger
	sessionsDir     string
	reportBaseURL   string
	now             func() time.Time
	sessionEventsFn http.HandlerFunc
	sessionStreamFn http.HandlerFunc
}

// NewHandler constructs a Handler backed by the given store.
func NewHandler(st *store.Store) *Handler {
	return &Handler{
		store:  st,
		events: eventlog.NewEventLogger(st),
		now:    time.Now,
	}
}

// SetSessionsDir configures the directory where per-session artifacts live.
func (h *Handler) SetSessionsDir(dir string) {
	h.sessionsDir = dir
}

// SetReportBaseURL configures the base URL used to format last_session_url on
// dashboard responses. Should match the reporter's --report-base-url flag.
func (h *Handler) SetReportBaseURL(baseURL string) {
	h.reportBaseURL = strings.TrimRight(baseURL, "/")
}

// SetNowFunc overrides the clock used for stuck-detection arithmetic. Tests
// inject a deterministic clock; passing nil restores time.Now.
func (h *Handler) SetNowFunc(now func() time.Time) {
	if now == nil {
		h.now = time.Now
		return
	}
	h.now = now
}

// SetSessionEventsHandler installs the handler invoked for
// /api/v1/sessions/{id}/events. The webui package owns the implementation
// (it reads events-v1.jsonl from disk); auditapi just routes the suffix to it.
func (h *Handler) SetSessionEventsHandler(fn http.HandlerFunc) {
	h.sessionEventsFn = fn
}

// SetSessionStreamHandler installs the handler invoked for
// /api/v1/sessions/{id}/stream (SSE).
func (h *Handler) SetSessionStreamHandler(fn http.HandlerFunc) {
	h.sessionStreamFn = fn
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
	mux.HandleFunc("/api/v1/alerts", h.handleAPIAlerts)
	mux.HandleFunc("/api/v1/metrics", h.handleAPIMetrics)
	mux.HandleFunc("/api/v1/workers", h.handleAPIWorkers)
	mux.HandleFunc("/api/v1/issues/in-flight", h.handleAPIIssuesInFlight)
	mux.HandleFunc("/api/v1/issues/", h.handleAPIIssueDetail)
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
	Repo                string                    `json:"repo"`
	IssueNum            int                       `json:"issue_num"`
	State               string                    `json:"state,omitempty"`
	Labels              []string                  `json:"labels"`
	CycleCount          int                       `json:"cycle_count"`
	DevReviewCycleCount int                       `json:"dev_review_cycle_count"`
	CapHit              bool                      `json:"cap_hit,omitempty"`
	DependencyVerdict   string                    `json:"dependency_verdict,omitempty"`
	DependencyState     *dependencyStateResponse  `json:"dependency_state,omitempty"`
	TransitionCounts    []transitionCountResponse `json:"transition_counts,omitempty"`
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
	ActiveSessions    int        `json:"active_sessions"`
	Workers           int        `json:"workers"`
	LastPoll          *time.Time `json:"last_poll"`
	InFlightIssues    int        `json:"in_flight_issues"`
	StuckIssuesOver1H int        `json:"stuck_issues_over_1h"`
	Done24H           int        `json:"done_24h"`
	Failed24H         int        `json:"failed_24h"`
}

type sessionListResponse struct {
	SessionID    string     `json:"session_id"`
	TaskID       string     `json:"task_id,omitempty"`
	Repo         string     `json:"repo"`
	IssueNum     int        `json:"issue_num"`
	AgentName    string     `json:"agent_name"`
	Runtime      string     `json:"runtime,omitempty"`
	WorkerID     string     `json:"worker_id,omitempty"`
	Attempt      int        `json:"attempt"`
	Status       string     `json:"status"`
	TaskStatus   string     `json:"task_status,omitempty"`
	Workflow     string     `json:"workflow,omitempty"`
	CurrentState string     `json:"current_state,omitempty"`
	ExitCode     int        `json:"exit_code"`
	Duration     int64      `json:"duration"`
	CreatedAt    time.Time  `json:"created_at"`
	FinishedAt   *time.Time `json:"finished_at"`
	Summary      string     `json:"summary,omitempty"`
	// Degraded marks rows whose persisted artefacts indicate the agent
	// never produced a normal session: either no DB row at all (the response
	// is synthesized from disk metadata.json) or the events-v1.jsonl file is
	// missing/empty for a non-running status. Issue #275.
	Degraded       bool   `json:"degraded,omitempty"`
	DegradedReason string `json:"degraded_reason,omitempty"`
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
	// Degraded is true when the API could not produce a fully-normal response
	// for this session — typically because the agent_sessions DB row never
	// got written (we fell back to disk metadata.json) or because no
	// events-v1.jsonl file was ever produced. The frontend uses this to
	// surface a "this session never produced any events" warning instead of
	// rendering an empty-but-otherwise-normal-looking timeline. Issue #275.
	Degraded       bool   `json:"degraded,omitempty"`
	DegradedReason string `json:"degraded_reason,omitempty"`
}

type metricsResponse struct {
	SuccessRate     float64        `json:"success_rate"`
	AvgDuration     float64        `json:"avg_duration_seconds"`
	RetryRate       float64        `json:"retry_rate"`
	AgentExecutions map[string]int `json:"agent_executions"`
}

type workerResponse struct {
	ID              string    `json:"id"`
	Repo            string    `json:"repo"`
	Roles           []string  `json:"roles"`
	Hostname        string    `json:"hostname,omitempty"`
	Status          string    `json:"status"`
	LastHeartbeat   time.Time `json:"last_heartbeat"`
	LastHeartbeatAt time.Time `json:"last_heartbeat_at"`
	RegisteredAt    time.Time `json:"registered_at"`
	CurrentTaskID   string    `json:"current_task_id,omitempty"`
	MgmtBaseURL     string    `json:"mgmt_base_url,omitempty"`
}

// inFlightIssueResponse is one row of /api/v1/issues/in-flight.
type inFlightIssueResponse struct {
	Repo             string         `json:"repo"`
	IssueNum         int            `json:"issue_num"`
	Title            string         `json:"title"`
	CurrentState     string         `json:"current_state"`
	CurrentLabel     string         `json:"current_label"`
	Labels           []string       `json:"labels"`
	CycleCounts      map[string]int `json:"cycle_counts"`
	LastTransitionAt *time.Time     `json:"last_transition_at,omitempty"`
	StuckForSeconds  int64          `json:"stuck_for_seconds"`
	ClaimedWorkerID  string         `json:"claimed_worker_id,omitempty"`
	LastSessionID    string         `json:"last_session_id,omitempty"`
	LastSessionURL   string         `json:"last_session_url,omitempty"`
	// Hazard, when non-empty, names a configuration-incompleteness condition
	// that prevents the issue from making progress. Set by the coordinator
	// from issue_pipeline_hazards (REQ #255). Possible values:
	//   "no-workflow-match"      — issue has status:* but no workflow trigger
	//   "awaiting-status-label"  — workflow trigger + depends_on but no status:*
	Hazard string `json:"hazard,omitempty"`
}

// issueTransitionResponse is one transition row in the issue-detail endpoint.
type issueTransitionResponse struct {
	From string    `json:"from"`
	To   string    `json:"to"`
	At   time.Time `json:"at"`
	By   string    `json:"by,omitempty"`
}

// issueSessionRefResponse is one session row in the issue-detail endpoint.
type issueSessionRefResponse struct {
	SessionID  string     `json:"session_id"`
	Agent      string     `json:"agent"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Status     string     `json:"status,omitempty"`
	ExitCode   int        `json:"exit_code"`
}

// issueDetailResponse is the body of /api/v1/issues/{repo}/{num}.
type issueDetailResponse struct {
	Repo             string                    `json:"repo"`
	IssueNum         int                       `json:"issue_num"`
	Title            string                    `json:"title"`
	CurrentState     string                    `json:"current_state"`
	Labels           []string                  `json:"labels"`
	Transitions      []issueTransitionResponse `json:"transitions"`
	TransitionCounts []transitionCountResponse `json:"transition_counts"`
	Sessions         []issueSessionRefResponse `json:"sessions"`
}

func (h *Handler) handleEvents(w http.ResponseWriter, r *http.Request) {
	events, filter, err := h.queryEvents(r)
	if err != nil {
		var bre *badRequestError
		if errors.As(err, &bre) {
			writeError(w, http.StatusBadRequest, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
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
	q := r.URL.Query()
	issue := 0
	if raw := strings.TrimSpace(q.Get("issue")); raw != "" {
		parsed, perr := strconv.Atoi(raw)
		if perr != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "invalid issue parameter")
			return
		}
		issue = parsed
	}
	sessions, err := h.listSessions(sessionListParams{
		Repo:      q.Get("repo"),
		AgentName: q.Get("agent"),
		IssueNum:  issue,
		Limit:     limit,
		Offset:    offset,
	})
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
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/")
	sessionID, suffix, _ := strings.Cut(rest, "/")
	if !isValidSessionID(sessionID) {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	switch suffix {
	case "":
		h.serveSessionDetail(w, sessionID)
	case "events":
		if h.sessionEventsFn == nil {
			writeError(w, http.StatusNotFound, "session events not configured")
			return
		}
		h.sessionEventsFn(w, r)
	case "stream":
		if h.sessionStreamFn == nil {
			writeError(w, http.StatusNotFound, "session stream not configured")
			return
		}
		h.sessionStreamFn(w, r)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (h *Handler) serveSessionDetail(w http.ResponseWriter, sessionID string) {
	record, err := h.store.GetSession(sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query session")
		return
	}
	if record == nil {
		// Issue #275: if no agent_sessions DB row exists, attempt to
		// reconstruct a degraded response from the on-disk metadata.json
		// so the operator can still see what happened. The response is
		// flagged degraded=true so the SPA renders a "never produced any
		// events" warning instead of an empty-looking normal session.
		if resp, ok := h.buildDegradedFromDisk(sessionID); ok {
			writeJSON(w, http.StatusOK, resp)
			return
		}
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

func (h *Handler) handleAPIIssuesInFlight(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	caches, err := h.collectInFlightIssues()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to collect in-flight issues")
		return
	}
	now := h.now().UTC()
	out := make([]inFlightIssueResponse, 0, len(caches))
	for _, ic := range caches {
		row, err := h.buildInFlightRow(ic, now)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to build in-flight row")
			return
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) buildInFlightRow(ic store.IssueCache, now time.Time) (inFlightIssueResponse, error) {
	labels := decodeLabels(ic.Labels)
	cur := currentLabel(labels)
	row := inFlightIssueResponse{
		Repo:         ic.Repo,
		IssueNum:     ic.IssueNum,
		Title:        firstLine(ic.Body),
		CurrentState: labelToState(cur),
		CurrentLabel: cur,
		Labels:       labels,
		CycleCounts:  map[string]int{},
	}
	counts, err := h.store.QueryTransitionCounts(ic.Repo, ic.IssueNum)
	if err != nil {
		return row, fmt.Errorf("transition counts: %w", err)
	}
	for _, tc := range counts {
		row.CycleCounts[tc.FromState+"->"+tc.ToState] = tc.Count
	}
	last, err := h.store.LatestIssueTransition(ic.Repo, ic.IssueNum)
	if err != nil {
		return row, fmt.Errorf("latest transition: %w", err)
	}
	if last != nil {
		ts := last.At
		row.LastTransitionAt = &ts
		if !ts.IsZero() {
			seconds := int64(now.Sub(ts).Seconds())
			if seconds < 0 {
				seconds = 0
			}
			row.StuckForSeconds = seconds
		}
	}
	if claim, err := h.store.QueryIssueClaim(ic.Repo, ic.IssueNum); err == nil && claim != nil {
		row.ClaimedWorkerID = claim.WorkerID
	}
	if session, err := h.store.LatestSessionForIssue(ic.Repo, ic.IssueNum); err == nil && session != nil {
		row.LastSessionID = session.SessionID
		row.LastSessionURL = sessionref.BuildURL(h.reportBaseURL, session.WorkerID, session.SessionID)
	}
	if hazard, err := h.store.QueryIssuePipelineHazard(ic.Repo, ic.IssueNum); err == nil && hazard != nil {
		row.Hazard = hazard.Kind
	}
	return row, nil
}

func (h *Handler) handleAPIIssueDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/issues/")
	if rest == "" || rest == "in-flight" {
		// The in-flight handler is registered at the more-specific path, so
		// reaching here means a malformed request.
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	repo, issueNum, ok := splitRepoIssue(rest)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid issue path; expect /api/v1/issues/<owner>/<repo>/<num>")
		return
	}
	cache, err := h.store.QueryIssueCache(repo, issueNum)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query issue cache")
		return
	}
	if cache == nil {
		writeError(w, http.StatusNotFound, "issue not found")
		return
	}
	labels := decodeLabels(cache.Labels)
	resp := issueDetailResponse{
		Repo:             cache.Repo,
		IssueNum:         cache.IssueNum,
		Title:            firstLine(cache.Body),
		CurrentState:     labelToState(currentLabel(labels)),
		Labels:           labels,
		Transitions:      []issueTransitionResponse{},
		TransitionCounts: []transitionCountResponse{},
		Sessions:         []issueSessionRefResponse{},
	}
	transitions, err := h.store.QueryIssueTransitions(repo, issueNum)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query transitions")
		return
	}
	for _, t := range transitions {
		resp.Transitions = append(resp.Transitions, issueTransitionResponse{
			From: t.From,
			To:   t.To,
			At:   t.At,
			By:   t.By,
		})
	}
	counts, err := h.store.QueryTransitionCounts(repo, issueNum)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query transition counts")
		return
	}
	for _, c := range counts {
		resp.TransitionCounts = append(resp.TransitionCounts, transitionCountResponse{
			FromState: c.FromState,
			ToState:   c.ToState,
			Count:     c.Count,
		})
	}
	sessions, err := h.store.ListSessions(store.SessionFilter{Repo: repo, IssueNum: issueNum})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list sessions")
		return
	}
	for _, sess := range sessions {
		exitCode, _, finishedAt, _ := h.sessionTaskStats(sess)
		resp.Sessions = append(resp.Sessions, issueSessionRefResponse{
			SessionID:  sess.SessionID,
			Agent:      sess.AgentName,
			StartedAt:  sess.CreatedAt,
			FinishedAt: finishedAt,
			Status:     sess.Status,
			ExitCode:   exitCode,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func splitRepoIssue(trimmed string) (string, int, bool) {
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 {
		return "", 0, false
	}
	tail := parts[len(parts)-1]
	issueNum, err := strconv.Atoi(tail)
	if err != nil || issueNum <= 0 {
		return "", 0, false
	}
	repo := strings.Join(parts[:len(parts)-1], "/")
	if repo == "" {
		return "", 0, false
	}
	return repo, issueNum, true
}

func firstLine(s string) string {
	if s == "" {
		return ""
	}
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func (h *Handler) handleAPIEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	events, _, err := h.queryEvents(r)
	if err != nil {
		var bre *badRequestError
		if errors.As(err, &bre) {
			writeError(w, http.StatusBadRequest, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
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

func (h *Handler) handleAPIAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	alerts, err := h.queryAlerts(r)
	if err != nil {
		var bre *badRequestError
		if errors.As(err, &bre) {
			writeError(w, http.StatusBadRequest, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "failed to query alerts")
		}
		return
	}
	writeJSON(w, http.StatusOK, alerts)
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
		currentTask, _ := h.store.WorkerCurrentTaskID(worker.ID)
		resp = append(resp, workerResponse{
			ID:              worker.ID,
			Repo:            worker.Repo,
			Roles:           decodeRoles(worker.Roles),
			Hostname:        worker.Hostname,
			Status:          worker.Status,
			LastHeartbeat:   worker.LastHeartbeat,
			LastHeartbeatAt: worker.LastHeartbeat,
			RegisteredAt:    worker.RegisteredAt,
			CurrentTaskID:   currentTask,
			MgmtBaseURL:     worker.MgmtBaseURL,
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
	cycleState, err := h.store.QueryIssueCycleState(repo, issueNum)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query cycle state")
		return
	}
	if cycleState != nil {
		resp.DevReviewCycleCount = cycleState.DevReviewCycleCount
		resp.CapHit = !cycleState.CapHitAt.IsZero()
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
	// REQ #255: surface pipeline-incompleteness hazards by overriding the
	// DependencyVerdict string. The DependencyState block keeps the real
	// verdict for clients that need both signals.
	if hazard, err := h.store.QueryIssuePipelineHazard(repo, issueNum); err == nil && hazard != nil {
		resp.DependencyVerdict = hazard.Kind
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
			return nil, filter, &badRequestError{"invalid issue query parameter"}
		}
		filter.IssueNum = issueNum
	}

	if sinceStr := strings.TrimSpace(q.Get("since")); sinceStr != "" {
		since, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			return nil, filter, &badRequestError{"invalid since query parameter; use RFC3339"}
		}
		filter.Since = &since
	}

	if untilStr := strings.TrimSpace(q.Get("until")); untilStr != "" {
		until, err := time.Parse(time.RFC3339, untilStr)
		if err != nil {
			return nil, filter, &badRequestError{"invalid until query parameter; use RFC3339"}
		}
		filter.Until = &until
	}

	events, err := h.events.Query(filter)
	if err != nil {
		return nil, filter, fmt.Errorf("failed to query events")
	}
	return events, filter, nil
}

func (h *Handler) queryAlerts(r *http.Request) ([]operator.Alert, error) {
	filter := eventlog.EventFilter{Type: eventlog.TypeAlert}
	if sinceStr := strings.TrimSpace(r.URL.Query().Get("since")); sinceStr != "" {
		since, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			return nil, &badRequestError{"invalid since query parameter; use RFC3339"}
		}
		filter.Since = &since
	}

	severity := strings.TrimSpace(r.URL.Query().Get("severity"))
	if severity != "" && severity != operator.SeverityInfo && severity != operator.SeverityWarn && severity != operator.SeverityError {
		return nil, &badRequestError{"invalid severity query parameter; use info|warn|error"}
	}

	events, err := h.events.Query(filter)
	if err != nil {
		return nil, fmt.Errorf("failed to query alerts")
	}
	alerts := make([]operator.Alert, 0, len(events))
	for _, event := range events {
		var alert operator.Alert
		if err := json.Unmarshal([]byte(event.Payload), &alert); err != nil {
			continue
		}
		if severity != "" && alert.Severity != severity {
			continue
		}
		alerts = append(alerts, alert)
	}
	return alerts, nil
}

func (h *Handler) queryStatus() (statusResponse, error) {
	var resp statusResponse
	active, err := h.store.CountActiveSessions()
	if err != nil {
		return resp, fmt.Errorf("count active sessions: %w", err)
	}
	resp.ActiveSessions = active
	workers, err := h.store.CountWorkers()
	if err != nil {
		return resp, fmt.Errorf("count workers: %w", err)
	}
	resp.Workers = workers
	lastPoll, err := h.store.LastEventTimestampByType(poller.EventPollCycleDone)
	if err != nil {
		return resp, fmt.Errorf("query last poll: %w", err)
	}
	resp.LastPoll = lastPoll

	now := h.now().UTC()
	inFlight, stuck, err := h.summariseInFlight(now)
	if err != nil {
		return resp, fmt.Errorf("summarise in-flight: %w", err)
	}
	resp.InFlightIssues = inFlight
	resp.StuckIssuesOver1H = stuck

	since := now.Add(-24 * time.Hour)
	if done, err := h.store.CountTerminalSessionsSince(store.TaskStatusCompleted, since); err == nil {
		resp.Done24H = done
	}
	if failed, err := h.store.CountTerminalSessionsSince(store.TaskStatusFailed, since); err == nil {
		resp.Failed24H = failed
	}
	return resp, nil
}

// summariseInFlight returns the in-flight issue count and how many of those
// have been stuck longer than the 1-hour threshold (no transition since).
func (h *Handler) summariseInFlight(now time.Time) (int, int, error) {
	caches, err := h.collectInFlightIssues()
	if err != nil {
		return 0, 0, err
	}
	stuck := 0
	for _, ic := range caches {
		last, err := h.store.LatestIssueTransition(ic.Repo, ic.IssueNum)
		if err != nil {
			return 0, 0, err
		}
		if last == nil {
			continue
		}
		if int64(now.Sub(last.At).Seconds()) > stuckThresholdSeconds {
			stuck++
		}
	}
	return len(caches), stuck, nil
}

// collectInFlightIssues returns all open issue caches that are not in a
// terminal status (status:done / status:failed / status:blocked dependent on
// dep state). Used by both the in-flight endpoint and the status summary.
func (h *Handler) collectInFlightIssues() ([]store.IssueCache, error) {
	all, err := h.store.ListIssueCaches("")
	if err != nil {
		return nil, fmt.Errorf("list issue caches: %w", err)
	}
	out := make([]store.IssueCache, 0, len(all))
	for _, ic := range all {
		if !isInFlight(ic) {
			continue
		}
		out = append(out, ic)
	}
	return out, nil
}

func isInFlight(ic store.IssueCache) bool {
	state := strings.ToLower(strings.TrimSpace(ic.State))
	if state == "closed" {
		return false
	}
	labels := decodeLabels(ic.Labels)
	for _, label := range labels {
		switch label {
		case "status:done", "status:failed":
			return false
		}
	}
	return true
}

func currentLabel(labels []string) string {
	for _, label := range labels {
		if strings.HasPrefix(label, "status:") {
			return label
		}
	}
	return ""
}

// labelToState maps "status:developing" -> "developing"; returns "" when the
// label is not a workbuddy state label.
func labelToState(label string) string {
	if strings.HasPrefix(label, "status:") {
		return strings.TrimPrefix(label, "status:")
	}
	return label
}

type sessionListParams struct {
	Repo      string
	AgentName string
	IssueNum  int
	Limit     int
	Offset    int
}

func (h *Handler) listSessions(p sessionListParams) ([]sessionListResponse, error) {
	records, err := h.store.ListSessionsForAPI(store.SessionListFilter{
		Repo:      p.Repo,
		AgentName: p.AgentName,
		IssueNum:  p.IssueNum,
		Limit:     p.Limit,
		Offset:    p.Offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	out := make([]sessionListResponse, 0, len(records))
	seen := make(map[string]struct{}, len(records))
	for i := range records {
		record := records[i]
		seen[record.SessionID] = struct{}{}
		exitCode, _, finishedAt, err := h.sessionTaskStats(record)
		if err != nil {
			return nil, err
		}
		row := sessionListResponse{
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
		}
		if record.TaskID != "" {
			if task, err := h.store.GetTask(record.TaskID); err == nil && task != nil {
				row.TaskStatus = task.Status
				row.Workflow = task.Workflow
				row.CurrentState = task.State
			}
		}
		if row.CurrentState == "" {
			if cache, err := h.store.QueryIssueCache(record.Repo, record.IssueNum); err == nil && cache != nil {
				row.CurrentState = labelToState(currentLabel(decodeLabels(cache.Labels)))
			}
		}
		// Issue #275: flag rows whose events-v1.jsonl artefact never
		// materialized AND the session is already in a terminal-ish state.
		// Running sessions are excluded — they may legitimately not have
		// streamed any events yet.
		if isTerminalSessionStatus(row.TaskStatus) || isTerminalSessionStatus(row.Status) {
			eventsPath := record.Dir
			if eventsPath == "" && h.sessionsDir != "" {
				eventsPath = filepath.Join(h.sessionsDir, record.SessionID)
			}
			if eventsPath != "" {
				eventsPath = filepath.Join(eventsPath, "events-v1.jsonl")
			}
			if eventsPath != "" && !eventsFileHasContent(eventsPath) {
				row.Degraded = true
				row.DegradedReason = degradedReasonNoEventsFile
			}
		}
		out = append(out, row)
	}
	// Issue #275: surface disk-only sessions (metadata.json with no DB row)
	// so the operator can find and drill into them. They are always tagged
	// degraded=no_db_row. Skipped when sessionsDir is unset (tests / split
	// deployments without a sessions directory) or when filters were used —
	// disk-walk filtering would be lossy and surprising.
	if h.sessionsDir != "" && p.Repo == "" && p.AgentName == "" && p.IssueNum == 0 {
		out = append(out, h.listDiskOnlySessions(seen)...)
	}
	return out, nil
}

// listDiskOnlySessions enumerates sessionsDir for per-session metadata.json
// files whose session_id is not present in `seen` (the DB-backed listing).
// Each match becomes a degraded sessionListResponse row. Errors are
// swallowed: a missing/unreadable directory simply returns no extra rows.
func (h *Handler) listDiskOnlySessions(seen map[string]struct{}) []sessionListResponse {
	if h.sessionsDir == "" {
		return nil
	}
	entries, err := os.ReadDir(h.sessionsDir)
	if err != nil {
		return nil
	}
	out := make([]sessionListResponse, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sessionID := entry.Name()
		if !isValidSessionID(sessionID) {
			continue
		}
		if _, ok := seen[sessionID]; ok {
			continue
		}
		meta, ok := readDiskSessionMetadata(filepath.Join(h.sessionsDir, sessionID, "metadata.json"))
		if !ok {
			continue
		}
		// Issue #282: a session whose DB row hasn't been written yet but is
		// actively streaming events is *live*, not aborted. Surface it as a
		// running row so the SPA does not paint a misleading degraded badge.
		eventsPath := filepath.Join(h.sessionsDir, sessionID, "events-v1.jsonl")
		if metaStatusRunning(meta.Status) && eventsFileHasContent(eventsPath) {
			out = append(out, sessionListResponse{
				SessionID:  sessionID,
				TaskID:     meta.TaskID,
				Repo:       meta.Repo,
				IssueNum:   meta.IssueNum,
				AgentName:  meta.AgentName,
				Runtime:    meta.Runtime,
				WorkerID:   meta.WorkerID,
				Attempt:    meta.Attempt,
				Status:     store.TaskStatusRunning,
				TaskStatus: store.TaskStatusRunning,
				ExitCode:   0,
				Duration:   sessionDuration(meta.CreatedAt, nullableMetaTime(meta.ClosedAt)),
				CreatedAt:  meta.CreatedAt,
				FinishedAt: nullableMetaTime(meta.ClosedAt),
				Summary:    meta.Summary,
			})
			continue
		}
		row := sessionListResponse{
			SessionID:      sessionID,
			TaskID:         meta.TaskID,
			Repo:           meta.Repo,
			IssueNum:       meta.IssueNum,
			AgentName:      meta.AgentName,
			Runtime:        meta.Runtime,
			WorkerID:       meta.WorkerID,
			Attempt:        meta.Attempt,
			Status:         StatusAbortedBeforeStart,
			TaskStatus:     StatusAbortedBeforeStart,
			ExitCode:       firstNonZeroExitCode(meta.ExitCode, -1),
			Duration:       sessionDuration(meta.CreatedAt, nullableMetaTime(meta.ClosedAt)),
			CreatedAt:      meta.CreatedAt,
			FinishedAt:     nullableMetaTime(meta.ClosedAt),
			Summary:        meta.Summary,
			Degraded:       true,
			DegradedReason: degradedReasonNoDBRow,
		}
		out = append(out, row)
	}
	return out
}

// firstNonZeroExitCode returns `recorded` when it is non-zero (so an
// explicitly-recorded -1 or other failure code wins) and falls back to
// `fallback` otherwise. Used to default disk-only rows to -1 instead of a
// misleading 0.
func firstNonZeroExitCode(recorded, fallback int) int {
	if recorded != 0 {
		return recorded
	}
	return fallback
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
	resp := sessionDetailResponse{
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
	}
	// Issue #275: even when a DB row exists, mark the session degraded if
	// the events-v1.jsonl artefact never materialized AND the session is
	// already in a non-running terminal-ish state — that combination is the
	// same "looks completed, actually aborted" failure mode operators have
	// been hitting in production. Running/pending sessions are excluded
	// because absence-of-events is normal for them.
	if isTerminalSessionStatus(resp.Status) && !eventsFileHasContent(artifactPaths.EventsV1) {
		resp.Degraded = true
		resp.DegradedReason = degradedReasonNoEventsFile
	}
	return resp, nil
}

// buildDegradedFromDisk reconstructs a sessionDetailResponse from a
// per-session metadata.json on disk when the agent_sessions DB row is
// missing. Returns ok=false (so the caller can return 404) when the on-disk
// metadata is unreadable or unusable. Issue #275.
func (h *Handler) buildDegradedFromDisk(sessionID string) (sessionDetailResponse, bool) {
	if h.sessionsDir == "" {
		return sessionDetailResponse{}, false
	}
	dir := filepath.Join(h.sessionsDir, sessionID)
	meta, ok := readDiskSessionMetadata(filepath.Join(dir, "metadata.json"))
	if !ok {
		return sessionDetailResponse{}, false
	}
	stdoutPath := meta.StdoutPath
	if stdoutPath == "" {
		stdoutPath = filepath.Join(dir, "stdout")
	}
	stderrPath := meta.StderrPath
	if stderrPath == "" {
		stderrPath = filepath.Join(dir, "stderr")
	}
	artifactPaths := SessionArtifactPaths{
		SessionDir: dir,
		EventsV1:   filepath.Join(dir, "events-v1.jsonl"),
	}
	stderrSummary := readArtifactSummary(stderrPath, 4096)
	// Issue #282: a session whose DB row hasn't been written yet but is
	// actively streaming events to events-v1.jsonl is still live. Surface
	// it as running/non-degraded so the SPA renders the timeline normally
	// instead of the red "aborted_before_start" warning card.
	if metaStatusRunning(meta.Status) && eventsFileHasContent(artifactPaths.EventsV1) {
		return sessionDetailResponse{
			SessionID:     sessionID,
			TaskID:        meta.TaskID,
			Repo:          meta.Repo,
			IssueNum:      meta.IssueNum,
			AgentName:     meta.AgentName,
			Runtime:       meta.Runtime,
			WorkerID:      meta.WorkerID,
			Attempt:       meta.Attempt,
			Status:        store.TaskStatusRunning,
			ExitCode:      0,
			Duration:      sessionDuration(meta.CreatedAt, nullableMetaTime(meta.ClosedAt)),
			CreatedAt:     meta.CreatedAt,
			FinishedAt:    nullableMetaTime(meta.ClosedAt),
			Summary:       strings.TrimSpace(meta.Summary),
			StdoutSummary: readArtifactSummary(stdoutPath, 4096),
			StderrSummary: stderrSummary,
			ArtifactPaths: artifactPaths,
		}, true
	}
	summary := strings.TrimSpace(meta.Summary)
	if summary == "" {
		summary = synthesizeDegradedSummary(meta, stderrSummary)
	}
	exitCode := meta.ExitCode
	// When SessionManager.Create wrote metadata.json but Close() never ran,
	// status remains "running" and exit_code defaults to zero. Surface a
	// distinct -1 placeholder so the UI does not display a misleading
	// "exit 0 / completed" pair.
	if exitCode == 0 && !isTerminalSessionStatus(meta.Status) {
		exitCode = -1
	}
	finishedAt := nullableMetaTime(meta.ClosedAt)
	resp := sessionDetailResponse{
		SessionID:      sessionID,
		TaskID:         meta.TaskID,
		Repo:           meta.Repo,
		IssueNum:       meta.IssueNum,
		AgentName:      meta.AgentName,
		Runtime:        meta.Runtime,
		WorkerID:       meta.WorkerID,
		Attempt:        meta.Attempt,
		Status:         StatusAbortedBeforeStart,
		ExitCode:       exitCode,
		Duration:       sessionDuration(meta.CreatedAt, finishedAt),
		CreatedAt:      meta.CreatedAt,
		FinishedAt:     finishedAt,
		Summary:        summary,
		StdoutSummary:  readArtifactSummary(stdoutPath, 4096),
		StderrSummary:  stderrSummary,
		ArtifactPaths:  artifactPaths,
		Degraded:       true,
		DegradedReason: degradedReasonNoDBRow,
	}
	return resp, true
}

// diskSessionMetadata mirrors the runtime SessionManager's metadata.json
// schema. Defined locally to avoid an import cycle with internal/runtime
// and to insulate the API from future schema changes.
type diskSessionMetadata struct {
	SessionID  string    `json:"session_id"`
	TaskID     string    `json:"task_id,omitempty"`
	Repo       string    `json:"repo"`
	IssueNum   int       `json:"issue_num"`
	AgentName  string    `json:"agent_name"`
	Runtime    string    `json:"runtime,omitempty"`
	WorkerID   string    `json:"worker_id,omitempty"`
	Attempt    int       `json:"attempt"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	ClosedAt   time.Time `json:"closed_at,omitempty"`
	StdoutPath string    `json:"stdout_path,omitempty"`
	StderrPath string    `json:"stderr_path,omitempty"`
	// ExitCode and Summary are not part of the canonical metadata schema
	// today, but the fields are reserved here so writers (the supervisor /
	// session manager) can populate them in the future and the API will
	// surface them automatically.
	ExitCode int    `json:"exit_code,omitempty"`
	Summary  string `json:"summary,omitempty"`
}

func readDiskSessionMetadata(path string) (diskSessionMetadata, bool) {
	if path == "" {
		return diskSessionMetadata{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return diskSessionMetadata{}, false
	}
	var meta diskSessionMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return diskSessionMetadata{}, false
	}
	if meta.SessionID == "" && meta.Repo == "" && meta.AgentName == "" {
		return diskSessionMetadata{}, false
	}
	return meta, true
}

func nullableMetaTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	ts := t.UTC()
	return &ts
}

// synthesizeDegradedSummary builds a markdown summary mirroring the shape
// produced by audit.buildMinimalSummary so the SPA's warning card has
// something useful to render even when the metadata.json itself does not
// carry an explicit summary field.
func synthesizeDegradedSummary(meta diskSessionMetadata, stderrExcerpt string) string {
	var b strings.Builder
	b.WriteString("## Session Summary (no session file)\n\n")
	b.WriteString("- DB row: missing — response synthesized from metadata.json\n")
	if meta.Status != "" {
		fmt.Fprintf(&b, "- Recorded status: %s\n", meta.Status)
	}
	if meta.ExitCode != 0 {
		fmt.Fprintf(&b, "- Exit code: %d\n", meta.ExitCode)
	} else {
		b.WriteString("- Exit code: -1 (unknown — runtime aborted before reporting)\n")
	}
	if !meta.CreatedAt.IsZero() {
		fmt.Fprintf(&b, "- Created at: %s\n", meta.CreatedAt.UTC().Format(time.RFC3339))
	}
	if !meta.ClosedAt.IsZero() {
		fmt.Fprintf(&b, "- Closed at: %s\n", meta.ClosedAt.UTC().Format(time.RFC3339))
	}
	if stderrExcerpt != "" {
		b.WriteString("\n### Stderr (excerpt)\n```\n")
		b.WriteString(stderrExcerpt)
		b.WriteString("\n```\n")
	}
	return b.String()
}

func isTerminalSessionStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case store.TaskStatusCompleted, store.TaskStatusFailed, store.TaskStatusTimeout, StatusAbortedBeforeStart:
		return true
	}
	return false
}

// metaStatusRunning reports whether a metadata.json status field marks the
// session as still in flight. Issue #282: used together with
// eventsFileHasContent to distinguish a live no-DB-row session from a real
// aborted-before-start one.
func metaStatusRunning(status string) bool {
	return strings.ToLower(strings.TrimSpace(status)) == store.TaskStatusRunning
}

// eventsFileHasContent reports whether the events-v1.jsonl artefact at
// `path` exists and contains at least one byte. Missing files return false;
// any other I/O error is treated as "no usable content" so the caller can
// degrade gracefully.
func eventsFileHasContent(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return false
	}
	return info.Size() > 0
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
	agg, err := h.store.AggregateSessionMetrics()
	if err != nil {
		return resp, fmt.Errorf("aggregate metrics: %w", err)
	}
	if agg.Total > 0 {
		resp.SuccessRate = float64(agg.Successful) / float64(agg.Total)
		resp.RetryRate = float64(agg.Retried) / float64(agg.Total)
	}
	if agg.AvgDuration.Valid {
		resp.AvgDuration = agg.AvgDuration.Float64
	}

	perAgent, err := h.store.CountSessionsByAgent()
	if err != nil {
		return resp, fmt.Errorf("aggregate per-agent counts: %w", err)
	}
	for _, row := range perAgent {
		resp.AgentExecutions[row.AgentName] = row.Count
	}
	return resp, nil
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
