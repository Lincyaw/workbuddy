package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	"github.com/Lincyaw/workbuddy/internal/poller"
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
	os.MkdirAll(filepath.Join(dir, "agents"), 0o755)
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

	os.MkdirAll(filepath.Join(dir, "workflows"), 0o755)
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
	mu      sync.Mutex
	issues  []poller.Issue
	prs     []poller.PR
	calls   int
	onPoll  func(call int) // callback on each ListIssues call
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
		Status:    "running",
	}); err != nil {
		t.Fatal(err)
	}

	// Insert a "pending" task
	if err := st.InsertTask(store.TaskRecord{
		ID:        "task-pending-1",
		Repo:      "owner/repo",
		IssueNum:  2,
		AgentName: "dev-agent",
		Status:    "pending",
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Reopen store and run recovery
	st2, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	if err := recoverTasks(st2); err != nil {
		t.Fatalf("recoverTasks: %v", err)
	}

	// Verify running task is now failed
	tasks, err := st2.QueryTasks("failed")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != "task-running-1" {
		t.Errorf("expected 1 failed task (task-running-1), got %d tasks", len(tasks))
	}

	// Verify pending task is still pending
	pending, err := st2.QueryTasks("pending")
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
	defer resp.Body.Close()

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
