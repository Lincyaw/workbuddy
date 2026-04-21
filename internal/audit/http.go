package audit

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/store"
)

const stuckThreshold = time.Hour

type HTTPHandler struct {
	store *store.Store
	now   func() time.Time
}

type EventEnvelope struct {
	ID       int64           `json:"id"`
	TS       time.Time       `json:"ts"`
	Type     string          `json:"type"`
	Repo     string          `json:"repo"`
	IssueNum int             `json:"issue_num"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

type EventsResponse struct {
	Events []EventEnvelope `json:"events"`
}

type IssueStateResponse struct {
	Repo              string     `json:"repo"`
	IssueNum          int        `json:"issue_num"`
	IssueState        string     `json:"issue_state"`
	CurrentState      string     `json:"current_state"`
	CycleCount        int        `json:"cycle_count"`
	DependencyVerdict string     `json:"dependency_verdict"`
	LastEventAt       *time.Time `json:"last_event_at,omitempty"`
	Stuck             bool       `json:"stuck"`
}

func NewHTTPHandler(st *store.Store) *HTTPHandler {
	return &HTTPHandler{
		store: st,
		now:   time.Now,
	}
}

func (h *HTTPHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/events", h.handleEvents)
	mux.HandleFunc("/issues/", h.handleIssueState)
	mux.HandleFunc("/tasks", h.handleTasks)
}

func (h *HTTPHandler) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	filter, err := parseEventFilter(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	events, err := eventlog.NewEventLogger(h.store).Query(filter)
	if err != nil {
		http.Error(w, fmt.Sprintf("query events: %v", err), http.StatusInternalServerError)
		return
	}

	resp := EventsResponse{Events: make([]EventEnvelope, 0, len(events))}
	for _, ev := range events {
		resp.Events = append(resp.Events, EventEnvelope{
			ID:       ev.ID,
			TS:       ev.TS.UTC(),
			Type:     ev.Type,
			Repo:     ev.Repo,
			IssueNum: ev.IssueNum,
			Payload:  payloadJSON(ev.Payload),
		})
	}
	writeJSONResponse(w, http.StatusOK, resp)
}

func (h *HTTPHandler) handleIssueState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	repo, issueNum, ok := parseIssueStatePath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	state, err := h.queryIssueState(repo, issueNum)
	if err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
			return
		}
		http.Error(w, fmt.Sprintf("query issue state: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSONResponse(w, http.StatusOK, state)
}

func (h *HTTPHandler) handleTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	filter := store.TaskFilter{
		Repo:   strings.TrimSpace(r.URL.Query().Get("repo")),
		Status: strings.TrimSpace(r.URL.Query().Get("status")),
	}
	tasks, err := h.store.QueryTasksFiltered(filter)
	if err != nil {
		http.Error(w, fmt.Sprintf("query tasks: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSONResponse(w, http.StatusOK, tasks)
}

func (h *HTTPHandler) queryIssueState(repo string, issueNum int) (IssueStateResponse, error) {
	cache, err := h.store.QueryIssueCache(repo, issueNum)
	if err != nil {
		return IssueStateResponse{}, err
	}
	if cache == nil {
		return IssueStateResponse{}, sql.ErrNoRows
	}

	counts, err := h.store.QueryTransitionCounts(repo, issueNum)
	if err != nil {
		return IssueStateResponse{}, err
	}
	depState, err := h.store.QueryIssueDependencyState(repo, issueNum)
	if err != nil {
		return IssueStateResponse{}, err
	}
	lastEventAt, err := h.latestIssueEvent(repo, issueNum)
	if err != nil {
		return IssueStateResponse{}, err
	}

	currentState := inferCurrentState(cache.Labels, cache.State)
	dependencyVerdict := store.DependencyVerdictReady
	if depState != nil && depState.Verdict != "" {
		dependencyVerdict = depState.Verdict
	}

	resp := IssueStateResponse{
		Repo:              repo,
		IssueNum:          issueNum,
		IssueState:        cache.State,
		CurrentState:      currentState,
		CycleCount:        maxTransitionCount(counts),
		DependencyVerdict: dependencyVerdict,
		LastEventAt:       lastEventAt,
	}
	if lastEventAt != nil {
		resp.Stuck = cache.State == "open" && isIntermediateState(currentState) && h.now().Sub(*lastEventAt) > stuckThreshold
	}
	return resp, nil
}

func (h *HTTPHandler) latestIssueEvent(repo string, issueNum int) (*time.Time, error) {
	return h.store.LatestEventAt(repo, issueNum)
}

func parseEventFilter(values url.Values) (eventlog.EventFilter, error) {
	var filter eventlog.EventFilter
	filter.Repo = strings.TrimSpace(values.Get("repo"))
	filter.Type = strings.TrimSpace(values.Get("type"))

	rawIssue := strings.TrimSpace(values.Get("issue"))
	if rawIssue == "" {
		rawIssue = strings.TrimSpace(values.Get("issue_num"))
	}
	if raw := rawIssue; raw != "" {
		issueNum, err := strconv.Atoi(raw)
		if err != nil || issueNum <= 0 {
			return filter, fmt.Errorf("invalid issue %q", raw)
		}
		filter.IssueNum = issueNum
	}
	if raw := strings.TrimSpace(values.Get("since")); raw != "" {
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return filter, fmt.Errorf("invalid since %q", raw)
		}
		filter.Since = &ts
	}
	if raw := strings.TrimSpace(values.Get("until")); raw != "" {
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return filter, fmt.Errorf("invalid until %q", raw)
		}
		filter.Until = &ts
	}
	return filter, nil
}

func parseIssueStatePath(rawPath string) (string, int, bool) {
	clean := strings.TrimPrefix(path.Clean(rawPath), "/")
	if !strings.HasPrefix(clean, "issues/") || !strings.HasSuffix(clean, "/state") {
		return "", 0, false
	}
	trimmed := strings.TrimSuffix(strings.TrimPrefix(clean, "issues/"), "/state")
	slash := strings.LastIndex(trimmed, "/")
	if slash <= 0 {
		return "", 0, false
	}
	repo, err := url.PathUnescape(trimmed[:slash])
	if err != nil || strings.TrimSpace(repo) == "" {
		return "", 0, false
	}
	issueNum, err := strconv.Atoi(trimmed[slash+1:])
	if err != nil || issueNum <= 0 {
		return "", 0, false
	}
	return repo, issueNum, true
}

func inferCurrentState(labelsJSON, issueState string) string {
	var labels []string
	_ = json.Unmarshal([]byte(labelsJSON), &labels)
	sort.Strings(labels)
	for _, label := range labels {
		if strings.HasPrefix(label, "status:") {
			return label
		}
	}
	return issueState
}

func isIntermediateState(state string) bool {
	switch state {
	case "", "closed", "status:blocked", "status:done", "status:failed":
		return false
	default:
		return strings.HasPrefix(state, "status:")
	}
}

func maxTransitionCount(counts []store.TransitionCount) int {
	maxCount := 0
	for _, count := range counts {
		if count.Count > maxCount {
			maxCount = count.Count
		}
	}
	return maxCount
}

func payloadJSON(payload string) json.RawMessage {
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" {
		return nil
	}
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	quoted, _ := json.Marshal(trimmed)
	return json.RawMessage(quoted)
}

func parseAuditTimestamp(raw string) (time.Time, bool) {
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

func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
