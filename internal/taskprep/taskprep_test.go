package taskprep

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/config"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// fakeReader satisfies IssueDataReader without shelling out.
type fakeReader struct{}

func (fakeReader) ReadIssueSummary(_ string, _ int) (string, string, []string, error) {
	return "Issue", "body", []string{"workbuddy"}, nil
}
func (fakeReader) ReadIssueComments(_ string, _ int) ([]runtimepkg.IssueComment, error) {
	return nil, nil
}
func (fakeReader) ListRelatedPRs(_ string, _ int) ([]runtimepkg.PRSummary, error) {
	return nil, nil
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

func newDecision(agentCfg *config.AgentConfig, issue int) Decision {
	return Decision{
		Repo:      "test/repo",
		IssueNum:  issue,
		AgentName: agentCfg.Name,
		Agent:     agentCfg,
		Workflow:  "default",
		State:     "developing",
	}
}

func TestPreparer_PersistOnlyModeLeavesTaskPending(t *testing.T) {
	st := newTestStore(t)
	p := NewPreparer(st, t.TempDir(), nil, false)
	p.SetIssueDataReader(fakeReader{})

	agent := &config.AgentConfig{Name: "dev-agent", Role: "dev", Runtime: "codex", Command: "echo"}
	if err := p.Prepare(context.Background(), newDecision(agent, 2)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	tasks, err := st.QueryTasks(store.TaskStatusPending)
	if err != nil {
		t.Fatalf("QueryTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("pending tasks = %d, want 1", len(tasks))
	}
	got := tasks[0]
	if got.Role != "dev" || got.Runtime != "codex" || got.Workflow != "default" || got.State != "developing" {
		t.Fatalf("unexpected persisted task: %+v", got)
	}
}

func TestPreparer_PersistOnlyModeDoesNotCreateWorktree(t *testing.T) {
	st := newTestStore(t)
	repoRoot := initGitRepo(t)
	p := NewPreparer(st, repoRoot, nil, false)
	p.SetIssueDataReader(fakeReader{})

	agent := &config.AgentConfig{Name: "dev-agent", Role: "dev", Runtime: "codex", Command: "echo"}
	if err := p.Prepare(context.Background(), newDecision(agent, 41)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if _, err := os.Stat(filepath.Join(repoRoot, ".workbuddy", "worktrees", "issue-41")); !os.IsNotExist(err) {
		t.Fatalf("coordinator mode should not create a worktree, stat err = %v", err)
	}
}

func TestPreparer_DispatchDoesNotEagerlyCreateWorktree(t *testing.T) {
	st := newTestStore(t)
	repoRoot := initGitRepo(t)
	taskCh := make(chan WorkerTask, 1)
	p := NewPreparer(st, repoRoot, taskCh, true)
	p.SetIssueDataReader(fakeReader{})

	agent := &config.AgentConfig{Name: "dev-agent", Role: "dev", Runtime: "claude-code", Command: "echo"}
	if err := p.Prepare(context.Background(), newDecision(agent, 5)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

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
		if task.WorktreePath != "" {
			t.Fatalf("WorktreePath = %q, want empty transport payload", task.WorktreePath)
		}
	default:
		t.Fatal("expected dispatched task")
	}

	failed, err := st.QueryTasks(store.TaskStatusFailed)
	if err != nil {
		t.Fatalf("QueryTasks: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("failed tasks = %d, want 0", len(failed))
	}
}

func TestPreparer_DispatchUsesBaseRepoRootAndWorkDir(t *testing.T) {
	st := newTestStore(t)
	repoRoot := initGitRepo(t)
	taskCh := make(chan WorkerTask, 1)
	p := NewPreparer(st, repoRoot, taskCh, true)
	p.SetIssueDataReader(fakeReader{})

	agent := &config.AgentConfig{Name: "dev-agent", Role: "dev", Runtime: "codex", Command: "echo"}
	if err := p.Prepare(context.Background(), newDecision(agent, 6)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

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
}

// TestPreparer_AttachesWorkflowStateMetadata verifies that the Decision's
// StateDef (the *config.State pointer the router looks up) is propagated
// onto the dispatched WorkerTask's TaskContext via SetWorkflowState. This
// is the wiring that drives the runtime-injected transition footer (issue
// #204 batch 3).
func TestPreparer_AttachesWorkflowStateMetadata(t *testing.T) {
	st := newTestStore(t)
	repoRoot := initGitRepo(t)
	taskCh := make(chan WorkerTask, 1)
	p := NewPreparer(st, repoRoot, taskCh, true)
	p.SetIssueDataReader(fakeReader{})

	agent := &config.AgentConfig{Name: "dev-agent", Role: "dev", Runtime: "claude-code", Command: "echo"}
	state := &config.State{
		EnterLabel: "status:developing",
		Transitions: map[string]string{
			"status:reviewing": "reviewing",
			"status:blocked":   "blocked",
		},
	}
	d := newDecision(agent, 99)
	d.StateDef = state
	if err := p.Prepare(context.Background(), d); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	select {
	case task := <-taskCh:
		if task.Context == nil {
			t.Fatal("expected dispatched task context")
		}
		if got := task.Context.WorkflowStateName(); got != "developing" {
			t.Fatalf("WorkflowStateName = %q, want %q", got, "developing")
		}
		enterLabel, transitions := task.Context.WorkflowStateMetadata()
		if enterLabel != "status:developing" {
			t.Fatalf("enterLabel = %q, want %q", enterLabel, "status:developing")
		}
		if len(transitions) != 2 {
			t.Fatalf("transitions = %v, want 2 entries", transitions)
		}
		if transitions["status:reviewing"] != "reviewing" || transitions["status:blocked"] != "blocked" {
			t.Fatalf("transitions = %v, want reviewing/blocked targets", transitions)
		}
	default:
		t.Fatal("expected dispatched task")
	}
}

func TestPreparer_NilAgentReturnsError(t *testing.T) {
	st := newTestStore(t)
	p := NewPreparer(st, t.TempDir(), nil, false)
	if err := p.Prepare(context.Background(), Decision{Repo: "test/repo", IssueNum: 1, AgentName: "missing"}); err == nil {
		t.Fatal("expected error for nil agent")
	}
}
