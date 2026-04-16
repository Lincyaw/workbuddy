package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/audit"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/reporter"
	"github.com/Lincyaw/workbuddy/internal/router"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func setupTestConfigDir(t *testing.T, repo string) string {
	t.Helper()
	dir := t.TempDir()

	// config.yaml
	configYAML := fmt.Sprintf("repo: %s\npoll_interval: 1s\nport: 0\n", repo)
	writeFile(t, filepath.Join(dir, "config.yaml"), configYAML)

	// Agent
	agentMD := `---
name: dev-agent
description: Dev agent
triggers:
  - label: "status:developing"
role: dev
runtime: claude-code
command: echo "hello"
timeout: 30s
---
# Dev Agent
`
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "agents", "dev-agent.md"), agentMD)

	// Workflow
	workflowMD := `---
name: dev-workflow
description: Dev workflow
trigger:
  issue_label: "workbuddy"
max_retries: 3
---
# Dev Workflow

` + "```yaml\nstates:\n  triage:\n    enter_label: \"status:triage\"\n    transitions:\n      - to: developing\n        when: 'labeled \"status:developing\"'\n  developing:\n    enter_label: \"status:developing\"\n    agent: dev-agent\n    transitions:\n      - to: done\n        when: 'labeled \"status:done\"'\n  done:\n    enter_label: \"status:done\"\n```\n"

	if err := os.MkdirAll(filepath.Join(dir, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "workflows", "dev-workflow.md"), workflowMD)

	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mockGHReader provides controllable GitHub data for tests.
type mockGHReader struct {
	mu             sync.Mutex
	issues         []poller.Issue
	prs            []poller.PR
	calls          int
	onPoll         func(call int) // callback on each ListIssues call
	labelSnapshots [][]string
	labelCalls     int
}

func (m *mockGHReader) ListIssues(_ string) ([]poller.Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.onPoll != nil {
		m.onPoll(m.calls)
	}
	return m.issues, nil
}

func (m *mockGHReader) ListPRs(_ string) ([]poller.PR, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.prs, nil
}

func (m *mockGHReader) CheckRepoAccess(_ string) error {
	return nil
}

func (m *mockGHReader) ReadIssueLabels(_ string, _ int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.labelSnapshots) == 0 {
		return nil, fmt.Errorf("no label snapshots configured")
	}
	idx := m.labelCalls
	if idx >= len(m.labelSnapshots) {
		idx = len(m.labelSnapshots) - 1
	}
	m.labelCalls++
	return append([]string(nil), m.labelSnapshots[idx]...), nil
}

func (m *mockGHReader) ReadIssue(_ string, issueNum int) (poller.IssueDetails, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, issue := range m.issues {
		if issue.Number == issueNum {
			return poller.IssueDetails{
				Number: issueNum,
				State:  issue.State,
				Body:   issue.Body,
				Labels: append([]string(nil), issue.Labels...),
			}, nil
		}
	}
	return poller.IssueDetails{Number: issueNum, State: "closed"}, nil
}

func (m *mockGHReader) SetIssues(issues []poller.Issue) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.issues = issues
}

// mockRuntime provides a controllable agent runtime for tests.
type mockRuntime struct {
	name     string
	mu       sync.Mutex
	calls    int
	countFor string
	resultFn func(ctx context.Context, agent *config.AgentConfig, task *launcher.TaskContext) (*launcher.Result, error)
}

func (m *mockRuntime) Name() string { return m.name }

type mockSession struct {
	rt    *mockRuntime
	agent *config.AgentConfig
	task  *launcher.TaskContext
}

func (s *mockSession) Run(ctx context.Context, _ chan<- launcherevents.Event) (*launcher.Result, error) {
	return s.rt.Launch(ctx, s.agent, s.task)
}

func (s *mockSession) SetApprover(launcher.Approver) error { return launcher.ErrNotSupported }
func (s *mockSession) Close() error                        { return nil }

func (m *mockRuntime) Start(ctx context.Context, agent *config.AgentConfig, task *launcher.TaskContext) (launcher.Session, error) {
	return &mockSession{rt: m, agent: agent, task: task}, nil
}

func (m *mockRuntime) Launch(ctx context.Context, agent *config.AgentConfig, task *launcher.TaskContext) (*launcher.Result, error) {
	m.mu.Lock()
	if m.countFor == "" || (agent != nil && agent.Name == m.countFor) {
		m.calls++
	}
	fn := m.resultFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, agent, task)
	}
	return &launcher.Result{
		ExitCode: 0,
		Stdout:   "mock output",
		Duration: 1 * time.Second,
		Meta:     map[string]string{},
	}, nil
}

func (m *mockRuntime) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

type mockCommentWriter struct {
	mu       sync.Mutex
	comments []string
}

func (m *mockCommentWriter) WriteComment(_ string, _ int, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.comments = append(m.comments, body)
	return nil
}

func (m *mockCommentWriter) Comments() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.comments...)
}

func setupFakeGHCLI(t *testing.T) {
	t.Helper()
	fakeBin := t.TempDir()
	ghPath := filepath.Join(fakeBin, "gh")
	writeFile(t, ghPath, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(ghPath, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func waitForHealth(t *testing.T, port int) {
	t.Helper()
	addr := fmt.Sprintf("http://localhost:%d/health", port)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(addr)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("coordinator did not become healthy at %s", addr)
}

func getFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

func newWorkerTestDeps(t *testing.T, rt *mockRuntime, readers ...issueLabelReader) (*workerDeps, *store.Store) {
	t.Helper()
	setupFakeGHCLI(t)

	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	lnch := launcher.NewLauncher()
	lnch.Register(rt, config.RuntimeClaudeCode, config.RuntimeClaudeShot)
	sessionsDir := filepath.Join(t.TempDir(), ".workbuddy", "sessions")
	lnch.SetSessionManager(launcher.NewSessionManager(sessionsDir, st))

	var issueReader issueLabelReader
	if len(readers) > 0 {
		issueReader = readers[0]
	}

	return &workerDeps{
		launcher:     lnch,
		auditor:      audit.NewAuditor(st, filepath.Join(t.TempDir(), "archive")),
		reporter:     reporter.NewReporter(&mockCommentWriter{}),
		store:        st,
		sm:           statemachine.NewStateMachine(nil, st, nil, eventlog.NewEventLogger(st)),
		workerID:     "worker-1",
		cfg:          &config.FullConfig{Workflows: map[string]*config.WorkflowConfig{"dev-workflow": {MaxRetries: 3}}},
		runningTasks: NewRunningTasks(),
		closedIssues: &closedIssues{},
		sessionsDir:  sessionsDir,
		issueReader:  issueReader,
	}, st
}

func newWorkerTestDepsWithComments(t *testing.T, rt *mockRuntime, readers ...issueLabelReader) (*workerDeps, *store.Store, *mockCommentWriter) {
	t.Helper()
	setupFakeGHCLI(t)

	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	lnch := launcher.NewLauncher()
	lnch.Register(rt, config.RuntimeClaudeCode, config.RuntimeClaudeShot)
	sessionsDir := filepath.Join(t.TempDir(), ".workbuddy", "sessions")
	lnch.SetSessionManager(launcher.NewSessionManager(sessionsDir, st))

	var issueReader issueLabelReader
	if len(readers) > 0 {
		issueReader = readers[0]
	}
	comments := &mockCommentWriter{}

	return &workerDeps{
		launcher:     lnch,
		auditor:      audit.NewAuditor(st, filepath.Join(t.TempDir(), "archive")),
		reporter:     reporter.NewReporter(comments),
		store:        st,
		sm:           statemachine.NewStateMachine(nil, st, nil, eventlog.NewEventLogger(st)),
		workerID:     "worker-1",
		cfg:          &config.FullConfig{Workflows: map[string]*config.WorkflowConfig{"dev-workflow": {MaxRetries: 3}}},
		runningTasks: NewRunningTasks(),
		closedIssues: &closedIssues{},
		sessionsDir:  sessionsDir,
		issueReader:  issueReader,
	}, st, comments
}

func TestServeDependencyGateBlocksUntilDependencyDone(t *testing.T) {
	setupFakeGHCLI(t)
	repo := "owner/repo"
	configDir := setupTestConfigDir(t, repo)
	rt := &mockRuntime{name: "mock", countFor: "dev-agent"}

	gh := &mockGHReader{}
	gh.onPoll = func(call int) {
		switch call {
		case 1:
			gh.issues = []poller.Issue{
				{Number: 1, Title: "A", State: "open", Labels: []string{"workbuddy", "status:triage"}},
				{Number: 2, Title: "B", State: "open", Labels: []string{"workbuddy", "status:developing"}, Body: "```yaml\nworkbuddy:\n  depends_on:\n    - \"#1\"\n```"},
			}
		case 2:
			gh.issues = []poller.Issue{
				{Number: 1, Title: "A", State: "open", Labels: []string{"workbuddy", "status:done"}},
				{Number: 2, Title: "B", State: "open", Labels: []string{"workbuddy", "status:blocked"}, Body: "```yaml\nworkbuddy:\n  depends_on:\n    - \"#1\"\n```"},
			}
		default:
			gh.issues = []poller.Issue{
				{Number: 1, Title: "A", State: "open", Labels: []string{"workbuddy", "status:done"}},
				{Number: 2, Title: "B", State: "open", Labels: []string{"workbuddy", "status:developing"}, Body: "```yaml\nworkbuddy:\n  depends_on:\n    - \"#1\"\n```"},
			}
		}
	}

	lnch := launcher.NewLauncher()
	lnch.Register(rt, config.RuntimeClaudeCode, config.RuntimeClaudeShot)
	opts := &serveOpts{
		port:             18939,
		pollInterval:     40 * time.Millisecond,
		maxParallelTasks: 1,
		roles:            []string{"dev"},
		configDir:        configDir,
		dbPath:           filepath.Join(t.TempDir(), "workbuddy.db"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()

	if err := runServeWithOpts(opts, gh, lnch, ctx); err != nil {
		t.Fatalf("runServeWithOpts: %v", err)
	}
	if calls := rt.CallCount(); calls != 1 {
		t.Fatalf("runtime call count=%d want 1", calls)
	}
}

func newWorkerTestTask(t *testing.T, st *store.Store, repo string, issueNum int, taskID string) router.WorkerTask {
	t.Helper()
	repoRoot := t.TempDir()

	if err := st.InsertTask(store.TaskRecord{
		ID:        taskID,
		Repo:      repo,
		IssueNum:  issueNum,
		AgentName: "dev-agent",
		Status:    store.TaskStatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     repo,
		IssueNum: issueNum,
		Labels:   `["workbuddy","status:developing"]`,
		State:    "open",
	}); err != nil {
		t.Fatal(err)
	}

	return router.WorkerTask{
		TaskID:    taskID,
		Repo:      repo,
		IssueNum:  issueNum,
		AgentName: "dev-agent",
		Agent: &config.AgentConfig{
			Name:    "dev-agent",
			Runtime: config.RuntimeClaudeCode,
			Prompt:  "test prompt",
		},
		Workflow: "dev-workflow",
		State:    "developing",
		Context: &launcher.TaskContext{
			Repo:     repo,
			RepoRoot: repoRoot,
			WorkDir:  t.TempDir(),
			Issue: launcher.IssueContext{
				Number: issueNum,
				Title:  fmt.Sprintf("Issue %d", issueNum),
			},
			Session: launcher.SessionContext{ID: "session-" + taskID},
		},
	}
}

func taskStatusesByID(t *testing.T, st *store.Store) map[string]string {
	t.Helper()

	activeTasks, err := st.QueryTasks("")
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(map[string]string, len(activeTasks))
	for _, task := range activeTasks {
		statuses[task.ID] = task.Status
	}

	completedTasks, err := st.QueryTasks(store.TaskStatusCompleted)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range completedTasks {
		statuses[task.ID] = task.Status
	}

	return statuses
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestServe_GracefulShutdown verifies that serve starts and stops cleanly.
func TestServe_GracefulShutdown(t *testing.T) {
	repo := "owner/test-repo"
	configDir := setupTestConfigDir(t, repo)
	dbPath := filepath.Join(t.TempDir(), "test.db")

	gh := &mockGHReader{}

	mockRT := &mockRuntime{name: "claude-code"}
	lnch := launcher.NewLauncher()
	lnch.Register(mockRT)

	opts := &serveOpts{
		port:         18930,
		pollInterval: 500 * time.Millisecond,
		roles:        []string{"dev"},
		configDir:    configDir,
		dbPath:       dbPath,
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		errCh <- runServeWithOpts(opts, gh, lnch, ctx)
	}()

	// Let it run briefly, then cancel
	time.Sleep(1 * time.Second)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serve returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit within timeout")
	}
}

// TestServe_RecoverTasks verifies restart recovery marks running tasks as failed.
func TestServe_RecoverTasks(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// Insert a "running" task
	if err := st.InsertTask(store.TaskRecord{
		ID:        "task-running-1",
		Repo:      "owner/repo",
		IssueNum:  1,
		AgentName: "dev-agent",
		WorkerID:  "worker-1",
		Status:    store.TaskStatusRunning,
	}); err != nil {
		t.Fatal(err)
	}

	// Insert a "pending" task
	if err := st.InsertTask(store.TaskRecord{
		ID:        "task-pending-1",
		Repo:      "owner/repo",
		IssueNum:  2,
		AgentName: "dev-agent",
		Status:    store.TaskStatusPending,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen store and run recovery
	st2, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()

	if err := recoverTasks(st2); err != nil {
		t.Fatalf("recoverTasks: %v", err)
	}

	// Verify running task is now failed
	tasks, err := st2.QueryTasks(store.TaskStatusFailed)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != "task-running-1" {
		t.Errorf("expected 1 failed task (task-running-1), got %d tasks", len(tasks))
	}

	// Verify pending task is still pending
	pending, err := st2.QueryTasks(store.TaskStatusPending)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "task-pending-1" {
		t.Errorf("expected 1 pending task (task-pending-1), got %d", len(pending))
	}
}

// TestServe_HealthEndpoint verifies the /health endpoint works.
func TestServe_HealthEndpoint(t *testing.T) {
	repo := "owner/test-repo"
	configDir := setupTestConfigDir(t, repo)
	dbPath := filepath.Join(t.TempDir(), "test.db")

	port := 18932

	gh := &mockGHReader{}
	mockRT := &mockRuntime{name: "claude-code"}
	lnch := launcher.NewLauncher()
	lnch.Register(mockRT)

	opts := &serveOpts{
		port:         port,
		pollInterval: 5 * time.Second,
		roles:        []string{"dev"},
		configDir:    configDir,
		dbPath:       dbPath,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runServeWithOpts(opts, gh, lnch, ctx)
	}()

	// Give server time to start
	time.Sleep(500 * time.Millisecond)

	// Check /health
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/health", port))
	if err != nil {
		cancel()
		t.Fatalf("GET /health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var healthResp map[string]string
	if err := json.Unmarshal(body, &healthResp); err != nil {
		t.Fatalf("unmarshal health response: %v", err)
	}
	if healthResp["status"] != "ok" {
		t.Errorf("expected status=ok, got %s", healthResp["status"])
	}
	if healthResp["repo"] != repo {
		t.Errorf("expected repo=%s, got %s", repo, healthResp["repo"])
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serve returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit within timeout")
	}
}

func TestServe_AuditEndpoints(t *testing.T) {
	repo := "owner/test-repo"
	configDir := setupTestConfigDir(t, repo)
	dbPath := filepath.Join(t.TempDir(), "audit.db")
	port := 18933

	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rawPath, err := seedServeAuditFixture(st, repo)
	if err != nil {
		_ = st.Close()
		t.Fatalf("seedServeAuditFixture: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close seed store: %v", err)
	}

	gh := &mockGHReader{
		issues: []poller.Issue{
			{Number: 40, Title: "audit", State: "open", Labels: []string{"status:reviewing"}, Body: "seed"},
		},
	}
	mockRT := &mockRuntime{name: "claude-code"}
	lnch := launcher.NewLauncher()
	lnch.Register(mockRT)

	opts := &serveOpts{
		port:         port,
		pollInterval: 5 * time.Second,
		roles:        []string{"dev"},
		configDir:    configDir,
		dbPath:       dbPath,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runServeWithOpts(opts, gh, lnch, ctx)
	}()

	time.Sleep(500 * time.Millisecond)

	t.Run("events", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/events?repo=%s&issue=40&type=dispatch", port, repo))
		if err != nil {
			t.Fatalf("GET /events: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var body struct {
			Events []struct {
				Type    string         `json:"type"`
				Payload map[string]any `json:"payload"`
			} `json:"events"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Events) != 1 {
			t.Fatalf("events = %d, want 1", len(body.Events))
		}
		if body.Events[0].Type != "dispatch" {
			t.Fatalf("type = %q", body.Events[0].Type)
		}
		if body.Events[0].Payload["agent"] != "dev-agent" {
			t.Fatalf("payload = %#v", body.Events[0].Payload)
		}
	})

	t.Run("issue state", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/issues/%s/40/state", port, repo))
		if err != nil {
			t.Fatalf("GET /issues/.../state: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		var body struct {
			Repo              string `json:"repo"`
			IssueNum          int    `json:"issue_num"`
			CycleCount        int    `json:"cycle_count"`
			DependencyVerdict string `json:"dependency_verdict"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Repo != repo || body.IssueNum != 40 {
			t.Fatalf("repo/issue = %s/%d", body.Repo, body.IssueNum)
		}
		if body.CycleCount != 1 {
			t.Fatalf("cycle_count = %d", body.CycleCount)
		}
		if body.DependencyVerdict == "" {
			t.Fatal("dependency_verdict should not be empty")
		}
	})

	t.Run("session detail json", func(t *testing.T) {
		req, err := http.NewRequest("GET", fmt.Sprintf("http://localhost:%d/sessions/session-40?format=json", port), nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Accept", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /sessions/session-40: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var body struct {
			SessionID     string `json:"session_id"`
			ArtifactPaths struct {
				EventsV1 string `json:"events_v1"`
				Raw      string `json:"raw"`
			} `json:"artifact_paths"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.SessionID != "session-40" {
			t.Fatalf("session_id = %q", body.SessionID)
		}
		if !strings.HasSuffix(body.ArtifactPaths.EventsV1, filepath.Join("session-40", "events-v1.jsonl")) {
			t.Fatalf("events path = %q", body.ArtifactPaths.EventsV1)
		}
		if body.ArtifactPaths.Raw != rawPath {
			t.Fatalf("raw path = %q, want %q", body.ArtifactPaths.Raw, rawPath)
		}
	})

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serve returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit within timeout")
	}
}

func TestServe_StatusWatchIntegration(t *testing.T) {
	setupFakeGHCLI(t)

	repo := "owner/watch-repo"
	configDir := setupTestConfigDir(t, repo)
	dbPath := filepath.Join(t.TempDir(), "watch.db")
	port := getFreePort(t)
	release := make(chan struct{})

	gh := &mockGHReader{
		issues: []poller.Issue{
			{Number: 41, Title: "watch", State: "open", Labels: []string{"workbuddy", "status:developing"}, Body: "watch me"},
		},
	}
	mockRT := &mockRuntime{name: "claude-code", resultFn: func(ctx context.Context, _ *config.AgentConfig, _ *launcher.TaskContext) (*launcher.Result, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &launcher.Result{ExitCode: 0, Duration: 2 * time.Second, Meta: map[string]string{}}, nil
	}}
	lnch := launcher.NewLauncher()
	lnch.Register(mockRT)

	opts := &serveOpts{
		port:         port,
		pollInterval: 20 * time.Millisecond,
		roles:        []string{"dev"},
		configDir:    configDir,
		dbPath:       dbPath,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runServeWithOpts(opts, gh, lnch, ctx)
	}()
	waitForHealth(t, port)

	client := &statusClient{
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		http:    &http.Client{Timeout: 5 * time.Second},
	}
	var out bytes.Buffer
	watchErrCh := make(chan error, 1)
	go func() {
		watchErrCh <- runStatusWithOpts(context.Background(), &statusOpts{
			repo:    repo,
			watch:   true,
			timeout: 5 * time.Second,
			baseURL: client.baseURL,
		}, client, &out)
	}()

	time.Sleep(150 * time.Millisecond)
	close(release)

	select {
	case err := <-watchErrCh:
		if err != nil {
			t.Fatalf("runStatusWithOpts: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not complete")
	}

	got := out.String()
	for _, want := range []string{"Waiting for task completion...", "ISSUE", "#41", "completed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("watch output missing %q:\n%s", want, got)
		}
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serve returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not exit")
	}
}

func seedServeAuditFixture(st *store.Store, repo string) (string, error) {
	if _, err := st.InsertEvent(store.Event{
		Type:     "dispatch",
		Repo:     repo,
		IssueNum: 40,
		Payload:  `{"agent":"dev-agent"}`,
	}); err != nil {
		return "", err
	}
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     repo,
		IssueNum: 40,
		Labels:   `["status:reviewing","type:feature"]`,
		State:    "open",
	}); err != nil {
		return "", err
	}
	if _, err := st.IncrementTransition(repo, 40, "developing", "reviewing"); err != nil {
		return "", err
	}
	if _, err := st.IncrementTransition(repo, 40, "reviewing", "developing"); err != nil {
		return "", err
	}
	if err := st.UpsertIssueDependencyState(store.IssueDependencyState{
		Repo:              repo,
		IssueNum:          40,
		Verdict:           store.DependencyVerdictBlocked,
		ResumeLabel:       "status:developing",
		BlockedReasonHash: "abc123",
		GraphVersion:      7,
	}); err != nil {
		return "", err
	}

	repoRoot, err := os.Getwd()
	if err != nil {
		return "", err
	}
	sessionDir := filepath.Join(repoRoot, ".workbuddy", "sessions", "session-40")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "events-v1.jsonl"), []byte("{\"kind\":\"log\"}\n"), 0o644); err != nil {
		return "", err
	}
	rawPath := filepath.Join(sessionDir, "codex-exec.jsonl")
	if err := os.WriteFile(rawPath, []byte("{\"type\":\"task_started\"}\n"), 0o644); err != nil {
		return "", err
	}
	if _, err := st.InsertAgentSession(store.AgentSession{
		SessionID: "session-40",
		TaskID:    "task-40",
		Repo:      repo,
		IssueNum:  40,
		AgentName: "dev-agent",
		Summary:   "summary",
		RawPath:   rawPath,
	}); err != nil {
		return "", err
	}
	return rawPath, nil
}

// TestRunningTasks_RegisterCancelRemove verifies the RunningTasks registry.
func TestRunningTasks_RegisterCancelRemove(t *testing.T) {
	rt := NewRunningTasks()

	// Cancel non-existent task returns false.
	if rt.Cancel("owner/repo", 1) {
		t.Error("expected Cancel to return false for unregistered task")
	}

	// Register a task with a cancellable context.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt.Register("owner/repo", 1, cancel)

	// Cancel should return true and actually cancel the context.
	if !rt.Cancel("owner/repo", 1) {
		t.Error("expected Cancel to return true for registered task")
	}
	if ctx.Err() == nil {
		t.Error("expected context to be cancelled after Cancel()")
	}

	// Second cancel should return false (already removed).
	if rt.Cancel("owner/repo", 1) {
		t.Error("expected Cancel to return false after already cancelled")
	}
}

// TestRunningTasks_Remove verifies that Remove prevents future Cancel.
func TestRunningTasks_Remove(t *testing.T) {
	rt := NewRunningTasks()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt.Register("owner/repo", 5, cancel)

	// Remove without cancelling.
	rt.Remove("owner/repo", 5)

	// Cancel should now return false.
	if rt.Cancel("owner/repo", 5) {
		t.Error("expected Cancel to return false after Remove")
	}

	// Context should NOT have been cancelled by Remove.
	if ctx.Err() != nil {
		t.Error("expected context to still be active after Remove (not Cancel)")
	}
}

// TestRunningTasks_Concurrent verifies thread safety.
func TestRunningTasks_Concurrent(t *testing.T) {
	rt := NewRunningTasks()
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, cancel := context.WithCancel(context.Background())
			rt.Register("owner/repo", n, cancel)
			rt.Cancel("owner/repo", n)
			rt.Remove("owner/repo", n)
		}(i)
	}
	wg.Wait()
}

func TestRunEmbeddedWorker_ParallelAcrossIssues(t *testing.T) {
	started := make(chan int, 2)
	release := make(chan struct{})

	mockRT := &mockRuntime{name: config.RuntimeClaudeCode, resultFn: func(_ context.Context, _ *config.AgentConfig, task *launcher.TaskContext) (*launcher.Result, error) {
		started <- task.Issue.Number
		<-release
		return &launcher.Result{
			ExitCode: 0,
			Duration: 50 * time.Millisecond,
			Meta:     map[string]string{},
		}, nil
	}}
	deps, st := newWorkerTestDeps(t, mockRT)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	taskCh := make(chan router.WorkerTask, 2)
	done := make(chan struct{})
	go func() {
		runEmbeddedWorker(ctx, taskCh, deps, 2)
		close(done)
	}()

	taskCh <- newWorkerTestTask(t, st, "owner/repo", 1, "task-1")
	taskCh <- newWorkerTestTask(t, st, "owner/repo", 2, "task-2")
	close(taskCh)

	seen := map[int]bool{}
	for len(seen) < 2 {
		select {
		case issueNum := <-started:
			seen[issueNum] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("expected both issues to start concurrently, saw %v", seen)
		}
	}

	close(release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("embedded worker did not exit after completing tasks")
	}

	statuses := taskStatusesByID(t, st)
	if statuses["task-1"] != store.TaskStatusCompleted {
		t.Fatalf("task-1 status = %q, want %q", statuses["task-1"], store.TaskStatusCompleted)
	}
	if statuses["task-2"] != store.TaskStatusCompleted {
		t.Fatalf("task-2 status = %q, want %q", statuses["task-2"], store.TaskStatusCompleted)
	}
}

func TestRunEmbeddedWorker_SerializesTasksWithinIssue(t *testing.T) {
	firstStarted := make(chan struct{}, 1)
	secondStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})

	var mu sync.Mutex
	callCount := 0
	mockRT := &mockRuntime{name: config.RuntimeClaudeCode, resultFn: func(_ context.Context, _ *config.AgentConfig, _ *launcher.TaskContext) (*launcher.Result, error) {
		mu.Lock()
		callCount++
		callNum := callCount
		mu.Unlock()

		switch callNum {
		case 1:
			firstStarted <- struct{}{}
			<-releaseFirst
		case 2:
			secondStarted <- struct{}{}
			<-releaseSecond
		default:
			t.Fatalf("unexpected runtime call %d", callNum)
		}

		return &launcher.Result{
			ExitCode: 0,
			Duration: 50 * time.Millisecond,
			Meta:     map[string]string{},
		}, nil
	}}
	deps, st := newWorkerTestDeps(t, mockRT)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	taskCh := make(chan router.WorkerTask, 2)
	done := make(chan struct{})
	go func() {
		runEmbeddedWorker(ctx, taskCh, deps, 2)
		close(done)
	}()

	taskCh <- newWorkerTestTask(t, st, "owner/repo", 7, "task-1")
	taskCh <- newWorkerTestTask(t, st, "owner/repo", 7, "task-2")
	close(taskCh)

	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first task did not start")
	}

	select {
	case <-secondStarted:
		t.Fatal("second task started before the first one completed")
	case <-time.After(250 * time.Millisecond):
	}

	close(releaseFirst)

	select {
	case <-secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("second task did not start after the first one completed")
	}

	close(releaseSecond)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("embedded worker did not exit after serial tasks completed")
	}

	statuses := taskStatusesByID(t, st)
	if statuses["task-1"] != store.TaskStatusCompleted {
		t.Fatalf("task-1 status = %q, want %q", statuses["task-1"], store.TaskStatusCompleted)
	}
	if statuses["task-2"] != store.TaskStatusCompleted {
		t.Fatalf("task-2 status = %q, want %q", statuses["task-2"], store.TaskStatusCompleted)
	}
}

func TestRunEmbeddedWorker_CancelOnlyStopsMatchingIssue(t *testing.T) {
	started := make(chan int, 2)
	cancelled := make(chan int, 1)
	releaseIssue2 := make(chan struct{})

	mockRT := &mockRuntime{name: config.RuntimeClaudeCode, resultFn: func(ctx context.Context, _ *config.AgentConfig, task *launcher.TaskContext) (*launcher.Result, error) {
		started <- task.Issue.Number
		switch task.Issue.Number {
		case 1:
			<-ctx.Done()
			cancelled <- task.Issue.Number
			return nil, ctx.Err()
		case 2:
			<-releaseIssue2
			return &launcher.Result{
				ExitCode: 0,
				Duration: 50 * time.Millisecond,
				Meta:     map[string]string{},
			}, nil
		default:
			t.Fatalf("unexpected issue number %d", task.Issue.Number)
			return nil, nil
		}
	}}
	deps, st := newWorkerTestDeps(t, mockRT)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	taskCh := make(chan router.WorkerTask, 2)
	done := make(chan struct{})
	go func() {
		runEmbeddedWorker(ctx, taskCh, deps, 2)
		close(done)
	}()

	taskCh <- newWorkerTestTask(t, st, "owner/repo", 1, "task-1")
	taskCh <- newWorkerTestTask(t, st, "owner/repo", 2, "task-2")
	close(taskCh)

	seen := map[int]bool{}
	for len(seen) < 2 {
		select {
		case issueNum := <-started:
			seen[issueNum] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("expected both issues to start before cancel, saw %v", seen)
		}
	}

	if !deps.runningTasks.Cancel("owner/repo", 1) {
		t.Fatal("expected cancel for issue #1 to succeed")
	}

	select {
	case issueNum := <-cancelled:
		if issueNum != 1 {
			t.Fatalf("cancelled issue = %d, want 1", issueNum)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("issue #1 task was not cancelled")
	}

	select {
	case <-time.After(250 * time.Millisecond):
	case issueNum := <-cancelled:
		t.Fatalf("unexpected additional cancellation for issue #%d", issueNum)
	}

	close(releaseIssue2)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("embedded worker did not exit after cancel test")
	}

	statuses := taskStatusesByID(t, st)
	if statuses["task-1"] != store.TaskStatusFailed {
		t.Fatalf("task-1 status = %q, want %q", statuses["task-1"], store.TaskStatusFailed)
	}
	if statuses["task-2"] != store.TaskStatusCompleted {
		t.Fatalf("task-2 status = %q, want %q", statuses["task-2"], store.TaskStatusCompleted)
	}
}

func TestRunEmbeddedWorker_SkipsQueuedTaskAfterIssueClose(t *testing.T) {
	firstStarted := make(chan struct{}, 1)
	secondStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})

	var mu sync.Mutex
	callCount := 0
	mockRT := &mockRuntime{name: config.RuntimeClaudeCode, resultFn: func(ctx context.Context, _ *config.AgentConfig, _ *launcher.TaskContext) (*launcher.Result, error) {
		mu.Lock()
		callCount++
		callNum := callCount
		mu.Unlock()

		switch callNum {
		case 1:
			firstStarted <- struct{}{}
			<-ctx.Done()
			<-releaseFirst
			return nil, ctx.Err()
		case 2:
			secondStarted <- struct{}{}
			return &launcher.Result{
				ExitCode: 0,
				Duration: 50 * time.Millisecond,
				Meta:     map[string]string{},
			}, nil
		default:
			t.Fatalf("unexpected runtime call %d", callNum)
			return nil, nil
		}
	}}
	deps, st := newWorkerTestDeps(t, mockRT)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	taskCh := make(chan router.WorkerTask, 2)
	done := make(chan struct{})
	go func() {
		runEmbeddedWorker(ctx, taskCh, deps, 2)
		close(done)
	}()

	taskCh <- newWorkerTestTask(t, st, "owner/repo", 9, "task-1")
	taskCh <- newWorkerTestTask(t, st, "owner/repo", 9, "task-2")
	close(taskCh)

	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first task did not start")
	}

	deps.closedIssues.MarkClosed("owner/repo", 9)
	if err := st.DeleteIssueCache("owner/repo", 9); err != nil {
		t.Fatal(err)
	}
	if !deps.runningTasks.Cancel("owner/repo", 9) {
		t.Fatal("expected cancel for issue #9 to succeed")
	}
	close(releaseFirst)

	select {
	case <-secondStarted:
		t.Fatal("queued same-issue task started after the issue was closed")
	case <-time.After(300 * time.Millisecond):
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("embedded worker did not exit after closing queued issue")
	}

	statuses := taskStatusesByID(t, st)
	if statuses["task-1"] != store.TaskStatusFailed {
		t.Fatalf("task-1 status = %q, want %q", statuses["task-1"], store.TaskStatusFailed)
	}
	if statuses["task-2"] != store.TaskStatusFailed {
		t.Fatalf("task-2 status = %q, want %q", statuses["task-2"], store.TaskStatusFailed)
	}
}

func TestExecuteTask_PersistsPartialResultOnRunError(t *testing.T) {
	fakeBin := t.TempDir()
	ghPath := filepath.Join(fakeBin, "gh")
	writeFile(t, ghPath, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(ghPath, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	workdir := t.TempDir()
	repoRoot := t.TempDir()
	sessionID := "session-partial"
	artifactDir := filepath.Join(repoRoot, ".workbuddy", "sessions", sessionID)
	artifactPath := filepath.Join(artifactDir, "codex-exec.jsonl")
	writeFile(t, artifactPath, "{\"type\":\"task_started\"}\n")

	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	if err := st.InsertTask(store.TaskRecord{
		ID:        "task-1",
		Repo:      "owner/repo",
		IssueNum:  8,
		AgentName: "dev-agent",
		Status:    store.TaskStatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: 8,
		Labels:   `["workbuddy","status:developing"]`,
		State:    "open",
	}); err != nil {
		t.Fatal(err)
	}

	gh := &mockCommentWriter{}
	mockRT := &mockRuntime{name: config.RuntimeCodexExec, resultFn: func(ctx context.Context, agent *config.AgentConfig, task *launcher.TaskContext) (*launcher.Result, error) {
		return &launcher.Result{
			ExitCode:    -1,
			LastMessage: "partial failure report",
			Stderr:      "runtime failed after writing artifacts",
			Duration:    time.Second,
			SessionPath: artifactPath,
		}, fmt.Errorf("runtime failed")
	}}
	lnch := launcher.NewLauncher()
	lnch.Register(mockRT, config.RuntimeCodex, config.RuntimeCodexExec)

	sm := statemachine.NewStateMachine(nil, st, nil, eventlog.NewEventLogger(st))
	deps := &workerDeps{
		launcher: lnch,
		auditor:  audit.NewAuditor(st, filepath.Join(t.TempDir(), "archive")),
		reporter: reporter.NewReporter(gh),
		store:    st,
		sm:       sm,
		workerID: "worker-1",
		cfg:      &config.FullConfig{},
	}

	task := router.WorkerTask{
		TaskID:    "task-1",
		Repo:      "owner/repo",
		IssueNum:  8,
		AgentName: "dev-agent",
		Agent:     &config.AgentConfig{Name: "dev-agent", Runtime: config.RuntimeCodexExec, Prompt: "test prompt"},
		Workflow:  "dev-workflow",
		State:     "developing",
		Context:   &launcher.TaskContext{Repo: "owner/repo", RepoRoot: repoRoot, WorkDir: workdir, Session: launcher.SessionContext{ID: sessionID}},
	}

	executeTask(context.Background(), task, deps)

	tasks, err := st.QueryTasks("")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Status != store.TaskStatusFailed {
		t.Fatalf("task status = %+v", tasks)
	}

	sessions, err := deps.auditor.Query(audit.Filter{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 archived session, got %d", len(sessions))
	}
	if sessions[0].RawPath == "" {
		t.Fatalf("expected archived raw path, got %+v", sessions[0])
	}
	if _, err := os.Stat(sessions[0].RawPath); err != nil {
		t.Fatalf("archived raw path missing: %v", err)
	}
	if !strings.HasPrefix(artifactPath, repoRoot) {
		t.Fatalf("runtime artifact path = %q, want under repo root %q", artifactPath, repoRoot)
	}
	if strings.HasPrefix(artifactPath, workdir) {
		t.Fatalf("runtime artifact path should not remain under workdir: %q", artifactPath)
	}

	comments := gh.Comments()
	if len(comments) != 2 {
		t.Fatalf("expected started + final report comments, got %d", len(comments))
	}
	if !strings.Contains(comments[1], "partial failure report") {
		t.Fatalf("final report missing partial result: %s", comments[1])
	}
}

func TestExecuteTask_LabelValidationAudit(t *testing.T) {
	tests := []struct {
		name               string
		pre                []string
		post               []string
		exitCode           int
		wantClassification string
		wantSummary        string
		wantNeedsHuman     bool
	}{
		{
			name:               "allowed transition",
			pre:                []string{"workbuddy", "status:developing"},
			post:               []string{"workbuddy", "status:reviewing"},
			exitCode:           0,
			wantClassification: "ok",
			wantSummary:        "Label transition: developing -> reviewing (OK)",
		},
		{
			name:               "no transition after success",
			pre:                []string{"workbuddy", "status:developing"},
			post:               []string{"workbuddy", "status:developing"},
			exitCode:           0,
			wantClassification: "no_transition_after_success",
			wantSummary:        "Label transition: none - needs human review",
			wantNeedsHuman:     true,
		},
		{
			name:               "no transition after failure",
			pre:                []string{"workbuddy", "status:developing"},
			post:               []string{"workbuddy", "status:developing"},
			exitCode:           1,
			wantClassification: "no_transition_after_failure",
			wantSummary:        "Label transition: none - retry path",
		},
		{
			name:               "unexpected transition",
			pre:                []string{"workbuddy", "status:developing"},
			post:               []string{"workbuddy", "status:done"},
			exitCode:           0,
			wantClassification: "unexpected_transition",
			wantSummary:        "Label transition: developing -> done (unexpected)",
		},
		{
			name:               "failed label",
			pre:                []string{"workbuddy", "status:developing"},
			post:               []string{"workbuddy", "status:failed"},
			exitCode:           1,
			wantClassification: "failed",
			wantSummary:        "Label transition: developing -> failed (failed)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &mockRuntime{name: config.RuntimeClaudeCode, resultFn: func(_ context.Context, _ *config.AgentConfig, _ *launcher.TaskContext) (*launcher.Result, error) {
				return &launcher.Result{
					ExitCode: tt.exitCode,
					Stdout:   "task output",
					Duration: 50 * time.Millisecond,
					Meta:     map[string]string{},
				}, nil
			}}
			gh := &mockGHReader{
				labelSnapshots: [][]string{tt.pre, tt.post},
			}
			deps, st, comments := newWorkerTestDepsWithComments(t, rt, gh)

			deps.cfg.Workflows["dev-workflow"] = &config.WorkflowConfig{
				Name:       "dev-workflow",
				MaxRetries: 3,
				States: map[string]*config.State{
					"developing": {
						EnterLabel: "status:developing",
						Agent:      "dev-agent",
						Transitions: []config.Transition{
							{To: "reviewing", When: `labeled "status:reviewing"`},
						},
					},
					"reviewing": {EnterLabel: "status:reviewing", Agent: "review-agent"},
					"done":      {EnterLabel: "status:done"},
					"failed":    {EnterLabel: "status:failed"},
				},
			}

			task := newWorkerTestTask(t, st, "owner/repo", 11, "task-label-check")
			executeTask(context.Background(), task, deps)

			events, err := st.QueryEvents("owner/repo")
			if err != nil {
				t.Fatalf("QueryEvents: %v", err)
			}

			var validationEvents []store.Event
			for _, event := range events {
				if event.Type == string(audit.EventKindLabelValidation) {
					validationEvents = append(validationEvents, event)
				}
			}
			if len(validationEvents) != 1 {
				t.Fatalf("expected 1 label validation event, got %d", len(validationEvents))
			}

			var payload audit.LabelValidationPayload
			if err := json.Unmarshal([]byte(validationEvents[0].Payload), &payload); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}
			if payload.Classification != tt.wantClassification {
				t.Fatalf("Classification = %q, want %q", payload.Classification, tt.wantClassification)
			}

			allComments := comments.Comments()
			if len(allComments) == 0 {
				t.Fatal("expected issue comments to be posted")
			}
			lastComment := allComments[len(allComments)-1]
			if !strings.Contains(lastComment, tt.wantSummary) {
				t.Fatalf("final report missing label summary %q: %s", tt.wantSummary, lastComment)
			}

			if tt.wantNeedsHuman {
				if len(allComments) != 3 {
					t.Fatalf("expected started + managed + final comments, got %d", len(allComments))
				}
				if !strings.Contains(allComments[1], "needs-human") {
					t.Fatalf("managed comment missing needs-human recommendation: %s", allComments[1])
				}
			} else if len(allComments) != 2 {
				t.Fatalf("expected started + final comments, got %d", len(allComments))
			}
		})
	}
}

func TestExecuteTask_LabelValidationUsesPreRunStateTransitions(t *testing.T) {
	rt := &mockRuntime{name: config.RuntimeClaudeCode, resultFn: func(_ context.Context, _ *config.AgentConfig, _ *launcher.TaskContext) (*launcher.Result, error) {
		return &launcher.Result{
			ExitCode: 0,
			Stdout:   "task output",
			Duration: 50 * time.Millisecond,
			Meta:     map[string]string{},
		}, nil
	}}
	gh := &mockGHReader{
		labelSnapshots: [][]string{
			{"workbuddy", "status:reviewing"},
			{"workbuddy", "status:done"},
		},
	}
	deps, st, comments := newWorkerTestDepsWithComments(t, rt, gh)

	deps.cfg.Workflows["dev-workflow"] = &config.WorkflowConfig{
		Name:       "dev-workflow",
		MaxRetries: 3,
		States: map[string]*config.State{
			"developing": {
				EnterLabel: "status:developing",
				Agent:      "dev-agent",
				Transitions: []config.Transition{
					{To: "reviewing", When: `labeled "status:reviewing"`},
				},
			},
			"reviewing": {
				EnterLabel: "status:reviewing",
				Agent:      "review-agent",
				Transitions: []config.Transition{
					{To: "done", When: `labeled "status:done"`},
				},
			},
			"done":   {EnterLabel: "status:done"},
			"failed": {EnterLabel: "status:failed"},
		},
	}

	task := newWorkerTestTask(t, st, "owner/repo", 12, "task-stale-state")
	task.State = "developing"

	executeTask(context.Background(), task, deps)

	events, err := st.QueryEvents("owner/repo")
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}

	var validationEvents []store.Event
	for _, event := range events {
		if event.Type == string(audit.EventKindLabelValidation) {
			validationEvents = append(validationEvents, event)
		}
	}
	if len(validationEvents) != 1 {
		t.Fatalf("expected 1 label validation event, got %d", len(validationEvents))
	}

	var payload audit.LabelValidationPayload
	if err := json.Unmarshal([]byte(validationEvents[0].Payload), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Classification != "ok" {
		t.Fatalf("Classification = %q, want %q", payload.Classification, "ok")
	}

	allComments := comments.Comments()
	if len(allComments) != 2 {
		t.Fatalf("expected started + final comments, got %d", len(allComments))
	}
	if !strings.Contains(allComments[1], "Label transition: reviewing -> done (OK)") {
		t.Fatalf("final report missing resolved transition summary: %s", allComments[1])
	}
}

// TestExecuteTask_RecordsSessionEarly verifies that the session row is
// inserted into agent_sessions before the agent finishes, so the web UI
// can display running sessions.
func TestExecuteTask_RecordsSessionEarly(t *testing.T) {
	sessionID := "sess-early-001"
	rt := &mockRuntime{name: config.RuntimeClaudeCode, resultFn: func(_ context.Context, _ *config.AgentConfig, _ *launcher.TaskContext) (*launcher.Result, error) {
		return &launcher.Result{
			ExitCode: 0,
			Stdout:   "done",
			Duration: 50 * time.Millisecond,
			Meta:     map[string]string{},
		}, nil
	}}
	deps, st := newWorkerTestDeps(t, rt)

	task := router.WorkerTask{
		TaskID:    "task-early",
		Repo:      "owner/repo",
		IssueNum:  99,
		AgentName: "dev-agent",
		Agent:     &config.AgentConfig{Name: "dev-agent", Runtime: config.RuntimeClaudeCode, Prompt: "fix"},
		Workflow:  "dev-workflow",
		State:     "developing",
		Context:   &launcher.TaskContext{Repo: "owner/repo", RepoRoot: t.TempDir(), WorkDir: t.TempDir(), Session: launcher.SessionContext{ID: sessionID}},
	}

	// We can't easily inspect the DB mid-run in a single goroutine, but we
	// can verify that after executeTask returns, the auditor updated the
	// pre-existing row rather than creating a duplicate.
	executeTask(context.Background(), task, deps)

	sessions, err := st.ListAgentSessions(store.SessionFilter{Repo: "owner/repo"})
	if err != nil {
		t.Fatalf("ListAgentSessions: %v", err)
	}
	var found int
	for _, s := range sessions {
		if s.SessionID == sessionID {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("expected exactly 1 session row for %s, got %d", sessionID, found)
	}
}

func TestStreamSessionEventsUsesRepoRoot(t *testing.T) {
	repoRoot := t.TempDir()
	workDir := t.TempDir()
	taskCtx := &launcher.TaskContext{
		RepoRoot: repoRoot,
		WorkDir:  workDir,
		Session:  launcher.SessionContext{ID: "session-123"},
	}
	manager := launcher.NewSessionManager(filepath.Join(repoRoot, ".workbuddy", "sessions"), nil)
	handle, err := manager.Create(launcher.SessionCreateInput{
		SessionID: taskCtx.Session.ID,
		Repo:      "owner/repo",
		IssueNum:  1,
		AgentName: "dev-agent",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	taskCtx.SetSessionHandle(handle)
	eventsCh := make(chan launcherevents.Event, 1)
	path, wait := streamSessionEvents(taskCtx, eventsCh)
	eventsCh <- launcherevents.Event{Kind: launcherevents.KindLog}
	close(eventsCh)
	if err := wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}

	want := filepath.Join(repoRoot, ".workbuddy", "sessions", taskCtx.Session.ID, "events-v1.jsonl")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected events file: %v", err)
	}
	if strings.HasPrefix(path, workDir) {
		t.Fatalf("events path should not live under workdir: %q", path)
	}
}
