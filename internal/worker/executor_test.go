package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/store"
)

func TestExecutorExecuteCapturesLabelsAndSessionArtifacts(t *testing.T) {
	t.Parallel()

	lnch := runtimepkg.NewRegistry()
	lnch.Register(&fakeRuntime{
		name: "fake-runtime",
		start: func(_ context.Context, _ *config.AgentConfig, task *runtimepkg.TaskContext) (runtimepkg.Session, error) {
			return &fakeSession{
				run: func(_ context.Context, events chan<- launcherevents.Event) (*runtimepkg.Result, error) {
					events <- launcherevents.Event{
						Kind:      launcherevents.KindLog,
						SessionID: task.Session.ID,
						TurnID:    task.Session.ID,
						Seq:       1,
					}
					return &runtimepkg.Result{
						ExitCode:    0,
						SessionPath: filepath.Join(task.WorkDir, "raw-session.jsonl"),
						Meta:        map[string]string{},
					}, nil
				},
			}, nil
		},
	}, "fake-runtime")

	repoRoot := t.TempDir()
	lnch.SetSessionManager(runtimepkg.NewSessionManager(filepath.Join(repoRoot, ".workbuddy", "sessions"), nil))

	reader := &sequenceLabelReader{snapshots: [][]string{{"status:queued"}, {"status:done"}}}
	executor := NewExecutor(lnch, reader)
	task := Task{
		TaskID:    "task-1",
		Repo:      "owner/repo",
		IssueNum:  42,
		WorkerID:  "worker-1",
		Attempt:   2,
		AgentName: "dev-agent",
		Agent: &config.AgentConfig{
			Name:    "dev-agent",
			Runtime: "fake-runtime",
		},
		Context: &runtimepkg.TaskContext{
			RepoRoot: repoRoot,
			WorkDir:  repoRoot,
		},
	}

	exec := executor.Execute(context.Background(), task)
	if exec.RunErr != nil {
		t.Fatalf("RunErr = %v, want nil", exec.RunErr)
	}
	if exec.Result == nil {
		t.Fatal("Result = nil, want non-nil")
	}
	if exec.Result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", exec.Result.ExitCode)
	}
	if got := exec.Result.RawSessionPath; got != filepath.Join(repoRoot, "raw-session.jsonl") {
		t.Fatalf("RawSessionPath = %q, want raw runtime artifact", got)
	}
	if got, want := filepath.Base(exec.Result.SessionPath), "events-v1.jsonl"; got != want {
		t.Fatalf("SessionPath base = %q, want %q", got, want)
	}
	if exec.EventErr != nil {
		t.Fatalf("EventErr = %v, want nil", exec.EventErr)
	}
	if got := exec.Task.Context.Session.ID; !strings.HasPrefix(got, "session-task-1") {
		t.Fatalf("Session.ID = %q, want prefix session-task-1", got)
	}
	if got := exec.Task.Context.Session.TaskID; got != "task-1" {
		t.Fatalf("Session.TaskID = %q, want task-1", got)
	}
	if got := exec.Task.Context.Session.WorkerID; got != "worker-1" {
		t.Fatalf("Session.WorkerID = %q, want worker-1", got)
	}
	if got := exec.Task.Context.Session.Attempt; got != 2 {
		t.Fatalf("Session.Attempt = %d, want 2", got)
	}
	if got, want := exec.PreLabels, []string{"status:queued"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("PreLabels = %v, want %v", got, want)
	}
	if got, want := exec.PostLabels, []string{"status:done"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("PostLabels = %v, want %v", got, want)
	}
	if got, want := exec.CompletionLabels, []string{"status:done"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("CompletionLabels = %v, want %v", got, want)
	}
	meta := readSessionMetadata(t, filepath.Join(repoRoot, ".workbuddy", "sessions", exec.Task.Context.Session.ID, "metadata.json"))
	if got, want := meta["status"], store.TaskStatusCompleted; got != want {
		t.Fatalf("metadata status = %v, want %q", got, want)
	}
}

func TestExecutorExecuteStartFailureMarksInfraFailure(t *testing.T) {
	t.Parallel()

	lnch := runtimepkg.NewRegistry()
	lnch.Register(&fakeRuntime{
		name: "fake-runtime",
		start: func(context.Context, *config.AgentConfig, *runtimepkg.TaskContext) (runtimepkg.Session, error) {
			return nil, errors.New("boom")
		},
	}, "fake-runtime")

	exec := NewExecutor(lnch, nil).Execute(context.Background(), Task{
		TaskID:    "task-2",
		Repo:      "owner/repo",
		IssueNum:  7,
		AgentName: "dev-agent",
		Agent: &config.AgentConfig{
			Name:    "dev-agent",
			Runtime: "fake-runtime",
		},
	})

	if exec.Result == nil || !exec.InfraFailure() {
		t.Fatalf("InfraFailure = false, want true (result=%v)", exec.Result)
	}
	if got, want := exec.FailureSource, "launcher_start_error"; got != want {
		t.Fatalf("FailureSource = %q, want %q", got, want)
	}
	if got := exec.InfraReason(); got == "" {
		t.Fatal("InfraReason = empty, want operator-facing reason")
	}
}

func TestExecutorExecuteClosesManagedSessionOnStartFailure(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	st, err := store.NewStore(filepath.Join(repoRoot, "sessions.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	lnch := runtimepkg.NewRegistry()
	lnch.SetSessionManager(runtimepkg.NewSessionManager(filepath.Join(repoRoot, ".workbuddy", "sessions"), st))
	lnch.Register(&fakeRuntime{
		name: "fake-runtime",
		start: func(context.Context, *config.AgentConfig, *runtimepkg.TaskContext) (runtimepkg.Session, error) {
			return nil, errors.New("boom")
		},
	}, "fake-runtime")

	exec := NewExecutor(lnch, nil).Execute(context.Background(), Task{
		TaskID:    "task-start-fail",
		Repo:      "owner/repo",
		IssueNum:  13,
		AgentName: "dev-agent",
		Agent: &config.AgentConfig{
			Name:    "dev-agent",
			Runtime: "fake-runtime",
		},
		Context: &runtimepkg.TaskContext{
			RepoRoot: repoRoot,
			WorkDir:  repoRoot,
		},
	})
	if exec.Result == nil {
		t.Fatal("Result = nil, want infra failure result")
	}
	meta := readSessionMetadata(t, filepath.Join(repoRoot, ".workbuddy", "sessions", exec.Task.Context.Session.ID, "metadata.json"))
	if got, want := meta["status"], store.TaskStatusFailed; got != want {
		t.Fatalf("metadata status = %v, want %q", got, want)
	}
}

func TestExecutorExecuteCreatesAndRemovesWorktree(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	worktreePath := filepath.Join(repoRoot, ".workbuddy", "worktrees", "issue-99")
	ws := &fakeWorkspaceManager{path: worktreePath}

	lnch := runtimepkg.NewRegistry()
	lnch.Register(&fakeRuntime{
		name: "fake-runtime",
		start: func(_ context.Context, _ *config.AgentConfig, task *runtimepkg.TaskContext) (runtimepkg.Session, error) {
			if task.WorkDir != worktreePath {
				t.Fatalf("WorkDir = %q, want %q", task.WorkDir, worktreePath)
			}
			if task.RepoRoot != worktreePath {
				t.Fatalf("RepoRoot = %q, want %q", task.RepoRoot, worktreePath)
			}
			return &fakeSession{
				run: func(_ context.Context, _ chan<- launcherevents.Event) (*runtimepkg.Result, error) {
					return &runtimepkg.Result{ExitCode: 0, Meta: map[string]string{}}, nil
				},
			}, nil
		},
	}, "fake-runtime")

	exec := NewExecutor(lnch, nil).Execute(context.Background(), Task{
		TaskID:           "task-worktree-ok",
		Repo:             "owner/repo",
		IssueNum:         99,
		AgentName:        "dev-agent",
		Agent:            &config.AgentConfig{Name: "dev-agent", Runtime: "fake-runtime"},
		Context:          &runtimepkg.TaskContext{RepoRoot: repoRoot, WorkDir: repoRoot},
		WorkspaceManager: ws,
	})
	if exec.RunErr != nil {
		t.Fatalf("RunErr = %v, want nil", exec.RunErr)
	}
	if !ws.created {
		t.Fatal("workspace Create was not called")
	}
	if ws.removed != worktreePath {
		t.Fatalf("removed path = %q, want %q", ws.removed, worktreePath)
	}
}

func TestExecutorExecuteWorktreeSetupFailureMarksInfraFailure(t *testing.T) {
	t.Parallel()

	lnch := runtimepkg.NewRegistry()
	ws := &fakeWorkspaceManager{err: errors.New("stale registration")}

	exec := NewExecutor(lnch, nil).Execute(context.Background(), Task{
		TaskID:           "task-worktree-fail",
		Repo:             "owner/repo",
		IssueNum:         88,
		AgentName:        "dev-agent",
		Agent:            &config.AgentConfig{Name: "dev-agent", Runtime: "fake-runtime"},
		Context:          &runtimepkg.TaskContext{},
		WorkspaceManager: ws,
	})
	if exec.Result == nil || !exec.InfraFailure() {
		t.Fatalf("InfraFailure = false, want true (result=%v)", exec.Result)
	}
	if got, want := exec.FailureSource, "worktree_setup_error"; got != want {
		t.Fatalf("FailureSource = %q, want %q", got, want)
	}
	if exec.RunErr == nil || !strings.Contains(exec.RunErr.Error(), "worktree setup failed") {
		t.Fatalf("RunErr = %v, want worktree setup failure", exec.RunErr)
	}
	if ws.created {
		t.Fatal("workspace Create should fail before runtime start")
	}
}

func TestExecutorStopCancelsInFlightRun(t *testing.T) {
	t.Parallel()

	lnch := runtimepkg.NewRegistry()
	started := make(chan struct{})
	lnch.Register(&fakeRuntime{
		name: "fake-runtime",
		start: func(_ context.Context, _ *config.AgentConfig, _ *runtimepkg.TaskContext) (runtimepkg.Session, error) {
			return &fakeSession{
				run: func(ctx context.Context, _ chan<- launcherevents.Event) (*runtimepkg.Result, error) {
					close(started)
					<-ctx.Done()
					return nil, ctx.Err()
				},
			}, nil
		},
	}, "fake-runtime")

	executor := NewExecutor(lnch, nil)
	done := make(chan Execution, 1)
	go func() {
		done <- executor.Execute(context.Background(), Task{
			TaskID:    "task-3",
			Repo:      "owner/repo",
			IssueNum:  9,
			AgentName: "dev-agent",
			Agent: &config.AgentConfig{
				Name:    "dev-agent",
				Runtime: "fake-runtime",
			},
		})
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("executor run did not start")
	}
	executor.Stop()

	select {
	case exec := <-done:
		if !errors.Is(exec.RunErr, context.Canceled) {
			t.Fatalf("RunErr = %v, want context.Canceled", exec.RunErr)
		}
		if got, want := exec.FailureSource, "session_run_nil_result"; got != want {
			t.Fatalf("FailureSource = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("executor did not stop in-flight run")
	}
}

type fakeRuntime struct {
	name  string
	start func(ctx context.Context, agent *config.AgentConfig, task *runtimepkg.TaskContext) (runtimepkg.Session, error)
}

func (f *fakeRuntime) Name() string { return f.name }

func (f *fakeRuntime) Start(ctx context.Context, agent *config.AgentConfig, task *runtimepkg.TaskContext) (runtimepkg.Session, error) {
	return f.start(ctx, agent, task)
}

func (f *fakeRuntime) Launch(ctx context.Context, agent *config.AgentConfig, task *runtimepkg.TaskContext) (*runtimepkg.Result, error) {
	session, err := f.Start(ctx, agent, task)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()
	return session.Run(ctx, nil)
}

type fakeSession struct {
	run       func(ctx context.Context, events chan<- launcherevents.Event) (*runtimepkg.Result, error)
	closeOnce sync.Once
}

func (f *fakeSession) Run(ctx context.Context, events chan<- launcherevents.Event) (*runtimepkg.Result, error) {
	return f.run(ctx, events)
}

func (f *fakeSession) SetApprover(runtimepkg.Approver) error { return nil }

func (f *fakeSession) Close() error {
	f.closeOnce.Do(func() {})
	return nil
}

type sequenceLabelReader struct {
	mu        sync.Mutex
	snapshots [][]string
	idx       int
}

func (r *sequenceLabelReader) ReadIssueLabels(string, int) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.snapshots) == 0 {
		return nil, nil
	}
	if r.idx >= len(r.snapshots) {
		return append([]string(nil), r.snapshots[len(r.snapshots)-1]...), nil
	}
	labels := append([]string(nil), r.snapshots[r.idx]...)
	r.idx++
	return labels, nil
}

type fakeWorkspaceManager struct {
	path    string
	err     error
	created bool
	removed string
}

func (f *fakeWorkspaceManager) Create(issueNum int, taskID string, rolloutIndex int) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.created = true
	return f.path, nil
}

func (f *fakeWorkspaceManager) Remove(worktreePath string) error {
	f.removed = worktreePath
	return nil
}

func readSessionMetadata(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("Unmarshal(%s): %v", path, err)
	}
	return meta
}
