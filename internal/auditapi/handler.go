// Package auditapi serves the read-only HTTP audit API for external tooling.
package auditapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/ghadapter"
	"github.com/Lincyaw/workbuddy/internal/operator"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/sessionref"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// stuckThresholdSeconds is the elapsed-since-last-transition threshold beyond
// which an issue is reported as `stuck` on /api/v1/status. Mirrors the audit
// HTTP handler's 1-hour rule.
const stuckThresholdSeconds = int64(3600)

// degradedReason values populate the SessionDetail.DegradedReason field.
//
// Phase 3 of the session-data ownership refactor (REQ-122) deleted the
// disk-only synthesis path. Two reasons remain:
//
//   - no_events_file: the worker's DB row exists and the session reached a
//     terminal status, but events-v1.jsonl never got any content. Real
//     signal — the agent crashed before producing any tool calls.
//   - worker_offline: emitted by the coordinator's sessionproxy (NOT this
//     handler) when the owning worker can't be dialled. Surfaced here only
//     in comments and the SPA copy.
//
// The legacy `no_db_row` reason and its `aborted_before_start` synthesized
// status were removed: with worker-owned session data, a missing DB row
// is a real 404, not a "we lost the row but kept the disk artefact"
// degraded shape.
const (
	degradedReasonNoEventsFile = "no_events_file"
)

// badRequestError wraps parameter-validation errors so callers can distinguish
// them from backend/DB errors returned by queryEvents.
type badRequestError struct {
	msg string
}

func (e *badRequestError) Error() string { return e.msg }

// IssueSessionsLister returns the sessions for a single repo+issue. It's
// the seam the coordinator uses to fan out across worker audit_urls
// instead of reading from its own (now-dropped) sessions table. The
// worker-side audit handler does NOT set this — it falls back to the
// store's local ListSessions, which is authoritative on the worker DB.
type IssueSessionsLister interface {
	ListSessionsForRepoIssue(ctx context.Context, repo string, issueNum int) ([]map[string]any, []string, error)
}

// Handler serves the JSON audit API.
type Handler struct {
	store               *store.Store
	events              *eventlog.EventLogger
	sessionsDir         string
	reportBaseURL       string
	now                 func() time.Time
	sessionEventsFn     http.HandlerFunc
	sessionStreamFn     http.HandlerFunc
	gh                  rolloutPRReader
	prDiffTTL           time.Duration
	prDiffMu            sync.Mutex
	prDiffCache         map[string]cachedPRDiff
	issueSessionsLister IssueSessionsLister
}

type rolloutPRReader interface {
	FindPullRequestByBranch(ctx context.Context, repo, branch string) (ghadapter.PullRequest, error)
	DiffPullRequest(ctx context.Context, repo string, prNum int) (string, error)
}

func hasRolloutMetadata(index, total int, groupID string) bool {
	if strings.TrimSpace(groupID) != "" {
		return true
	}
	if index > 0 {
		return true
	}
	return total > 1
}

type cachedPRDiff struct {
	body      string
	expiresAt time.Time
}

// NewHandler constructs a Handler backed by the given store.
func NewHandler(st *store.Store) *Handler {
	return &Handler{
		store:       st,
		events:      eventlog.NewEventLogger(st),
		now:         time.Now,
		gh:          ghadapter.NewCLI(),
		prDiffTTL:   30 * time.Second,
		prDiffCache: make(map[string]cachedPRDiff),
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

func (h *Handler) SetRolloutPRReader(reader rolloutPRReader) {
	if reader == nil {
		h.gh = ghadapter.NewCLI()
		return
	}
	h.gh = reader
}

// SetIssueSessionsLister injects the source for issue-detail's session
// list. Coordinators wire this to internal/sessionproxy.Handler so the
// detail endpoint fans out to live workers instead of reading from the
// coordinator's own (Phase 3-dropped) sessions table. Pass nil to
// restore the local-store fallback.
func (h *Handler) SetIssueSessionsLister(lister IssueSessionsLister) {
	h.issueSessionsLister = lister
}

func (h *Handler) SetPRDiffTTL(ttl time.Duration) {
	if ttl <= 0 {
		h.prDiffTTL = 30 * time.Second
		return
	}
	h.prDiffTTL = ttl
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

// RegisterAPISessions mounts only the /api/v1/sessions and
// /api/v1/sessions/{id...} JSON endpoints. Used by worker-side audit
// servers that expose just the session subset of the dashboard API
// (Phase 1 of the session-data ownership refactor).
func (h *Handler) RegisterAPISessions(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/sessions", h.handleAPISessions)
	mux.HandleFunc("/api/v1/sessions/", h.handleAPISession)
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
	SessionID      string     `json:"session_id"`
	TaskID         string     `json:"task_id,omitempty"`
	Repo           string     `json:"repo"`
	IssueNum       int        `json:"issue_num"`
	AgentName      string     `json:"agent_name"`
	Runtime        string     `json:"runtime,omitempty"`
	WorkerID       string     `json:"worker_id,omitempty"`
	Attempt        int        `json:"attempt"`
	Status         string     `json:"status"`
	TaskStatus     string     `json:"task_status,omitempty"`
	Workflow       string     `json:"workflow,omitempty"`
	CurrentState   string     `json:"current_state,omitempty"`
	RolloutIndex   int        `json:"rollout_index,omitempty"`
	RolloutsTotal  int        `json:"rollouts_total,omitempty"`
	RolloutGroupID string     `json:"rollout_group_id,omitempty"`
	ExitCode       int        `json:"exit_code"`
	Duration       int64      `json:"duration"`
	CreatedAt      time.Time  `json:"created_at"`
	FinishedAt     *time.Time `json:"finished_at"`
	Summary        string     `json:"summary,omitempty"`
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
	SessionID      string     `json:"session_id"`
	Agent          string     `json:"agent"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	Status         string     `json:"status,omitempty"`
	ExitCode       int        `json:"exit_code"`
	WorkerID       string     `json:"worker_id,omitempty"`
	TaskID         string     `json:"task_id,omitempty"`
	RolloutIndex   int        `json:"rollout_index,omitempty"`
	RolloutsTotal  int        `json:"rollouts_total,omitempty"`
	RolloutGroupID string     `json:"rollout_group_id,omitempty"`
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

type rolloutGroupResponse struct {
	GroupID       string                       `json:"group_id"`
	RolloutsTotal int                          `json:"rollouts_total"`
	Members       []rolloutMemberResponse      `json:"members"`
	SynthOutcome  *rolloutSynthOutcomeResponse `json:"synth_outcome,omitempty"`
}

type rolloutMemberResponse struct {
	RolloutIndex    int        `json:"rollout_index"`
	PRNumber        int        `json:"pr_number"`
	Status          string     `json:"status"`
	SessionID       string     `json:"session_id,omitempty"`
	WorkerID        string     `json:"worker_id,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	DurationSeconds int64      `json:"duration_seconds"`
}

type rolloutSynthOutcomeResponse struct {
	Decision      string    `json:"decision"`
	ChosenPR      int       `json:"chosen_pr,omitempty"`
	SynthPR       int       `json:"synth_pr,omitempty"`
	ChosenRollout int       `json:"chosen_rollout_index,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	TS            time.Time `json:"ts"`
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
		// Phase 3 (REQ-122) deleted the disk-only synthesis fallback. The
		// owning worker is authoritative for session existence; a missing
		// row is a real 404. Coordinator callers reach this handler only
		// via the sessionproxy local-fallback branch (legacy pre-bundle
		// rows on the coordinator host), where 404 is also the right
		// answer when the row is gone.
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
	repo, issueNum, suffix, ok := splitRepoIssueSuffix(rest)
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
	switch suffix {
	case "":
		resp, err := h.buildIssueDetailResponse(*cache)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	case "rollouts":
		resp, err := h.buildRolloutGroupResponse(repo, issueNum)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	default:
		if strings.HasPrefix(suffix, "rollouts/") && strings.HasSuffix(suffix, "/diff") {
			indexRaw := strings.TrimSuffix(strings.TrimPrefix(suffix, "rollouts/"), "/diff")
			rolloutIndex, err := strconv.Atoi(strings.Trim(indexRaw, "/"))
			if err != nil || rolloutIndex <= 0 {
				writeError(w, http.StatusBadRequest, "invalid rollout index")
				return
			}
			h.serveRolloutDiff(w, r, repo, issueNum, rolloutIndex)
			return
		}
		writeError(w, http.StatusNotFound, "not found")
		return
	}
}

func (h *Handler) buildIssueDetailResponse(cache store.IssueCache) (issueDetailResponse, error) {
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
	transitions, err := h.store.QueryIssueTransitions(cache.Repo, cache.IssueNum)
	if err != nil {
		return issueDetailResponse{}, fmt.Errorf("failed to query transitions")
	}
	for _, t := range transitions {
		resp.Transitions = append(resp.Transitions, issueTransitionResponse{
			From: t.From,
			To:   t.To,
			At:   t.At,
			By:   t.By,
		})
	}
	counts, err := h.store.QueryTransitionCounts(cache.Repo, cache.IssueNum)
	if err != nil {
		return issueDetailResponse{}, fmt.Errorf("failed to query transition counts")
	}
	for _, c := range counts {
		resp.TransitionCounts = append(resp.TransitionCounts, transitionCountResponse{
			FromState: c.FromState,
			ToState:   c.ToState,
			Count:     c.Count,
		})
	}
	tasks, err := h.store.ListTasksForIssue(cache.Repo, cache.IssueNum)
	if err != nil {
		return issueDetailResponse{}, fmt.Errorf("failed to list tasks: %w", err)
	}
	taskBySession, taskByID := indexIssueTasks(tasks)

	sessionRows, sessionsErr := h.collectIssueSessions(cache.Repo, cache.IssueNum, taskBySession, taskByID)
	if sessionsErr != nil {
		return issueDetailResponse{}, sessionsErr
	}
	resp.Sessions = append(resp.Sessions, sessionRows...)
	return resp, nil
}

// collectIssueSessions resolves the per-issue session list. When an
// issueSessionsLister is wired (coordinator path, Phase 3) it fans out
// across workers via internal/sessionproxy; otherwise (worker path or
// tests with a local DB) it reads from the local store directly. Both
// branches converge on the same issueSessionRefResponse shape.
func (h *Handler) collectIssueSessions(
	repo string,
	issueNum int,
	taskBySession map[string]*store.TaskRecord,
	taskByID map[string]*store.TaskRecord,
) ([]issueSessionRefResponse, error) {
	if h.issueSessionsLister != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rows, _, err := h.issueSessionsLister.ListSessionsForRepoIssue(ctx, repo, issueNum)
		if err != nil {
			return nil, fmt.Errorf("failed to list sessions: %w", err)
		}
		out := make([]issueSessionRefResponse, 0, len(rows))
		for _, row := range rows {
			ref := issueSessionRefFromRow(row)
			fillRolloutFromTask(&ref, taskBySession, taskByID)
			out = append(out, ref)
		}
		return out, nil
	}
	sessions, err := h.store.ListSessions(store.SessionFilter{Repo: repo, IssueNum: issueNum})
	if err != nil {
		return nil, fmt.Errorf("failed to list sessions")
	}
	out := make([]issueSessionRefResponse, 0, len(sessions))
	for _, sess := range sessions {
		exitCode, _, finishedAt, _ := h.sessionTaskStats(sess)
		ref := issueSessionRefResponse{
			SessionID:  sess.SessionID,
			Agent:      sess.AgentName,
			StartedAt:  sess.CreatedAt,
			FinishedAt: finishedAt,
			Status:     sess.Status,
			ExitCode:   exitCode,
			WorkerID:   sess.WorkerID,
			TaskID:     sess.TaskID,
		}
		fillRolloutFromTask(&ref, taskBySession, taskByID)
		out = append(out, ref)
	}
	return out, nil
}

// issueSessionRefFromRow translates a sessionproxy row (raw map[string]any
// shaped by the worker's session list response) into the typed
// issueSessionRefResponse the issue-detail endpoint serves.
func issueSessionRefFromRow(row map[string]any) issueSessionRefResponse {
	ref := issueSessionRefResponse{
		SessionID: stringField(row, "session_id"),
		Agent:     stringField(row, "agent_name"),
		Status:    stringField(row, "status"),
		WorkerID:  stringField(row, "worker_id"),
		TaskID:    stringField(row, "task_id"),
		ExitCode:  intField(row, "exit_code"),
	}
	if started, ok := timeField(row, "created_at"); ok {
		ref.StartedAt = started
	}
	if finished, ok := timeField(row, "finished_at"); ok && !finished.IsZero() {
		t := finished
		ref.FinishedAt = &t
	}
	return ref
}

func fillRolloutFromTask(ref *issueSessionRefResponse, taskBySession, taskByID map[string]*store.TaskRecord) {
	var task *store.TaskRecord
	if t := taskBySession[ref.SessionID]; t != nil {
		task = t
	} else if t := taskByID[ref.TaskID]; t != nil {
		task = t
	}
	if task == nil {
		return
	}
	if hasRolloutMetadata(task.RolloutIndex, task.RolloutsTotal, task.RolloutGroupID) {
		ref.RolloutIndex = task.RolloutIndex
		ref.RolloutsTotal = task.RolloutsTotal
		ref.RolloutGroupID = task.RolloutGroupID
	}
	if ref.WorkerID == "" {
		ref.WorkerID = task.WorkerID
	}
}

func stringField(row map[string]any, key string) string {
	if v, ok := row[key].(string); ok {
		return v
	}
	return ""
}

func intField(row map[string]any, key string) int {
	switch v := row[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func timeField(row map[string]any, key string) (time.Time, bool) {
	raw, ok := row[key].(string)
	if !ok || raw == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, true
	}
	return time.Time{}, false
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

func splitRepoIssueSuffix(trimmed string) (string, int, string, bool) {
	base := trimmed
	suffix := ""
	if idx := strings.Index(trimmed, "/rollouts"); idx >= 0 {
		base = trimmed[:idx]
		suffix = strings.TrimPrefix(trimmed[idx:], "/")
	}
	repo, issueNum, ok := splitRepoIssue(base)
	return repo, issueNum, suffix, ok
}

func (h *Handler) buildRolloutGroupResponse(repo string, issueNum int) (rolloutGroupResponse, error) {
	resp := rolloutGroupResponse{Members: []rolloutMemberResponse{}}
	tasks, err := h.store.ListTasksForIssue(repo, issueNum)
	if err != nil {
		return resp, fmt.Errorf("failed to list issue tasks: %w", err)
	}
	groupID := latestRolloutGroupID(tasks)
	if groupID == "" {
		return resp, nil
	}
	groupTasks, err := h.store.ListTasksByRolloutGroup(groupID)
	if err != nil {
		return resp, fmt.Errorf("failed to list rollout group tasks: %w", err)
	}
	if len(groupTasks) == 0 {
		return resp, nil
	}

	resp.GroupID = groupID
	resp.RolloutsTotal = maxRolloutsTotal(groupTasks)

	latestByRollout := make(map[int]store.TaskRecord)
	for _, task := range groupTasks {
		if task.RolloutIndex <= 0 {
			continue
		}
		prev, ok := latestByRollout[task.RolloutIndex]
		if !ok || task.CreatedAt.After(prev.CreatedAt) || (task.CreatedAt.Equal(prev.CreatedAt) && task.UpdatedAt.After(prev.UpdatedAt)) {
			latestByRollout[task.RolloutIndex] = task
		}
	}

	prByRollout := make(map[int]int, len(latestByRollout))
	for rolloutIndex := range latestByRollout {
		pr, err := h.findRolloutPullRequest(context.Background(), repo, issueNum, rolloutIndex)
		if err != nil {
			if !errors.Is(err, ghadapter.ErrPullRequestNotFound) {
				return resp, fmt.Errorf("failed to look up rollout PR %d: %w", rolloutIndex, err)
			}
			continue
		}
		prByRollout[rolloutIndex] = pr.Number
	}

	indices := make([]int, 0, len(latestByRollout))
	for idx := range latestByRollout {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	for _, rolloutIndex := range indices {
		task := latestByRollout[rolloutIndex]
		sessionID := latestSessionRef(task.SessionRefs)
		startedAt := task.CreatedAt
		endedAt := timePtr(task.CompletedAt)
		durationSeconds := durationSeconds(startedAt, endedAt, h.now())
		resp.Members = append(resp.Members, rolloutMemberResponse{
			RolloutIndex:    rolloutIndex,
			PRNumber:        prByRollout[rolloutIndex],
			Status:          task.Status,
			SessionID:       sessionID,
			WorkerID:        task.WorkerID,
			StartedAt:       startedAt,
			EndedAt:         endedAt,
			DurationSeconds: durationSeconds,
		})
	}

	if outcome, err := h.latestSynthOutcome(repo, issueNum, groupID, prByRollout); err != nil {
		return resp, fmt.Errorf("failed to build synth outcome: %w", err)
	} else {
		resp.SynthOutcome = outcome
	}
	return resp, nil
}

func (h *Handler) serveRolloutDiff(w http.ResponseWriter, r *http.Request, repo string, issueNum, rolloutIndex int) {
	pr, err := h.findRolloutPullRequest(r.Context(), repo, issueNum, rolloutIndex)
	if err != nil {
		if errors.Is(err, ghadapter.ErrPullRequestNotFound) {
			writeError(w, http.StatusNotFound, "rollout PR not found")
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	diff, err := h.cachedPullRequestDiff(r.Context(), repo, pr.Number)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(diff))
}

func (h *Handler) cachedPullRequestDiff(ctx context.Context, repo string, prNum int) (string, error) {
	cacheKey := fmt.Sprintf("%s#%d", repo, prNum)
	now := h.now()

	h.prDiffMu.Lock()
	if cached, ok := h.prDiffCache[cacheKey]; ok && now.Before(cached.expiresAt) {
		h.prDiffMu.Unlock()
		return cached.body, nil
	}
	h.prDiffMu.Unlock()

	diff, err := h.gh.DiffPullRequest(ctx, repo, prNum)
	if err != nil {
		return "", err
	}

	h.prDiffMu.Lock()
	h.prDiffCache[cacheKey] = cachedPRDiff{
		body:      diff,
		expiresAt: now.Add(h.prDiffTTL),
	}
	h.prDiffMu.Unlock()
	return diff, nil
}

func (h *Handler) findRolloutPullRequest(ctx context.Context, repo string, issueNum, rolloutIndex int) (ghadapter.PullRequest, error) {
	branch := fmt.Sprintf("workbuddy/issue-%d/rollout-%d", issueNum, rolloutIndex)
	return h.gh.FindPullRequestByBranch(ctx, repo, branch)
}

func (h *Handler) latestSynthOutcome(repo string, issueNum int, groupID string, prByRollout map[int]int) (*rolloutSynthOutcomeResponse, error) {
	events, err := h.events.Query(eventlog.EventFilter{Repo: repo, IssueNum: issueNum, Type: "synthesis_decision"})
	if err != nil {
		return nil, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		outcome, ok := parseSynthOutcomeEvent(events[i], groupID, prByRollout)
		if ok {
			return outcome, nil
		}
	}
	return nil, nil
}

func parseSynthOutcomeEvent(ev store.Event, groupID string, prByRollout map[int]int) (*rolloutSynthOutcomeResponse, bool) {
	payload, ok := decodeJSONOrString(ev.Payload).(map[string]any)
	if !ok {
		return nil, false
	}
	if gid := stringValue(payload["group_id"]); gid != "" && groupID != "" && gid != groupID {
		return nil, false
	}
	outcome := &rolloutSynthOutcomeResponse{
		Decision:      firstNonEmptyString(payload["decision"], payload["outcome"]),
		ChosenPR:      intValue(payload["chosen_pr"]),
		SynthPR:       intValue(payload["synth_pr"]),
		ChosenRollout: intValue(payload["chosen_rollout_index"]),
		Reason:        stringValue(payload["reason"]),
		TS:            ev.TS,
	}
	if outcome.TS.IsZero() {
		outcome.TS = time.Now().UTC()
	}
	if ts := timeValue(payload["ts"]); !ts.IsZero() {
		outcome.TS = ts
	}
	if outcome.ChosenRollout == 0 {
		outcome.ChosenRollout = intValue(payload["rollout_index"])
	}
	if outcome.ChosenPR == 0 && outcome.ChosenRollout > 0 {
		outcome.ChosenPR = prByRollout[outcome.ChosenRollout]
	}
	if outcome.Decision == "" && outcome.Reason == "" && outcome.ChosenPR == 0 && outcome.SynthPR == 0 {
		return nil, false
	}
	return outcome, true
}

func firstNonEmptyString(values ...any) string {
	for _, value := range values {
		if s := stringValue(value); s != "" {
			return s
		}
	}
	return ""
}

func indexIssueTasks(tasks []store.TaskRecord) (map[string]*store.TaskRecord, map[string]*store.TaskRecord) {
	bySession := make(map[string]*store.TaskRecord)
	byID := make(map[string]*store.TaskRecord)
	for i := range tasks {
		task := &tasks[i]
		byID[task.ID] = task
		for _, sessionID := range decodeSessionRefs(task.SessionRefs) {
			if sessionID == "" {
				continue
			}
			bySession[sessionID] = task
		}
	}
	return bySession, byID
}

func decodeSessionRefs(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var refs []string
	if err := json.Unmarshal([]byte(raw), &refs); err != nil {
		return nil
	}
	return refs
}

func latestSessionRef(raw string) string {
	refs := decodeSessionRefs(raw)
	if len(refs) == 0 {
		return ""
	}
	return refs[len(refs)-1]
}

func latestRolloutGroupID(tasks []store.TaskRecord) string {
	var chosen store.TaskRecord
	found := false
	for _, task := range tasks {
		if strings.TrimSpace(task.RolloutGroupID) == "" {
			continue
		}
		if !found || task.CreatedAt.After(chosen.CreatedAt) || (task.CreatedAt.Equal(chosen.CreatedAt) && task.UpdatedAt.After(chosen.UpdatedAt)) {
			chosen = task
			found = true
		}
	}
	if !found {
		return ""
	}
	return chosen.RolloutGroupID
}

func maxRolloutsTotal(tasks []store.TaskRecord) int {
	max := 0
	for _, task := range tasks {
		if task.RolloutsTotal > max {
			max = task.RolloutsTotal
		}
	}
	return max
}

func timePtr(ts time.Time) *time.Time {
	if ts.IsZero() {
		return nil
	}
	cpy := ts
	return &cpy
}

func durationSeconds(start time.Time, end *time.Time, now time.Time) int64 {
	finish := now
	if end != nil {
		finish = *end
	}
	if finish.Before(start) {
		return 0
	}
	return int64(finish.Sub(start).Seconds())
}

func stringValue(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	default:
		return ""
	}
}

func intValue(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(t))
		return n
	default:
		return 0
	}
}

func timeValue(v any) time.Time {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return ts
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
	for i := range records {
		record := records[i]
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
				if hasRolloutMetadata(task.RolloutIndex, task.RolloutsTotal, task.RolloutGroupID) {
					row.RolloutIndex = task.RolloutIndex
					row.RolloutsTotal = task.RolloutsTotal
					row.RolloutGroupID = task.RolloutGroupID
				}
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
	// Phase 3 (REQ-122) deleted the disk-only listDiskOnlySessions branch.
	// Sessions live on the worker that produced them; the coordinator's
	// sessionproxy fans out to every worker for this repo, and each worker
	// reads from its own DB authoritatively.
	return out, nil
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

// Phase 3 (REQ-122) deleted buildDegradedFromDisk, readDiskSessionMetadata,
// synthesizeDegradedSummary, metaStatusRunning, nullableMetaTime,
// firstNonZeroExitCode and the diskSessionMetadata struct. They reconstructed
// a synthetic "aborted_before_start" response from on-disk metadata.json when
// the DB row was missing. With worker-owned session data, the worker is
// authoritative for session existence; a missing row is a real 404.

func isTerminalSessionStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case store.TaskStatusCompleted, store.TaskStatusFailed, store.TaskStatusTimeout:
		return true
	}
	return false
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
		// Worker DBs don't keep terminal task_queue rows around (the row
		// is acked + deleted on completion), so a missing task is the
		// normal case for finished sessions on the worker-side audit
		// listener. Treat ErrTaskNotFound like the task==nil branch
		// rather than bubbling it up as a 500.
		if errors.Is(err, store.ErrTaskNotFound) {
			return exitCode, status, finishedAt, nil
		}
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
