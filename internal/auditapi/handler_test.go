package auditapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/operator"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedAuditData(t *testing.T, st *store.Store, sessionsDir string) {
	t.Helper()
	if _, err := st.InsertEvent(store.Event{
		Type:     "dispatch",
		Repo:     "owner/repo",
		IssueNum: 40,
		Payload:  `{"agent":"dev-agent"}`,
	}); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 40,
		Labels:   `["status:reviewing","type:feature"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	if _, err := st.IncrementTransition("owner/repo", 40, "developing", "reviewing"); err != nil {
		t.Fatalf("IncrementTransition 1: %v", err)
	}
	if _, err := st.IncrementTransition("owner/repo", 40, "reviewing", "developing"); err != nil {
		t.Fatalf("IncrementTransition 2: %v", err)
	}
	if err := st.UpsertIssueDependencyState(store.IssueDependencyState{
		Repo:              "owner/repo",
		IssueNum:          40,
		Verdict:           store.DependencyVerdictBlocked,
		ResumeLabel:       "status:developing",
		BlockedReasonHash: "abc123",
		GraphVersion:      7,
	}); err != nil {
		t.Fatalf("UpsertIssueDependencyState: %v", err)
	}
	rawPath := filepath.Join(sessionsDir, "session-40", "codex-exec.jsonl")
	if err := os.MkdirAll(filepath.Dir(rawPath), 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, "session-40", "events-v1.jsonl"), []byte("{\"kind\":\"log\"}\n"), 0o644); err != nil {
		t.Fatalf("write events-v1: %v", err)
	}
	if err := os.WriteFile(rawPath, []byte("{\"type\":\"task_started\"}\n"), 0o644); err != nil {
		t.Fatalf("write raw session: %v", err)
	}
	if _, err := st.InsertAgentSession(store.AgentSession{
		SessionID: "session-40",
		TaskID:    "task-40",
		Repo:      "owner/repo",
		IssueNum:  40,
		AgentName: "dev-agent",
		Summary:   "summary",
		RawPath:   rawPath,
	}); err != nil {
		t.Fatalf("InsertAgentSession: %v", err)
	}
}

func TestHandleEvents(t *testing.T) {
	st := newTestStore(t)
	h := NewHandler(st)
	dir := t.TempDir()
	h.SetSessionsDir(dir)
	seedAuditData(t, st, dir)

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/events?repo=owner/repo&issue=40&type=dispatch&since=2020-01-01T00:00:00Z", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp eventsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(resp.Events))
	}
	if resp.Events[0].Type != "dispatch" {
		t.Fatalf("type = %q", resp.Events[0].Type)
	}
	payload, ok := resp.Events[0].Payload.(map[string]any)
	if !ok || payload["agent"] != "dev-agent" {
		t.Fatalf("payload = %#v", resp.Events[0].Payload)
	}
}

func TestHandleIssueState(t *testing.T) {
	st := newTestStore(t)
	h := NewHandler(st)
	dir := t.TempDir()
	h.SetSessionsDir(dir)
	seedAuditData(t, st, dir)

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/issues/owner/repo/40/state", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp issueStateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CycleCount != 2 {
		t.Fatalf("cycle_count = %d, want 2", resp.CycleCount)
	}
	if resp.DependencyVerdict != store.DependencyVerdictBlocked {
		t.Fatalf("dependency_verdict = %q", resp.DependencyVerdict)
	}
	if len(resp.Labels) != 2 || resp.Labels[0] != "status:reviewing" {
		t.Fatalf("labels = %#v", resp.Labels)
	}
}

func TestHandleSession(t *testing.T) {
	st := newTestStore(t)
	h := NewHandler(st)
	dir := t.TempDir()
	h.SetSessionsDir(dir)
	seedAuditData(t, st, dir)

	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/sessions/session-40", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp SessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SessionID != "session-40" {
		t.Fatalf("session_id = %q", resp.SessionID)
	}
	if !strings.HasSuffix(resp.ArtifactPaths.EventsV1, filepath.Join("session-40", "events-v1.jsonl")) {
		t.Fatalf("events path = %q", resp.ArtifactPaths.EventsV1)
	}
	if !strings.HasSuffix(resp.ArtifactPaths.Raw, filepath.Join("session-40", "codex-exec.jsonl")) {
		t.Fatalf("raw path = %q", resp.ArtifactPaths.Raw)
	}
}

func TestHandleEvents_InvalidSince(t *testing.T) {
	st := newTestStore(t)
	h := NewHandler(st)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest("GET", "/events?since=not-a-time", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestParseIssueStatePath(t *testing.T) {
	repo, issueNum, ok := parseIssueStatePath("/issues/owner/repo/40/state")
	if !ok {
		t.Fatal("expected valid path")
	}
	if repo != "owner/repo" || issueNum != 40 {
		t.Fatalf("repo/issue = %q/%d", repo, issueNum)
	}
}

func TestDecodeJSONOrString(t *testing.T) {
	got := decodeJSONOrString(`{"ok":true}`)
	m, ok := got.(map[string]any)
	if !ok || m["ok"] != true {
		t.Fatalf("decoded = %#v", got)
	}
	if decodeJSONOrString("") != nil {
		t.Fatal("empty payload should decode to nil")
	}
	if s, ok := decodeJSONOrString("not-json").(string); !ok || s != "not-json" {
		t.Fatalf("fallback = %#v", decodeJSONOrString("not-json"))
	}
}

func TestSessionResponseJSONTime(t *testing.T) {
	resp := SessionResponse{CreatedAt: time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), "2026-04-15T10:00:00Z") {
		t.Fatalf("json = %s", string(data))
	}
}

type dashboardFixture struct {
	server      *httptest.Server
	sessionsDir string
}

func newDashboardFixture(t *testing.T) *dashboardFixture {
	t.Helper()
	st := newTestStore(t)
	sessionsDir := filepath.Join(t.TempDir(), "sessions")
	seedDashboardData(t, st, sessionsDir)

	h := NewHandler(st)
	h.SetSessionsDir(sessionsDir)
	mux := http.NewServeMux()
	h.RegisterDashboard(mux)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &dashboardFixture{
		server:      server,
		sessionsDir: sessionsDir,
	}
}

func seedDashboardData(t *testing.T, st *store.Store, sessionsDir string) {
	t.Helper()

	insertSession := func(record store.SessionRecord, stdout, stderr string, finishedAfter time.Duration, exitCode int) {
		if err := os.MkdirAll(record.Dir, 0o755); err != nil {
			t.Fatalf("mkdir session dir: %v", err)
		}
		if stdout != "" {
			if err := os.WriteFile(record.StdoutPath, []byte(stdout), 0o644); err != nil {
				t.Fatalf("write stdout: %v", err)
			}
		}
		if stderr != "" {
			if err := os.WriteFile(record.StderrPath, []byte(stderr), 0o644); err != nil {
				t.Fatalf("write stderr: %v", err)
			}
		}
		if err := os.WriteFile(filepath.Join(record.Dir, "events-v1.jsonl"), []byte("{\"kind\":\"log\"}\n"), 0o644); err != nil {
			t.Fatalf("write events-v1: %v", err)
		}
		if record.RawPath != "" {
			if err := os.WriteFile(record.RawPath, []byte("{\"type\":\"task_started\"}\n"), 0o644); err != nil {
				t.Fatalf("write raw path: %v", err)
			}
		}
		if err := st.InsertTask(store.TaskRecord{
			ID:        record.TaskID,
			Repo:      record.Repo,
			IssueNum:  record.IssueNum,
			AgentName: record.AgentName,
			Status:    record.Status,
			WorkerID:  record.WorkerID,
			ExitCode:  exitCode,
		}); err != nil {
			t.Fatalf("InsertTask: %v", err)
		}
		if _, err := st.CreateSession(record); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		if finishedAfter > 0 {
			saved, err := st.GetSession(record.SessionID)
			if err != nil {
				t.Fatalf("GetSession: %v", err)
			}
			saved.ClosedAt = saved.CreatedAt.Add(finishedAfter)
			if err := st.UpdateSession(*saved); err != nil {
				t.Fatalf("UpdateSession: %v", err)
			}
			if _, err := st.DB().Exec(`UPDATE task_queue SET completed_at = ? WHERE id = ?`, saved.ClosedAt.UTC().Format(time.RFC3339), record.TaskID); err != nil {
				t.Fatalf("update task completed_at: %v", err)
			}
		}
	}

	if err := st.InsertWorker(store.WorkerRecord{
		ID:       "worker-1",
		Repo:     "owner/repo",
		Roles:    `["dev","review"]`,
		Hostname: "host-a",
		Status:   "online",
	}); err != nil {
		t.Fatalf("InsertWorker worker-1: %v", err)
	}
	if err := st.InsertWorker(store.WorkerRecord{
		ID:       "worker-2",
		Repo:     "owner/repo",
		Roles:    `["dev"]`,
		Hostname: "host-b",
		Status:   "offline",
	}); err != nil {
		t.Fatalf("InsertWorker worker-2: %v", err)
	}

	if _, err := st.InsertEvent(store.Event{
		Type:     poller.EventPollCycleDone,
		Repo:     "owner/repo",
		IssueNum: 0,
		Payload:  `{"source":"poller"}`,
	}); err != nil {
		t.Fatalf("InsertEvent poll_cycle_done: %v", err)
	}
	if _, err := st.InsertEvent(store.Event{
		Type:     "dispatch",
		Repo:     "owner/repo",
		IssueNum: 40,
		Payload:  `{"agent":"dev-agent"}`,
	}); err != nil {
		t.Fatalf("InsertEvent dispatch: %v", err)
	}
	if _, err := st.InsertEvent(store.Event{
		Type:     "completed",
		Repo:     "owner/repo",
		IssueNum: 40,
		Payload:  `{"status":"completed"}`,
	}); err != nil {
		t.Fatalf("InsertEvent completed: %v", err)
	}
	alertPayload, err := json.Marshal(operator.Alert{
		ID:       "alert-40",
		Kind:     operator.KindWorkerMissing,
		Severity: operator.SeverityWarn,
		Ts:       time.Date(2026, 4, 17, 11, 58, 0, 0, time.UTC),
		Resource: map[string]any{"repo": "owner/repo", "worker_id": "worker-2"},
		Detail:   "worker heartbeat is stale",
	})
	if err != nil {
		t.Fatalf("marshal alert payload: %v", err)
	}
	if _, err := st.InsertEvent(store.Event{
		Type:    eventlog.TypeAlert,
		Repo:    "owner/repo",
		Payload: string(alertPayload),
	}); err != nil {
		t.Fatalf("InsertEvent alert: %v", err)
	}
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 40,
		Labels:   `["status:reviewing","type:feature"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	if _, err := st.IncrementTransition("owner/repo", 40, "developing", "reviewing"); err != nil {
		t.Fatalf("IncrementTransition 1: %v", err)
	}
	if _, err := st.IncrementTransition("owner/repo", 40, "reviewing", "developing"); err != nil {
		t.Fatalf("IncrementTransition 2: %v", err)
	}

	insertSession(store.SessionRecord{
		SessionID:  "session-40",
		TaskID:     "task-40",
		Repo:       "owner/repo",
		IssueNum:   40,
		AgentName:  "dev-agent",
		Runtime:    "codex",
		WorkerID:   "worker-1",
		Attempt:    2,
		Status:     store.TaskStatusCompleted,
		Dir:        filepath.Join(sessionsDir, "session-40"),
		StdoutPath: filepath.Join(sessionsDir, "session-40", "stdout"),
		StderrPath: filepath.Join(sessionsDir, "session-40", "stderr"),
		Summary:    "session 40 summary",
		RawPath:    filepath.Join(sessionsDir, "session-40", "codex-exec.jsonl"),
	}, "stdout line 1\nstdout line 2\n", "stderr line 1\n", 2*time.Second, 0)

	insertSession(store.SessionRecord{
		SessionID:  "session-41",
		TaskID:     "task-41",
		Repo:       "owner/repo",
		IssueNum:   41,
		AgentName:  "review-agent",
		Runtime:    "claude-code",
		WorkerID:   "worker-2",
		Attempt:    1,
		Status:     store.TaskStatusFailed,
		Dir:        filepath.Join(sessionsDir, "session-41"),
		StdoutPath: filepath.Join(sessionsDir, "session-41", "stdout"),
		StderrPath: filepath.Join(sessionsDir, "session-41", "stderr"),
		Summary:    "session 41 summary",
		RawPath:    filepath.Join(sessionsDir, "session-41", "codex-exec.jsonl"),
	}, "review stdout\n", "review stderr\n", 4*time.Second, 1)

	insertSession(store.SessionRecord{
		SessionID:  "session-active",
		TaskID:     "task-active",
		Repo:       "owner/repo",
		IssueNum:   42,
		AgentName:  "dev-agent",
		Runtime:    "codex",
		WorkerID:   "worker-1",
		Attempt:    1,
		Status:     store.TaskStatusRunning,
		Dir:        filepath.Join(sessionsDir, "session-active"),
		StdoutPath: filepath.Join(sessionsDir, "session-active", "stdout"),
		StderrPath: filepath.Join(sessionsDir, "session-active", "stderr"),
		Summary:    "active summary",
		RawPath:    filepath.Join(sessionsDir, "session-active", "codex-exec.jsonl"),
	}, "active stdout\n", "", 0, 0)
}

func TestDashboardStatusEndpoint(t *testing.T) {
	fixture := newDashboardFixture(t)

	resp, err := http.Get(fixture.server.URL + "/api/v1/status")
	if err != nil {
		t.Fatalf("GET /api/v1/status: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type = %q", got)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var body statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ActiveSessions != 1 {
		t.Fatalf("active_sessions = %d, want 1", body.ActiveSessions)
	}
	if body.Workers != 2 {
		t.Fatalf("workers = %d, want 2", body.Workers)
	}
	if body.LastPoll == nil || body.LastPoll.IsZero() {
		t.Fatalf("last_poll = %v, want timestamp", body.LastPoll)
	}
}

func TestDashboardSessionsEndpoint(t *testing.T) {
	fixture := newDashboardFixture(t)

	resp, err := http.Get(fixture.server.URL + "/api/v1/sessions?repo=owner/repo&limit=1&offset=1")
	if err != nil {
		t.Fatalf("GET /api/v1/sessions: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type = %q", got)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var body []sessionListResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body) != 1 {
		t.Fatalf("len = %d, want 1", len(body))
	}
	if body[0].SessionID != "session-41" {
		t.Fatalf("session_id = %q, want session-41", body[0].SessionID)
	}
}

func TestDashboardSessionDetailEndpoint(t *testing.T) {
	fixture := newDashboardFixture(t)

	resp, err := http.Get(fixture.server.URL + "/api/v1/sessions/session-40")
	if err != nil {
		t.Fatalf("GET /api/v1/sessions/session-40: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type = %q", got)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var body sessionDetailResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0", body.ExitCode)
	}
	if body.Duration <= 0 {
		t.Fatalf("duration = %d, want > 0", body.Duration)
	}
	if !strings.Contains(body.StdoutSummary, "stdout line 1") {
		t.Fatalf("stdout_summary = %q", body.StdoutSummary)
	}
	if !strings.Contains(body.StderrSummary, "stderr line 1") {
		t.Fatalf("stderr_summary = %q", body.StderrSummary)
	}

	notFoundResp, err := http.Get(fixture.server.URL + "/api/v1/sessions/does-not-exist")
	if err != nil {
		t.Fatalf("GET /api/v1/sessions/does-not-exist: %v", err)
	}
	defer func() { _ = notFoundResp.Body.Close() }()
	if notFoundResp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", notFoundResp.StatusCode)
	}
	if got := notFoundResp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("404 content-type = %q", got)
	}
}

func TestDashboardEventsEndpoint(t *testing.T) {
	fixture := newDashboardFixture(t)

	req, err := http.NewRequest(http.MethodGet, fixture.server.URL+"/api/v1/events?repo=owner/repo&type=dispatch&since=2020-01-01T00:00:00Z", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type = %q", got)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var body []eventResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body) != 1 {
		t.Fatalf("len = %d, want 1", len(body))
	}
	if body[0].Type != "dispatch" {
		t.Fatalf("type = %q, want dispatch", body[0].Type)
	}
}

func TestDashboardAlertsEndpoint(t *testing.T) {
	fixture := newDashboardFixture(t)

	resp, err := http.Get(fixture.server.URL + "/api/v1/alerts?severity=warn")
	if err != nil {
		t.Fatalf("GET /api/v1/alerts: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type = %q", got)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var body []operator.Alert
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body) != 1 {
		t.Fatalf("len = %d, want 1", len(body))
	}
	if body[0].Kind != operator.KindWorkerMissing {
		t.Fatalf("kind = %q, want %q", body[0].Kind, operator.KindWorkerMissing)
	}
}

func TestDashboardMetricsEndpoint(t *testing.T) {
	fixture := newDashboardFixture(t)

	resp, err := http.Get(fixture.server.URL + "/api/v1/metrics")
	if err != nil {
		t.Fatalf("GET /api/v1/metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type = %q", got)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var body metricsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.SuccessRate <= 0 || body.SuccessRate >= 1 {
		t.Fatalf("success_rate = %v, want between 0 and 1", body.SuccessRate)
	}
	if body.AvgDuration <= 0 {
		t.Fatalf("avg_duration = %v, want > 0", body.AvgDuration)
	}
	if body.RetryRate <= 0 {
		t.Fatalf("retry_rate = %v, want > 0", body.RetryRate)
	}
	if body.AgentExecutions["dev-agent"] != 2 {
		t.Fatalf("dev-agent executions = %d, want 2", body.AgentExecutions["dev-agent"])
	}
}

func TestDashboardWorkersEndpoint(t *testing.T) {
	fixture := newDashboardFixture(t)

	resp, err := http.Get(fixture.server.URL + "/api/v1/workers")
	if err != nil {
		t.Fatalf("GET /api/v1/workers: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type = %q", got)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var body []workerResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body) != 2 {
		t.Fatalf("len = %d, want 2", len(body))
	}
	if body[0].ID != "worker-1" {
		t.Fatalf("worker[0].id = %q, want worker-1", body[0].ID)
	}
	if len(body[0].Roles) != 2 {
		t.Fatalf("worker[0].roles = %#v", body[0].Roles)
	}
}
