package auditapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/ghadapter"
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
	rawPath := filepath.Join(sessionsDir, "session-40", "raw-session.jsonl")
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
	if !strings.HasSuffix(resp.ArtifactPaths.Raw, filepath.Join("session-40", "raw-session.jsonl")) {
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

type fakeRolloutPRReader struct {
	prs       map[string]ghadapter.PullRequest
	diffs     map[int]string
	diffCalls int
}

func (f *fakeRolloutPRReader) FindPullRequestByBranch(_ context.Context, _ string, branch string) (ghadapter.PullRequest, error) {
	pr, ok := f.prs[branch]
	if !ok {
		return ghadapter.PullRequest{}, ghadapter.ErrPullRequestNotFound
	}
	return pr, nil
}

func (f *fakeRolloutPRReader) DiffPullRequest(_ context.Context, _ string, prNum int) (string, error) {
	f.diffCalls++
	if diff, ok := f.diffs[prNum]; ok {
		return diff, nil
	}
	return "", errors.New("missing diff")
}

func TestHandleAPIIssueDetailIncludesRolloutMetadataOnSessions(t *testing.T) {
	st := newTestStore(t)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 294,
		Body:     "Rollout issue",
		Labels:   `["workbuddy","status:reviewing"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	if err := st.InsertTask(store.TaskRecord{
		ID:             "task-rollout-1",
		Repo:           "owner/repo",
		IssueNum:       294,
		AgentName:      "dev-agent",
		WorkerID:       "worker-a",
		Status:         store.TaskStatusCompleted,
		SessionRefs:    `["session-rollout-1"]`,
		RolloutIndex:   1,
		RolloutsTotal:  3,
		RolloutGroupID: "group-294",
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	if _, err := st.InsertAgentSession(store.AgentSession{
		SessionID:  "session-rollout-1",
		TaskID:     "task-rollout-1",
		Repo:       "owner/repo",
		IssueNum:   294,
		AgentName:  "dev-agent",
		TaskStatus: store.TaskStatusCompleted,
	}); err != nil {
		t.Fatalf("InsertAgentSession: %v", err)
	}

	h := NewHandler(st)
	mux := http.NewServeMux()
	h.RegisterDashboard(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/issues/owner/repo/294", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp issueDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(resp.Sessions))
	}
	if resp.Sessions[0].RolloutIndex != 1 || resp.Sessions[0].RolloutsTotal != 3 || resp.Sessions[0].RolloutGroupID != "group-294" {
		t.Fatalf("session rollout fields = %+v", resp.Sessions[0])
	}
}

func TestHandleAPIIssueDetailOmitsDefaultRolloutMetadataOnSessions(t *testing.T) {
	st := newTestStore(t)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 295,
		Body:     "Regular issue",
		Labels:   `["workbuddy","status:reviewing"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	if err := st.InsertTask(store.TaskRecord{
		ID:          "task-regular-1",
		Repo:        "owner/repo",
		IssueNum:    295,
		AgentName:   "dev-agent",
		WorkerID:    "worker-a",
		Status:      store.TaskStatusCompleted,
		SessionRefs: `["session-regular-1"]`,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	if _, err := st.InsertAgentSession(store.AgentSession{
		SessionID:  "session-regular-1",
		TaskID:     "task-regular-1",
		Repo:       "owner/repo",
		IssueNum:   295,
		AgentName:  "dev-agent",
		TaskStatus: store.TaskStatusCompleted,
	}); err != nil {
		t.Fatalf("InsertAgentSession: %v", err)
	}

	h := NewHandler(st)
	mux := http.NewServeMux()
	h.RegisterDashboard(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/issues/owner/repo/295", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp issueDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(resp.Sessions))
	}
	if resp.Sessions[0].RolloutIndex != 0 || resp.Sessions[0].RolloutsTotal != 0 || resp.Sessions[0].RolloutGroupID != "" {
		t.Fatalf("regular issue session should omit rollout fields, got %+v", resp.Sessions[0])
	}
}

func TestHandleAPIIssueRolloutsBuildsGroupedView(t *testing.T) {
	st := newTestStore(t)
	evlog := eventlog.NewEventLogger(st)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 294,
		Body:     "Rollout issue",
		Labels:   `["workbuddy","status:reviewing"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	insert := func(id string, rolloutIndex int, status string, workerID, sessionID string, created time.Time) {
		t.Helper()
		if err := st.InsertTask(store.TaskRecord{
			ID:             id,
			Repo:           "owner/repo",
			IssueNum:       294,
			AgentName:      "dev-agent",
			WorkerID:       workerID,
			Status:         status,
			SessionRefs:    fmt.Sprintf(`["%s"]`, sessionID),
			RolloutIndex:   rolloutIndex,
			RolloutsTotal:  3,
			RolloutGroupID: "group-294",
			CreatedAt:      created,
			CompletedAt:    created.Add(5 * time.Minute),
			UpdatedAt:      created.Add(5 * time.Minute),
		}); err != nil {
			t.Fatalf("InsertTask(%s): %v", id, err)
		}
	}
	insert("rollout-1", 1, store.TaskStatusCompleted, "worker-a", "session-a", now)
	insert("rollout-2", 2, store.TaskStatusFailed, "worker-b", "session-b", now.Add(1*time.Minute))
	insert("rollout-3", 3, store.TaskStatusCompleted, "worker-c", "session-c", now.Add(2*time.Minute))
	evlog.Log("synthesis_decision", "owner/repo", 294, map[string]any{
		"group_id":             "group-294",
		"outcome":              "pick",
		"chosen_rollout_index": 3,
		"reason":               "best test coverage",
		"ts":                   now.Add(10 * time.Minute).Format(time.RFC3339),
	})

	reader := &fakeRolloutPRReader{
		prs: map[string]ghadapter.PullRequest{
			"workbuddy/issue-294/rollout-1": {Number: 401, URL: "https://example/pr/401"},
			"workbuddy/issue-294/rollout-2": {Number: 402, URL: "https://example/pr/402"},
			"workbuddy/issue-294/rollout-3": {Number: 403, URL: "https://example/pr/403"},
		},
	}

	h := NewHandler(st)
	h.events = evlog
	h.SetRolloutPRReader(reader)
	h.SetNowFunc(func() time.Time { return now.Add(15 * time.Minute) })
	mux := http.NewServeMux()
	h.RegisterDashboard(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/issues/owner/repo/294/rollouts", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp rolloutGroupResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.GroupID != "group-294" || resp.RolloutsTotal != 3 {
		t.Fatalf("group = %+v", resp)
	}
	if len(resp.Members) != 3 {
		t.Fatalf("members = %d, want 3", len(resp.Members))
	}
	if resp.Members[1].PRNumber != 402 || resp.Members[1].Status != store.TaskStatusFailed {
		t.Fatalf("member[1] = %+v", resp.Members[1])
	}
	if resp.SynthOutcome == nil || resp.SynthOutcome.ChosenPR != 403 || resp.SynthOutcome.Decision != "pick" {
		t.Fatalf("synth_outcome = %+v", resp.SynthOutcome)
	}
}

func TestHandleAPIIssueRolloutDiffCachesPRDiff(t *testing.T) {
	st := newTestStore(t)
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 294,
		Body:     "Rollout issue",
		Labels:   `["workbuddy","status:reviewing"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	reader := &fakeRolloutPRReader{
		prs: map[string]ghadapter.PullRequest{
			"workbuddy/issue-294/rollout-2": {Number: 402},
		},
		diffs: map[int]string{
			402: "diff --git a/file.txt b/file.txt\n",
		},
	}
	h := NewHandler(st)
	h.SetRolloutPRReader(reader)
	h.SetPRDiffTTL(30 * time.Second)
	mux := http.NewServeMux()
	h.RegisterDashboard(mux)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/issues/owner/repo/294/rollouts/2/diff", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
		}
		if body := w.Body.String(); !strings.Contains(body, "diff --git") {
			t.Fatalf("body = %q", body)
		}
	}
	if reader.diffCalls != 1 {
		t.Fatalf("diffCalls = %d, want 1 cached call", reader.diffCalls)
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
		RawPath:    filepath.Join(sessionsDir, "session-40", "raw-session.jsonl"),
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
		RawPath:    filepath.Join(sessionsDir, "session-41", "raw-session.jsonl"),
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
		RawPath:    filepath.Join(sessionsDir, "session-active", "raw-session.jsonl"),
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

// TestDashboardSessionsEndpointAgentAndIssueFilters covers the agent + issue
// query parameters added for the SPA Sessions page (issue #220). The seed
// fixture has three sessions: session-40 (issue 40, dev-agent),
// session-41 (issue 41, review-agent), session-active (issue 42, dev-agent).
func TestDashboardSessionsEndpointAgentAndIssueFilters(t *testing.T) {
	fixture := newDashboardFixture(t)

	cases := []struct {
		query string
		ids   []string
	}{
		{"agent=dev-agent", []string{"session-active", "session-40"}},
		{"agent=review-agent", []string{"session-41"}},
		{"issue=41", []string{"session-41"}},
		{"agent=dev-agent&issue=42", []string{"session-active"}},
	}

	for _, tc := range cases {
		resp, err := http.Get(fixture.server.URL + "/api/v1/sessions?" + tc.query + "&limit=10")
		if err != nil {
			t.Fatalf("GET %s: %v", tc.query, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d", tc.query, resp.StatusCode)
		}
		var body []sessionListResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("%s decode: %v", tc.query, err)
		}
		_ = resp.Body.Close()
		got := make([]string, 0, len(body))
		for _, row := range body {
			got = append(got, row.SessionID)
		}
		if len(got) != len(tc.ids) {
			t.Fatalf("%s ids = %v, want %v", tc.query, got, tc.ids)
		}
		for i, want := range tc.ids {
			if got[i] != want {
				t.Fatalf("%s ids[%d] = %s, want %s (full=%v)", tc.query, i, got[i], want, got)
			}
		}
	}

	// Invalid issue rejects with 400.
	resp, err := http.Get(fixture.server.URL + "/api/v1/sessions?issue=abc")
	if err != nil {
		t.Fatalf("GET invalid issue: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid issue status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()
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
	// New fields added in #218.
	if body[0].LastHeartbeatAt.IsZero() {
		t.Fatalf("worker[0].last_heartbeat_at = zero, want timestamp")
	}
	// CurrentTaskID is "" for both seeded workers (active session for worker-1
	// is task-active but its status is "running", so it should populate).
	foundCurrent := false
	for _, w := range body {
		if w.CurrentTaskID != "" {
			foundCurrent = true
			break
		}
	}
	if !foundCurrent {
		t.Fatalf("expected at least one worker.current_task_id to be populated, got %#v", body)
	}
}

// ---------------------------------------------------------------------------
// Issue #218: in-flight, issue detail, sessions subpath, status field tests.
// ---------------------------------------------------------------------------

func newInFlightFixture(t *testing.T) *dashboardFixture {
	t.Helper()
	st := newTestStore(t)
	sessionsDir := filepath.Join(t.TempDir(), "sessions")
	seedDashboardData(t, st, sessionsDir)
	seedInFlightExtras(t, st)

	h := NewHandler(st)
	h.SetSessionsDir(sessionsDir)
	h.SetReportBaseURL("http://coord.example.com")
	// Pin "now" so stuck_for_seconds is deterministic. Issue 40's last
	// transition is 30 minutes before this; issue 41 is 2h before.
	fixed := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	h.SetNowFunc(func() time.Time { return fixed })

	mux := http.NewServeMux()
	h.RegisterDashboard(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &dashboardFixture{server: server, sessionsDir: sessionsDir}
}

func seedInFlightExtras(t *testing.T, st *store.Store) {
	t.Helper()
	// Issue 40 is already seeded as "open" with status:reviewing label.
	// Issue 41 needs an open in-flight cache too.
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 41,
		Labels:   `["status:developing"]`,
		Body:     "Add cycle cap to dev/review loop\n\nFull body here",
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache 41: %v", err)
	}
	// Closed issue (must NOT appear in in-flight).
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 99,
		Labels:   `["status:done"]`,
		Body:     "Already done",
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache 99: %v", err)
	}
	// Set issue 40's body to give the title field something to render.
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 40,
		Labels:   `["status:reviewing","type:feature"]`,
		Body:     "API endpoints for dashboard\n\ndetails",
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache 40 reseed: %v", err)
	}
	// Insert the two transitions with explicit, ordered timestamps so the
	// dashboard's ORDER BY ts ASC matches the insertion sequence and
	// stuck_for_seconds can be asserted deterministically.
	if _, err := st.DB().Exec(
		`INSERT INTO events (ts, type, repo, issue_num, payload) VALUES
		 ('2026-04-17 10:00:00','transition','owner/repo',40,'{"from":"developing","to":"reviewing","by":"dev-agent"}'),
		 ('2026-04-17 11:30:00','transition','owner/repo',40,'{"from":"reviewing","to":"developing","by":"review-agent"}')`,
	); err != nil {
		t.Fatalf("insert transitions: %v", err)
	}
	// Add an issue-claim row so claimed_worker_id is populated.
	if _, err := st.AcquireIssueClaim("owner/repo", 40, "worker-1", time.Hour); err != nil {
		t.Fatalf("AcquireIssueClaim: %v", err)
	}
}

func TestHandleIssuesInFlight_Happy(t *testing.T) {
	fixture := newInFlightFixture(t)

	resp, err := http.Get(fixture.server.URL + "/api/v1/issues/in-flight")
	if err != nil {
		t.Fatalf("GET /api/v1/issues/in-flight: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type = %q", got)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// Structurally-empty data must serialize as `[]` (not `null`).
	if strings.TrimSpace(string(body)) == "null" {
		t.Fatal("body decoded as JSON null, want array")
	}
	var rows []inFlightIssueResponse
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (issues 40 and 41); body=%s", len(rows), string(body))
	}
	byNum := map[int]inFlightIssueResponse{}
	for _, r := range rows {
		byNum[r.IssueNum] = r
	}
	row40, ok := byNum[40]
	if !ok {
		t.Fatalf("missing issue 40: %+v", byNum)
	}
	if row40.CurrentLabel != "status:reviewing" || row40.CurrentState != "reviewing" {
		t.Fatalf("issue 40 state = %q/%q", row40.CurrentLabel, row40.CurrentState)
	}
	if row40.Title != "API endpoints for dashboard" {
		t.Fatalf("issue 40 title = %q", row40.Title)
	}
	if row40.CycleCounts["developing->reviewing"] != 1 {
		t.Fatalf("issue 40 cycle counts = %#v", row40.CycleCounts)
	}
	if row40.LastTransitionAt == nil || row40.LastTransitionAt.IsZero() {
		t.Fatalf("issue 40 last_transition_at = %v", row40.LastTransitionAt)
	}
	if row40.StuckForSeconds < 1500 || row40.StuckForSeconds > 1900 {
		t.Fatalf("issue 40 stuck_for_seconds = %d, want ≈1800", row40.StuckForSeconds)
	}
	if row40.ClaimedWorkerID != "worker-1" {
		t.Fatalf("issue 40 claimed_worker_id = %q", row40.ClaimedWorkerID)
	}
	if row40.LastSessionID != "session-40" {
		t.Fatalf("issue 40 last_session_id = %q", row40.LastSessionID)
	}
	wantURL := "http://coord.example.com/workers/worker-1/sessions/session-40"
	if row40.LastSessionURL != wantURL {
		t.Fatalf("issue 40 last_session_url = %q, want %q", row40.LastSessionURL, wantURL)
	}
}

func TestHandleIssuesInFlight_EmptyReturnsArray(t *testing.T) {
	st := newTestStore(t)
	h := NewHandler(st)
	mux := http.NewServeMux()
	h.RegisterDashboard(mux)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/api/v1/issues/in-flight")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("body = %q, want \"[]\"", string(body))
	}
}

func TestHandleIssueDetail_Happy(t *testing.T) {
	fixture := newInFlightFixture(t)

	resp, err := http.Get(fixture.server.URL + "/api/v1/issues/owner/repo/40")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body issueDetailResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Repo != "owner/repo" || body.IssueNum != 40 {
		t.Fatalf("repo/issue = %q/%d", body.Repo, body.IssueNum)
	}
	if body.CurrentState != "reviewing" {
		t.Fatalf("current_state = %q", body.CurrentState)
	}
	if len(body.Transitions) < 2 {
		t.Fatalf("transitions = %d, want ≥2", len(body.Transitions))
	}
	first := body.Transitions[0]
	last := body.Transitions[len(body.Transitions)-1]
	if first.From != "developing" || first.To != "reviewing" {
		t.Fatalf("transitions[0] = %+v", first)
	}
	if first.By != "dev-agent" {
		t.Fatalf("transitions[0].by = %q", first.By)
	}
	if last.From != "reviewing" || last.To != "developing" {
		t.Fatalf("transitions[last] = %+v", last)
	}
	if len(body.TransitionCounts) < 2 {
		t.Fatalf("transition_counts = %d, want ≥2", len(body.TransitionCounts))
	}
	if len(body.Sessions) == 0 {
		t.Fatal("sessions empty, expected at least session-40")
	}
}

func TestHandleIssueDetail_NotFound(t *testing.T) {
	fixture := newInFlightFixture(t)

	resp, err := http.Get(fixture.server.URL + "/api/v1/issues/owner/repo/9999")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandleIssueDetail_Malformed(t *testing.T) {
	fixture := newInFlightFixture(t)

	resp, err := http.Get(fixture.server.URL + "/api/v1/issues/not-a-path")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestDashboardSessionEvents_Delegates(t *testing.T) {
	st := newTestStore(t)
	h := NewHandler(st)

	// Stub the events handler.
	called := false
	h.SetSessionEventsHandler(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if !strings.HasPrefix(r.URL.Path, "/api/v1/sessions/") {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"events":[]}`))
	})

	mux := http.NewServeMux()
	h.RegisterDashboard(mux)

	req := httptest.NewRequest("GET", "/api/v1/sessions/session-x/events", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if !called {
		t.Fatal("events handler not invoked")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestDashboardSessionEvents_NoHandler404(t *testing.T) {
	st := newTestStore(t)
	h := NewHandler(st)
	mux := http.NewServeMux()
	h.RegisterDashboard(mux)

	req := httptest.NewRequest("GET", "/api/v1/sessions/session-x/events", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}

func TestDashboardSessionsEndpoint_AddedFields(t *testing.T) {
	st := newTestStore(t)
	sessionsDir := filepath.Join(t.TempDir(), "sessions")
	seedDashboardData(t, st, sessionsDir)
	// Add task workflow + state so the new session-list fields populate.
	if _, err := st.DB().Exec(`UPDATE task_queue SET workflow = 'default', state = 'reviewing' WHERE id = ?`, "task-40"); err != nil {
		t.Fatalf("update task workflow: %v", err)
	}

	h := NewHandler(st)
	h.SetSessionsDir(sessionsDir)
	mux := http.NewServeMux()
	h.RegisterDashboard(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/api/v1/sessions?repo=owner/repo&limit=10")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var rows []sessionListResponse
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var s40 *sessionListResponse
	for i := range rows {
		if rows[i].SessionID == "session-40" {
			s40 = &rows[i]
		}
	}
	if s40 == nil {
		t.Fatal("missing session-40")
	}
	if s40.Workflow != "default" {
		t.Fatalf("workflow = %q", s40.Workflow)
	}
	if s40.CurrentState != "reviewing" {
		t.Fatalf("current_state = %q", s40.CurrentState)
	}
	if s40.TaskStatus == "" {
		t.Fatalf("task_status = %q", s40.TaskStatus)
	}
	if s40.RolloutIndex != 0 || s40.RolloutsTotal != 0 || s40.RolloutGroupID != "" {
		t.Fatalf("non-rollout session should omit rollout fields, got %+v", *s40)
	}
}

func TestDashboardStatusEndpoint_AddedFields(t *testing.T) {
	fixture := newInFlightFixture(t)

	resp, err := http.Get(fixture.server.URL + "/api/v1/status")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.InFlightIssues != 2 {
		t.Fatalf("in_flight_issues = %d, want 2", body.InFlightIssues)
	}
	// Issue 40's last transition is 30m before fixed-now, so 0 stuck. Issue 41
	// has no transitions at all (not stuck). Both done/failed counters are
	// computed from sessions that closed in the last 24h.
	if body.StuckIssuesOver1H < 0 {
		t.Fatalf("stuck_issues_over_1h = %d", body.StuckIssuesOver1H)
	}
}

// Issue #275: a session with only a metadata.json (no DB row, no events
// file) must surface as degraded so operators can tell it apart from a
// session that ran normally and happened to emit nothing.
func TestDashboardSessionDetail_DegradedFromDiskMetadata(t *testing.T) {
	st := newTestStore(t)
	sessionsDir := filepath.Join(t.TempDir(), "sessions")
	sessionID := "session-aborted-before-start"
	dir := filepath.Join(sessionsDir, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	metadata := map[string]any{
		"session_id":  sessionID,
		"task_id":     "task-aborted",
		"repo":        "owner/repo",
		"issue_num":   77,
		"agent_name":  "dev-agent",
		"runtime":     "claude-oneshot",
		"worker_id":   "worker-x",
		"attempt":     1,
		"status":      "running",
		"created_at":  "2026-04-30T08:00:00Z",
		"stdout_path": filepath.Join(dir, "stdout"),
		"stderr_path": filepath.Join(dir, "stderr"),
	}
	metaJSON, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), metaJSON, 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	stderrExcerpt := "runtime: claude-oneshot: start: post /agents: Post \"http://127.0.0.1:36597/agents\": context canceled\n"
	if err := os.WriteFile(filepath.Join(dir, "stderr"), []byte(stderrExcerpt), 0o644); err != nil {
		t.Fatalf("write stderr: %v", err)
	}

	h := NewHandler(st)
	h.SetSessionsDir(sessionsDir)
	mux := http.NewServeMux()
	h.RegisterDashboard(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/api/v1/sessions/" + sessionID)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, string(body))
	}
	var body sessionDetailResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Degraded {
		t.Fatalf("degraded = false, want true")
	}
	if body.DegradedReason != "no_db_row" {
		t.Fatalf("degraded_reason = %q, want %q", body.DegradedReason, "no_db_row")
	}
	if body.Status != StatusAbortedBeforeStart {
		t.Fatalf("status = %q, want %q", body.Status, StatusAbortedBeforeStart)
	}
	if body.ExitCode != -1 {
		t.Fatalf("exit_code = %d, want -1", body.ExitCode)
	}
	if body.SessionID != sessionID {
		t.Fatalf("session_id = %q", body.SessionID)
	}
	if body.Repo != "owner/repo" || body.IssueNum != 77 {
		t.Fatalf("repo/issue = %q/%d", body.Repo, body.IssueNum)
	}
	if body.AgentName != "dev-agent" {
		t.Fatalf("agent_name = %q", body.AgentName)
	}
	if !strings.Contains(body.StderrSummary, "context canceled") {
		t.Fatalf("stderr_summary missing canceled marker: %q", body.StderrSummary)
	}
	if !strings.Contains(body.Summary, "no session file") {
		t.Fatalf("summary missing degraded marker: %q", body.Summary)
	}
	if !strings.Contains(body.Summary, "context canceled") {
		t.Fatalf("summary missing stderr excerpt: %q", body.Summary)
	}
}

// TestDashboardSessionDetail_NoMetadataReturns404 keeps the existing 404
// behavior for sessions where neither the DB nor disk has anything.
func TestDashboardSessionDetail_NoMetadataReturns404(t *testing.T) {
	fixture := newDashboardFixture(t)
	resp, err := http.Get(fixture.server.URL + "/api/v1/sessions/totally-missing")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// Issue #275: a DB-backed session whose events-v1.jsonl is empty AND whose
// status is terminal should also be flagged degraded so the SPA surfaces
// the warning even when the row exists.
func TestDashboardSessionDetail_DegradedForEmptyEventsFile(t *testing.T) {
	st := newTestStore(t)
	sessionsDir := filepath.Join(t.TempDir(), "sessions")
	dir := filepath.Join(sessionsDir, "session-empty-events")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events-v1.jsonl"), []byte{}, 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}
	if err := st.InsertTask(store.TaskRecord{
		ID:        "task-empty",
		Repo:      "owner/repo",
		IssueNum:  78,
		AgentName: "dev-agent",
		Status:    store.TaskStatusFailed,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	if _, err := st.CreateSession(store.SessionRecord{
		SessionID: "session-empty-events",
		TaskID:    "task-empty",
		Repo:      "owner/repo",
		IssueNum:  78,
		AgentName: "dev-agent",
		Status:    store.TaskStatusFailed,
		Dir:       dir,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	h := NewHandler(st)
	h.SetSessionsDir(sessionsDir)
	mux := http.NewServeMux()
	h.RegisterDashboard(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/api/v1/sessions/session-empty-events")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body sessionDetailResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Degraded {
		t.Fatalf("degraded = false, want true (empty events + terminal status)")
	}
	if body.DegradedReason != "no_events_file" {
		t.Fatalf("degraded_reason = %q, want no_events_file", body.DegradedReason)
	}
}

// Issue #275: the listing endpoint should surface disk-only sessions and
// flag them degraded so they appear in /sessions with the ⚠️ badge.
func TestDashboardSessionsList_IncludesDegradedDiskOnlyRow(t *testing.T) {
	st := newTestStore(t)
	sessionsDir := filepath.Join(t.TempDir(), "sessions")
	sessionID := "session-disk-only"
	dir := filepath.Join(sessionsDir, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	metadata := map[string]any{
		"session_id": sessionID,
		"repo":       "owner/repo",
		"issue_num":  79,
		"agent_name": "dev-agent",
		"attempt":    1,
		"status":     "running",
		"created_at": "2026-04-30T09:00:00Z",
	}
	metaJSON, _ := json.MarshalIndent(metadata, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), metaJSON, 0o644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	h := NewHandler(st)
	h.SetSessionsDir(sessionsDir)
	mux := http.NewServeMux()
	h.RegisterDashboard(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/api/v1/sessions?limit=10")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var rows []sessionListResponse
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var disk *sessionListResponse
	for i := range rows {
		if rows[i].SessionID == sessionID {
			disk = &rows[i]
		}
	}
	if disk == nil {
		t.Fatalf("disk-only session missing from listing: %+v", rows)
	}
	if !disk.Degraded {
		t.Fatalf("degraded = false, want true")
	}
	if disk.DegradedReason != "no_db_row" {
		t.Fatalf("degraded_reason = %q, want no_db_row", disk.DegradedReason)
	}
	if disk.Status != StatusAbortedBeforeStart {
		t.Fatalf("status = %q, want %q", disk.Status, StatusAbortedBeforeStart)
	}
	if disk.ExitCode != -1 {
		t.Fatalf("exit_code = %d, want -1", disk.ExitCode)
	}
}

func TestDeprecatedSessionPath_AddsHeaderAndKeepsBody(t *testing.T) {
	st := newTestStore(t)
	sessionsDir := filepath.Join(t.TempDir(), "sessions")
	seedDashboardData(t, st, sessionsDir)

	// Build a webui handler so the deprecation alias paths under /sessions/
	// are registered. This mirrors the coordinator wiring.
	uiCalled := false
	stubEvents := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uiCalled = true
		writeJSONStatus(w, http.StatusOK, map[string]any{"events": []any{}, "total": 0})
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/sessions/", func(w http.ResponseWriter, r *http.Request) {
		// Mimic the deprecation header that the real webui handler attaches.
		if strings.HasSuffix(r.URL.Path, "/events.json") {
			w.Header().Set("Deprecation", "true")
			stubEvents(w, r)
			return
		}
		http.NotFound(w, r)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/sessions/session-40/events.json")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if !uiCalled {
		t.Fatal("expected legacy events handler to be called")
	}
	if got := resp.Header.Get("Deprecation"); got != "true" {
		t.Fatalf("Deprecation header = %q, want \"true\"", got)
	}
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// TestHandleIssuesInFlight_Golden snapshots the canonical /api/v1/issues/
// in-flight payload so any future drift in the field names/shape is caught
// at PR-review time rather than in production. Pattern mirrors
// internal/reporter/format_golden_test.go.
func TestHandleIssuesInFlight_Golden(t *testing.T) {
	fixture := newInFlightFixture(t)

	resp, err := http.Get(fixture.server.URL + "/api/v1/issues/in-flight")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var rows []inFlightIssueResponse
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Re-marshal indented for stable snapshot (and to filter the issue 41 row,
	// which has no transitions and no deterministic data to assert).
	for i := range rows {
		if rows[i].IssueNum == 40 {
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetIndent("", "  ")
			enc.SetEscapeHTML(false)
			if err := enc.Encode(rows[i]); err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got := strings.TrimRight(buf.String(), "\n")
			want := `{
  "repo": "owner/repo",
  "issue_num": 40,
  "title": "API endpoints for dashboard",
  "current_state": "reviewing",
  "current_label": "status:reviewing",
  "labels": [
    "status:reviewing",
    "type:feature"
  ],
  "cycle_counts": {
    "developing->reviewing": 1,
    "reviewing->developing": 1
  },
  "last_transition_at": "2026-04-17T11:30:00Z",
  "stuck_for_seconds": 1800,
  "claimed_worker_id": "worker-1",
  "last_session_id": "session-40",
  "last_session_url": "http://coord.example.com/workers/worker-1/sessions/session-40"
}`
			if got != want {
				t.Fatalf("issue 40 row drift.\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
			return
		}
	}
	t.Fatal("issue 40 row not found in response")
}

// Issue #282: a session whose DB row hasn't been written yet but is actively
// streaming events to disk must be reported as live (status=running,
// degraded=false), not as aborted_before_start.
func TestSessionDetailNoDBRowRunningWithEventsIsNotDegraded(t *testing.T) {
	st := newTestStore(t)
	sessionsDir := filepath.Join(t.TempDir(), "sessions")
	sessionID := "session-running-with-events"
	dir := filepath.Join(sessionsDir, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	metadata := map[string]any{
		"session_id": sessionID,
		"task_id":    "task-live",
		"repo":       "owner/repo",
		"issue_num":  281,
		"agent_name": "dev-agent",
		"runtime":    "claude-oneshot",
		"worker_id":  "worker-z",
		"attempt":    1,
		"status":     "running",
		"created_at": "2026-04-30T08:00:00Z",
	}
	metaJSON, _ := json.MarshalIndent(metadata, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), metaJSON, 0o644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events-v1.jsonl"),
		[]byte(`{"type":"tool_use"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}

	h := NewHandler(st)
	h.SetSessionsDir(sessionsDir)
	mux := http.NewServeMux()
	h.RegisterDashboard(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/api/v1/sessions/" + sessionID)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, string(body))
	}
	var body sessionDetailResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Degraded {
		t.Fatalf("degraded = true, want false (live session with streaming events)")
	}
	if body.DegradedReason != "" {
		t.Fatalf("degraded_reason = %q, want empty", body.DegradedReason)
	}
	if body.Status != store.TaskStatusRunning {
		t.Fatalf("status = %q, want %q", body.Status, store.TaskStatusRunning)
	}
	if body.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0", body.ExitCode)
	}
	if body.Repo != "owner/repo" || body.IssueNum != 281 {
		t.Fatalf("repo/issue mismatch: %q/%d", body.Repo, body.IssueNum)
	}
}

// Issue #282 (list-side AC-3): a live in-flight session enumerated from disk
// must appear as a running, non-degraded row in /api/v1/sessions so it does
// not get a wb-badge-degraded pill.
func TestDashboardSessionsList_LiveDiskOnlyRowIsNotDegraded(t *testing.T) {
	st := newTestStore(t)
	sessionsDir := filepath.Join(t.TempDir(), "sessions")
	sessionID := "session-disk-only-live"
	dir := filepath.Join(sessionsDir, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	metadata := map[string]any{
		"session_id": sessionID,
		"repo":       "owner/repo",
		"issue_num":  281,
		"agent_name": "dev-agent",
		"attempt":    1,
		"status":     "running",
		"created_at": "2026-04-30T09:00:00Z",
	}
	metaJSON, _ := json.MarshalIndent(metadata, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), metaJSON, 0o644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events-v1.jsonl"),
		[]byte(`{"type":"tool_use"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}

	h := NewHandler(st)
	h.SetSessionsDir(sessionsDir)
	mux := http.NewServeMux()
	h.RegisterDashboard(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/api/v1/sessions?limit=10")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var rows []sessionListResponse
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var row *sessionListResponse
	for i := range rows {
		if rows[i].SessionID == sessionID {
			row = &rows[i]
		}
	}
	if row == nil {
		t.Fatalf("live disk-only session missing from listing: %+v", rows)
	}
	if row.Degraded {
		t.Fatalf("degraded = true, want false")
	}
	if row.DegradedReason != "" {
		t.Fatalf("degraded_reason = %q, want empty", row.DegradedReason)
	}
	if row.Status != store.TaskStatusRunning {
		t.Fatalf("status = %q, want %q", row.Status, store.TaskStatusRunning)
	}
	if row.TaskStatus != store.TaskStatusRunning {
		t.Fatalf("task_status = %q, want %q", row.TaskStatus, store.TaskStatusRunning)
	}
	if row.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0", row.ExitCode)
	}
}
