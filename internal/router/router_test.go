package router

import (
	"context"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
)

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
	r := NewRouter(agents, reg, st, "test/repo", taskCh)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dispatchCh := make(chan statemachine.DispatchRequest, 1)
	dispatchCh <- statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  1,
		AgentName: "dev-agent",
		Workflow:  "feature-dev",
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
	r := NewRouter(agents, reg, st, "test/repo", taskCh)

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
	r := NewRouter(agents, reg, st, "test/repo", taskCh)

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
