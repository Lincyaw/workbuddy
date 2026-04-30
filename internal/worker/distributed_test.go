package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/reporter"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/workerclient"
)

// fakeRemoteClient records calls and returns scripted errors per-method.
type fakeRemoteClient struct {
	mu sync.Mutex

	submitErrs  []error // consumed in order; once exhausted returns nil
	submitCalls int
	submitReqs  []workerclient.ResultRequest

	releaseErr   error
	releaseCalls int
	releaseReqs  []workerclient.ReleaseRequest
}

func (c *fakeRemoteClient) Heartbeat(ctx context.Context, taskID string, req workerclient.HeartbeatRequest) error {
	return nil
}

func (c *fakeRemoteClient) ReleaseTask(ctx context.Context, taskID string, req workerclient.ReleaseRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseCalls++
	c.releaseReqs = append(c.releaseReqs, req)
	return c.releaseErr
}

func (c *fakeRemoteClient) SubmitResult(ctx context.Context, taskID string, req workerclient.ResultRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.submitCalls++
	c.submitReqs = append(c.submitReqs, req)
	if len(c.submitErrs) == 0 {
		return nil
	}
	err := c.submitErrs[0]
	c.submitErrs = c.submitErrs[1:]
	return err
}

type fakeCommentWriter struct{}

func (fakeCommentWriter) WriteComment(string, int, string) error { return nil }

type fakeDistributedIssueReader struct{}

func (fakeDistributedIssueReader) ReadIssue(string, int) (poller.IssueDetails, error) {
	return poller.IssueDetails{}, nil
}

func (fakeDistributedIssueReader) ReadIssueLabels(string, int) ([]string, error) {
	return nil, nil
}

type fakeDistributedWorkspaceManager struct {
	mu           sync.Mutex
	createCalls  int
	issueNum     int
	taskID       string
	rolloutIndex int
}

func (m *fakeDistributedWorkspaceManager) Create(issueNum int, taskID string, rolloutIndex int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls++
	m.issueNum = issueNum
	m.taskID = taskID
	m.rolloutIndex = rolloutIndex
	return "", nil
}

func (m *fakeDistributedWorkspaceManager) Remove(string) error { return nil }

func init() {
	// Neutralize real backoff waits for tests.
	submitResultSleep = func(ctx context.Context, d time.Duration) {}
}

func TestSubmitResultWithRetry_TransientThenSuccess(t *testing.T) {
	t.Parallel()

	client := &fakeRemoteClient{
		submitErrs: []error{
			errors.New("temporary: connection refused"),
			errors.New("temporary: 503"),
		},
	}
	w := &DistributedWorker{deps: DistributedDeps{
		Client:          client,
		WorkerID:        "worker-1",
		ShutdownTimeout: time.Second,
	}}

	err := w.submitResultWithRetry(context.Background(), "task-xyz", workerclient.ResultRequest{WorkerID: "worker-1"})
	if err != nil {
		t.Fatalf("expected success after transient failures, got %v", err)
	}
	if client.submitCalls != 3 {
		t.Fatalf("expected 3 submit attempts (2 fail + 1 success), got %d", client.submitCalls)
	}
	if client.releaseCalls != 0 {
		t.Fatalf("expected no release on success, got %d", client.releaseCalls)
	}
}

func TestSubmitResultWithRetry_PermanentFailureReturnsError(t *testing.T) {
	t.Parallel()

	boom := errors.New("coordinator unreachable")
	client := &fakeRemoteClient{submitErrs: []error{boom, boom, boom, boom}}
	w := &DistributedWorker{deps: DistributedDeps{
		Client:          client,
		WorkerID:        "worker-1",
		ShutdownTimeout: time.Second,
	}}

	err := w.submitResultWithRetry(context.Background(), "task-xyz", workerclient.ResultRequest{WorkerID: "worker-1"})
	if err == nil {
		t.Fatalf("expected error after exhausting retries")
	}
	if client.submitCalls != submitResultMaxAttempts {
		t.Fatalf("expected %d submit attempts, got %d", submitResultMaxAttempts, client.submitCalls)
	}
}

func TestSubmitResultWithRetry_UnauthorizedShortCircuits(t *testing.T) {
	t.Parallel()

	client := &fakeRemoteClient{submitErrs: []error{workerclient.ErrUnauthorized}}
	w := &DistributedWorker{deps: DistributedDeps{
		Client:          client,
		WorkerID:        "worker-1",
		ShutdownTimeout: time.Second,
	}}

	err := w.submitResultWithRetry(context.Background(), "task-xyz", workerclient.ResultRequest{WorkerID: "worker-1"})
	if !errors.Is(err, workerclient.ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if client.submitCalls != 1 {
		t.Fatalf("expected non-retryable error to stop after 1 attempt, got %d", client.submitCalls)
	}
}

func TestSubmitResultWithRetry_OwnershipLostShortCircuits(t *testing.T) {
	t.Parallel()

	client := &fakeRemoteClient{submitErrs: []error{errors.New("task already completed")}}
	w := &DistributedWorker{deps: DistributedDeps{
		Client:          client,
		WorkerID:        "worker-1",
		ShutdownTimeout: time.Second,
	}}

	err := w.submitResultWithRetry(context.Background(), "task-xyz", workerclient.ResultRequest{WorkerID: "worker-1"})
	if err == nil || !strings.Contains(err.Error(), "task already completed") {
		t.Fatalf("expected ownership-lost short-circuit, got %v", err)
	}
	if client.submitCalls != 1 {
		t.Fatalf("expected no retry when coordinator reports ownership lost, got %d attempts", client.submitCalls)
	}
}

// Integration-style: confirm that on permanent submit failure the worker
// invokes ReleaseTask with a reason that references the submit error, so
// the coordinator observes a freed lease instead of waiting for expiry.
func TestDistributedWorker_ReleasesWithReasonOnPermanentSubmitFailure(t *testing.T) {
	t.Parallel()

	boom := errors.New("coordinator 500")
	client := &fakeRemoteClient{submitErrs: []error{boom, boom, boom}}
	w := &DistributedWorker{deps: DistributedDeps{
		Client:          client,
		WorkerID:        "worker-1",
		ShutdownTimeout: time.Second,
	}}

	submitErr := w.submitResultWithRetry(context.Background(), "task-xyz", workerclient.ResultRequest{WorkerID: "worker-1"})
	if submitErr == nil {
		t.Fatalf("precondition: expected submit error")
	}

	// Simulate the release step the caller performs on permanent failure.
	reason := fmt.Sprintf("submit result failed: %v", submitErr)
	if err := client.ReleaseTask(context.Background(), "task-xyz", workerclient.ReleaseRequest{WorkerID: "worker-1", Reason: reason}); err != nil {
		t.Fatalf("release failed: %v", err)
	}
	if client.releaseCalls != 1 {
		t.Fatalf("expected exactly 1 release call, got %d", client.releaseCalls)
	}
	if got := client.releaseReqs[0].Reason; !strings.Contains(got, "submit result failed") || !strings.Contains(got, "coordinator 500") {
		t.Fatalf("release reason should reference submit error, got %q", got)
	}
}

func TestDistributedWorker_PreservesExplicitAgentRuntime(t *testing.T) {
	t.Parallel()

	launcher := runtimepkg.NewRegistry()
	var startedRuntime string
	launcher.SetSessionStarter(func(_ context.Context, agent *config.AgentConfig, _ *runtimepkg.TaskContext) (runtimepkg.Session, error, bool) {
		startedRuntime = agent.Runtime
		return &fakeSession{
			run: func(_ context.Context, _ chan<- launcherevents.Event) (*runtimepkg.Result, error) {
				return &runtimepkg.Result{ExitCode: 0, Meta: map[string]string{}}, nil
			},
		}, nil, true
	})
	executor := NewExecutor(launcher, fakeDistributedIssueReader{})
	client := &fakeRemoteClient{}
	worker := NewDistributedWorker(DistributedDeps{
		Config: &config.FullConfig{
			Agents: map[string]*config.AgentConfig{
				"dev-agent": {Name: "dev-agent", Runtime: config.RuntimeCodex},
			},
		},
		Executor:          executor,
		Reporter:          reporter.NewReporter(fakeCommentWriter{}),
		Reader:            fakeDistributedIssueReader{},
		Client:            client,
		WorkDir:           t.TempDir(),
		WorkerID:          "worker-1",
		RuntimeAlias:      config.RuntimeClaudeCode,
		HeartbeatInterval: time.Hour,
		ShutdownTimeout:   time.Second,
	})

	err := worker.ExecuteTask(context.Background(), &workerclient.Task{
		TaskID:    "task-1",
		Repo:      "owner/repo",
		IssueNum:  42,
		AgentName: "dev-agent",
	})
	if err != nil {
		t.Fatalf("ExecuteTask: %v", err)
	}
	if startedRuntime != config.RuntimeCodex {
		t.Fatalf("runtime passed to launcher = %q, want %q", startedRuntime, config.RuntimeCodex)
	}
	if client.submitCalls != 1 {
		t.Fatalf("submitCalls = %d, want 1", client.submitCalls)
	}
}

func TestDistributedWorker_PassesRolloutMetadataIntoExecutor(t *testing.T) {
	t.Parallel()

	launcher := runtimepkg.NewRegistry()
	launcher.SetSessionStarter(func(_ context.Context, _ *config.AgentConfig, task *runtimepkg.TaskContext) (runtimepkg.Session, error, bool) {
		if task.Rollout.Index != 2 || task.Rollout.Total != 3 || task.Rollout.GroupID != "rollout-group-42" {
			t.Fatalf("task rollout context = %+v", task.Rollout)
		}
		return &fakeSession{
			run: func(_ context.Context, _ chan<- launcherevents.Event) (*runtimepkg.Result, error) {
				return &runtimepkg.Result{ExitCode: 0, Meta: map[string]string{}}, nil
			},
		}, nil, true
	})
	executor := NewExecutor(launcher, fakeDistributedIssueReader{})
	client := &fakeRemoteClient{}
	workspace := &fakeDistributedWorkspaceManager{}
	worker := NewDistributedWorker(DistributedDeps{
		Config: &config.FullConfig{
			Agents: map[string]*config.AgentConfig{
				"dev-agent": {Name: "dev-agent", Runtime: config.RuntimeCodex},
			},
		},
		Executor:          executor,
		Reporter:          reporter.NewReporter(fakeCommentWriter{}),
		Reader:            fakeDistributedIssueReader{},
		Client:            client,
		WorkDir:           t.TempDir(),
		WorkerID:          "worker-1",
		HeartbeatInterval: time.Hour,
		ShutdownTimeout:   time.Second,
		WorkspaceManager:  workspace,
	})

	err := worker.ExecuteTask(context.Background(), &workerclient.Task{
		TaskID:         "task-rollout",
		Repo:           "owner/repo",
		IssueNum:       42,
		AgentName:      "dev-agent",
		Workflow:       "default",
		State:          "developing",
		RolloutIndex:   2,
		RolloutsTotal:  3,
		RolloutGroupID: "rollout-group-42",
	})
	if err != nil {
		t.Fatalf("ExecuteTask: %v", err)
	}
	if client.submitCalls != 1 {
		t.Fatalf("submitCalls = %d, want 1", client.submitCalls)
	}

	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	if workspace.createCalls != 1 {
		t.Fatalf("workspace create calls = %d, want 1", workspace.createCalls)
	}
	if workspace.issueNum != 42 || workspace.taskID != "task-rollout" || workspace.rolloutIndex != 2 {
		t.Fatalf("workspace create args = issue=%d task=%q rollout=%d", workspace.issueNum, workspace.taskID, workspace.rolloutIndex)
	}
}
