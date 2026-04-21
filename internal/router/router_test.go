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

// fakePreparer captures the Decision the Router emits so scheduling
// behaviour can be asserted in isolation from persistence / GH / dispatch.
type fakePreparer struct {
	got  []Decision
	err  error
	seen int
}

func (f *fakePreparer) Prepare(_ context.Context, d Decision) error {
	f.got = append(f.got, d)
	f.seen++
	return f.err
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

func newSchedulingRouter(t *testing.T, agents map[string]*config.AgentConfig) (*Router, *fakePreparer, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	reg := registry.NewRegistry(st, 30*time.Second)
	r := NewRouter(agents, reg, st, "test/repo", t.TempDir(), nil, nil, false)
	fp := &fakePreparer{}
	r.SetPreparer(fp)
	return r, fp, st
}

func TestRouter_EmitsDecisionForKnownAgent(t *testing.T) {
	agents := map[string]*config.AgentConfig{
		"dev-agent": {
			Name:    "dev-agent",
			Role:    "dev",
			Runtime: "claude-code",
			Command: "echo hello",
			Timeout: 5 * time.Minute,
		},
	}
	r, fp, _ := newSchedulingRouter(t, agents)

	r.handleDispatch(context.Background(), statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  1,
		AgentName: "dev-agent",
		Workflow:  "default",
		State:     "developing",
	})

	if len(fp.got) != 1 {
		t.Fatalf("preparer invocations = %d, want 1", len(fp.got))
	}
	d := fp.got[0]
	if d.AgentName != "dev-agent" || d.IssueNum != 1 || d.Workflow != "default" || d.State != "developing" {
		t.Fatalf("unexpected decision: %+v", d)
	}
	if d.Agent == nil || d.Agent.Role != "dev" {
		t.Fatalf("decision Agent not populated: %+v", d.Agent)
	}
}

func TestRouter_SkipsUnknownAgent(t *testing.T) {
	r, fp, _ := newSchedulingRouter(t, map[string]*config.AgentConfig{})

	r.handleDispatch(context.Background(), statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  1,
		AgentName: "nonexistent",
	})

	if len(fp.got) != 0 {
		t.Fatalf("expected no preparer invocation for unknown agent, got %d", len(fp.got))
	}
}

func TestRouter_BlocksDispatchOnDependencyVerdict(t *testing.T) {
	agents := map[string]*config.AgentConfig{
		"dev-agent": {Name: "dev-agent", Role: "dev", Runtime: "claude-code", Command: "echo hello"},
	}
	r, fp, st := newSchedulingRouter(t, agents)

	// Seed a blocked verdict for the issue.
	if err := st.UpsertIssueDependencyState(store.IssueDependencyState{
		Repo:     "test/repo",
		IssueNum: 7,
		Verdict:  store.DependencyVerdictBlocked,
	}); err != nil {
		t.Fatalf("UpsertIssueDependencyState: %v", err)
	}

	r.handleDispatch(context.Background(), statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  7,
		AgentName: "dev-agent",
	})

	if len(fp.got) != 0 {
		t.Fatalf("expected no preparer invocation when dep verdict is blocked, got %d", len(fp.got))
	}
}

func TestRouter_AllowsDispatchWhenReady(t *testing.T) {
	agents := map[string]*config.AgentConfig{
		"dev-agent": {Name: "dev-agent", Role: "dev", Runtime: "claude-code", Command: "echo hello"},
	}
	r, fp, st := newSchedulingRouter(t, agents)

	if err := st.UpsertIssueDependencyState(store.IssueDependencyState{
		Repo:     "test/repo",
		IssueNum: 8,
		Verdict:  store.DependencyVerdictReady,
	}); err != nil {
		t.Fatalf("UpsertIssueDependencyState: %v", err)
	}

	r.handleDispatch(context.Background(), statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  8,
		AgentName: "dev-agent",
		Workflow:  "default",
		State:     "developing",
	})

	if len(fp.got) != 1 {
		t.Fatalf("preparer invocations = %d, want 1", len(fp.got))
	}
}

func TestRouter_RunConsumesDispatchChannel(t *testing.T) {
	agents := map[string]*config.AgentConfig{
		"dev-agent": {Name: "dev-agent", Role: "dev", Runtime: "claude-code", Command: "echo"},
	}
	r, fp, _ := newSchedulingRouter(t, agents)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dispatchCh := make(chan statemachine.DispatchRequest, 1)
	dispatchCh <- statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  42,
		AgentName: "dev-agent",
		Workflow:  "default",
		State:     "developing",
	}

	done := make(chan struct{})
	go func() {
		_ = r.Run(ctx, dispatchCh)
		close(done)
	}()

	// Wait for the preparer to observe the decision, then cancel.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fp.seen > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if fp.seen != 1 {
		t.Fatalf("preparer invocations = %d, want 1", fp.seen)
	}
}
