package router

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/reporter"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/workspace"
)

type mockCommentWriter struct {
	comments []string
}

func (m *mockCommentWriter) WriteComment(_ string, _ int, body string) error {
	m.comments = append(m.comments, body)
	return nil
}

func setupFakeGHCLI(t *testing.T) {
	t.Helper()
	fakeBin := t.TempDir()
	ghPath := filepath.Join(fakeBin, "gh")
	script := `#!/bin/sh
if [ "$1" = "issue" ] && [ "$2" = "view" ]; then
  echo '{"title":"Issue","body":"body","labels":[{"name":"workbuddy"}],"comments":[]}'
  exit 0
fi
if [ "$1" = "pr" ] && [ "$2" = "list" ]; then
  echo '[]'
  exit 0
fi
echo '{}'
`
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.NewStore(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestRouter_MatchingWorker(t *testing.T) {
	st := newTestStore(t)
	reg := registry.NewRegistry(st, 30*time.Second)

	// Register a worker
	if err := reg.Register("worker-1", "test/repo", []string{"dev"}, "localhost"); err != nil {
		t.Fatal(err)
	}

	agents := map[string]*config.AgentConfig{
		"dev-agent": {
			Name:    "dev-agent",
			Role:    "dev",
			Runtime: "claude-code",
			Command: "echo hello",
			Timeout: 5 * time.Minute,
		},
	}

	taskCh := make(chan WorkerTask, 10)
	r := NewRouter(agents, reg, st, "test/repo", t.TempDir(), taskCh, nil, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dispatchCh := make(chan statemachine.DispatchRequest, 1)
	dispatchCh <- statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  1,
		AgentName: "dev-agent",
		Workflow:  "default",
		State:     "developing",
	}

	go func() {
		time.Sleep(5 * time.Second)
		cancel()
	}()

	_ = r.Run(ctx, dispatchCh)

	select {
	case task := <-taskCh:
		if task.AgentName != "dev-agent" {
			t.Errorf("expected agent dev-agent, got %s", task.AgentName)
		}
		if task.IssueNum != 1 {
			t.Errorf("expected issue 1, got %d", task.IssueNum)
		}
	default:
		t.Error("expected task on channel, got none")
	}
}

func TestRouter_AgentNotFound(t *testing.T) {
	st := newTestStore(t)
	reg := registry.NewRegistry(st, 30*time.Second)

	agents := map[string]*config.AgentConfig{} // empty

	taskCh := make(chan WorkerTask, 10)
	r := NewRouter(agents, reg, st, "test/repo", t.TempDir(), taskCh, nil, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dispatchCh := make(chan statemachine.DispatchRequest, 1)
	dispatchCh <- statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  1,
		AgentName: "nonexistent",
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_ = r.Run(ctx, dispatchCh)

	select {
	case <-taskCh:
		t.Error("should not have dispatched a task for unknown agent")
	default:
		// expected
	}
}

func TestRouter_NoMatchingWorker(t *testing.T) {
	st := newTestStore(t)
	reg := registry.NewRegistry(st, 30*time.Second)
	// No workers registered

	agents := map[string]*config.AgentConfig{
		"dev-agent": {
			Name:    "dev-agent",
			Role:    "dev",
			Runtime: "claude-code",
			Command: "echo hello",
		},
	}

	taskCh := make(chan WorkerTask, 10)
	r := NewRouter(agents, reg, st, "test/repo", t.TempDir(), taskCh, nil, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dispatchCh := make(chan statemachine.DispatchRequest, 1)
	dispatchCh <- statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  1,
		AgentName: "dev-agent",
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_ = r.Run(ctx, dispatchCh)

	// Task should still be created in store as pending, but not dispatched to channel
	// (since no worker available and the router just inserts + tries to dispatch)
}

func TestRouter_PersistOnlyModeLeavesTaskPending(t *testing.T) {
	st := newTestStore(t)
	reg := registry.NewRegistry(st, 30*time.Second)

	agents := map[string]*config.AgentConfig{
		"dev-agent": {
			Name:    "dev-agent",
			Role:    "dev",
			Runtime: "codex",
			Command: "echo hello",
		},
	}

	r := NewRouter(agents, reg, st, "test/repo", t.TempDir(), nil, nil, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dispatchCh := make(chan statemachine.DispatchRequest, 1)
	dispatchCh <- statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  2,
		AgentName: "dev-agent",
		Workflow:  "default",
		State:     "developing",
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_ = r.Run(ctx, dispatchCh)

	tasks, err := st.QueryTasks(store.TaskStatusPending)
	if err != nil {
		t.Fatalf("QueryTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("pending tasks = %d, want 1", len(tasks))
	}
	if tasks[0].Role != "dev" || tasks[0].Runtime != "codex" || tasks[0].Workflow != "default" || tasks[0].State != "developing" {
		t.Fatalf("unexpected persisted task: %+v", tasks[0])
	}
}

func TestRouter_PersistOnlyModeDoesNotCreateWorktree(t *testing.T) {
	st := newTestStore(t)
	reg := registry.NewRegistry(st, 30*time.Second)
	repoRoot := initGitRepo(t)
	wsMgr := workspace.NewManager(repoRoot)

	agents := map[string]*config.AgentConfig{
		"dev-agent": {
			Name:    "dev-agent",
			Role:    "dev",
			Runtime: "codex",
			Command: "echo hello",
		},
	}

	r := NewRouter(agents, reg, st, "test/repo", repoRoot, nil, wsMgr, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dispatchCh := make(chan statemachine.DispatchRequest, 1)
	dispatchCh <- statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  41,
		AgentName: "dev-agent",
		Workflow:  "default",
		State:     "developing",
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_ = r.Run(ctx, dispatchCh)

	if _, err := os.Stat(filepath.Join(repoRoot, ".workbuddy", "worktrees", "issue-41")); !os.IsNotExist(err) {
		t.Fatalf("coordinator mode should not create a worktree, stat err = %v", err)
	}
}

func TestRouter_DoesNotEagerlyCreateWorktreeAtDispatch(t *testing.T) {
	setupFakeGHCLI(t)

	st := newTestStore(t)
	reg := registry.NewRegistry(st, 30*time.Second)
	taskCh := make(chan WorkerTask, 1)
	comments := &mockCommentWriter{}
	repoRoot := initGitRepo(t)
	wtPath := filepath.Join(repoRoot, ".workbuddy", "worktrees", "issue-5")
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(wtPath, []byte("not a worktree"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	r := NewRouter(
		map[string]*config.AgentConfig{
			"dev-agent": {
				Name:    "dev-agent",
				Role:    "dev",
				Runtime: "claude-code",
				Command: "echo hello",
			},
		},
		reg,
		st,
		"test/repo",
		repoRoot,
		taskCh,
		workspace.NewManager(repoRoot),
		true,
	)
	r.SetReporter(reporter.NewReporter(comments))

	r.handleDispatch(context.Background(), statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  5,
		AgentName: "dev-agent",
		Workflow:  "default",
		State:     "developing",
	})

	select {
	case task := <-taskCh:
		if task.Context == nil {
			t.Fatal("expected dispatched task context")
		}
		if task.Context.RepoRoot != repoRoot {
			t.Fatalf("RepoRoot = %q, want %q", task.Context.RepoRoot, repoRoot)
		}
		if task.Context.WorkDir != repoRoot {
			t.Fatalf("WorkDir = %q, want %q", task.Context.WorkDir, repoRoot)
		}
	default:
		t.Fatal("expected dispatched task")
	}

	tasks, err := st.QueryTasks(store.TaskStatusFailed)
	if err != nil {
		t.Fatalf("QueryTasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("failed tasks = %d, want 0", len(tasks))
	}

	if len(comments.comments) != 0 {
		t.Fatalf("comment count = %d, want 0", len(comments.comments))
	}
}

func TestRouter_DispatchUsesBaseRepoRootAndWorkDir(t *testing.T) {
	setupFakeGHCLI(t)

	st := newTestStore(t)
	reg := registry.NewRegistry(st, 30*time.Second)
	taskCh := make(chan WorkerTask, 1)
	repoRoot := initGitRepo(t)

	r := NewRouter(
		map[string]*config.AgentConfig{
			"dev-agent": {
				Name:    "dev-agent",
				Role:    "dev",
				Runtime: "codex",
				Command: "echo hello",
			},
		},
		reg,
		st,
		"test/repo",
		repoRoot,
		taskCh,
		workspace.NewManager(repoRoot),
		true,
	)

	r.handleDispatch(context.Background(), statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  6,
		AgentName: "dev-agent",
		Workflow:  "default",
		State:     "developing",
	})

	select {
	case task := <-taskCh:
		if task.WorktreePath != "" {
			t.Fatalf("WorktreePath = %q, want empty transport payload", task.WorktreePath)
		}
		if task.Context == nil {
			t.Fatal("expected dispatched task context")
		}
		if task.Context.RepoRoot != repoRoot {
			t.Fatalf("RepoRoot = %q, want %q", task.Context.RepoRoot, repoRoot)
		}
		if task.Context.WorkDir != repoRoot {
			t.Fatalf("WorkDir = %q, want %q", task.Context.WorkDir, repoRoot)
		}
	default:
		t.Fatal("expected dispatched task")
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}

	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("test repo"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %s: %v", out, err)
	}
	cmd = exec.Command("git", "commit", "-m", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %s: %v", out, err)
	}

	return dir
}
