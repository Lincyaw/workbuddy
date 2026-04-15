package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	mu     sync.Mutex
	issues []poller.Issue
	prs    []poller.PR
	calls  int
	onPoll func(call int) // callback on each ListIssues call
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
	m.calls++
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

func newWorkerTestDeps(t *testing.T, rt *mockRuntime) (*workerDeps, *store.Store) {
	t.Helper()
	setupFakeGHCLI(t)

	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	lnch := launcher.NewLauncher()
	lnch.Register(rt, config.RuntimeClaudeCode, config.RuntimeClaudeShot)

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
		sessionsDir:  filepath.Join(t.TempDir(), ".workbuddy", "sessions"),
	}, st
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

	tasks, err := st.QueryTasks("")
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(map[string]string, len(tasks))
	for _, task := range tasks {
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
		AgentName: "codex-dev-agent",
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
		AgentName: "codex-dev-agent",
		Agent:     &config.AgentConfig{Name: "codex-dev-agent", Runtime: config.RuntimeCodexExec, Prompt: "test prompt"},
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

func TestStreamSessionEventsUsesRepoRoot(t *testing.T) {
	repoRoot := t.TempDir()
	workDir := t.TempDir()
	taskCtx := &launcher.TaskContext{
		RepoRoot: repoRoot,
		WorkDir:  workDir,
		Session:  launcher.SessionContext{ID: "session-123"},
	}
	eventsCh := make(chan launcherevents.Event, 1)
	path, wait := streamSessionEvents(filepath.Join(t.TempDir(), "ignored", ".workbuddy", "sessions"), taskCtx, eventsCh)
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
