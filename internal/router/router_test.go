package router

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// errBoom is a sentinel returned by failingGateStore to force the router's
// dep-gate error branch in tests.
var errBoom = errors.New("boom")

// fakeEventRecorder captures router-emitted events so silent-skip telemetry
// (REQ #345) can be asserted in isolation from the SQLite event store.
type fakeEventRecorder struct {
	mu     sync.Mutex
	events []fakeRouterEvent
}

type fakeRouterEvent struct {
	Type     string
	Repo     string
	IssueNum int
	Payload  interface{}
}

func (r *fakeEventRecorder) Log(eventType, repo string, issueNum int, payload interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fakeRouterEvent{Type: eventType, Repo: repo, IssueNum: issueNum, Payload: payload})
}

func (r *fakeEventRecorder) find(eventType string) []fakeRouterEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []fakeRouterEvent
	for _, e := range r.events {
		if e.Type == eventType {
			out = append(out, e)
		}
	}
	return out
}

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

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.NewStore(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newSchedulingRouter(t *testing.T, agents map[string]*config.AgentConfig) (*Router, *fakePreparer, store.Store) {
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

// TestRouter_AttachesStateDef verifies that when SetWorkflows wires the
// router to a workflow registry, the Decision emitted by handleDispatch
// carries the *config.State pointer (used by the preparer to attach the
// workflow-state metadata to the runtime TaskContext for transition footer
// rendering — issue #204 batch 3).
func TestRouter_AttachesStateDef(t *testing.T) {
	agents := map[string]*config.AgentConfig{
		"dev-agent": {Name: "dev-agent", Role: "dev", Runtime: "claude-code", Command: "echo"},
	}
	r, fp, _ := newSchedulingRouter(t, agents)
	state := &config.State{
		EnterLabel: "status:developing",
		Transitions: map[string]string{
			"status:reviewing": "reviewing",
			"status:blocked":   "blocked",
		},
	}
	r.SetWorkflows(map[string]*config.WorkflowConfig{
		"default": {
			Name:   "default",
			States: map[string]*config.State{"developing": state},
		},
	})

	r.handleDispatch(context.Background(), statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  10,
		AgentName: "dev-agent",
		Workflow:  "default",
		State:     "developing",
	})

	if len(fp.got) != 1 {
		t.Fatalf("preparer invocations = %d, want 1", len(fp.got))
	}
	if fp.got[0].StateDef != state {
		t.Fatalf("StateDef pointer mismatch; want the wired state, got %+v", fp.got[0].StateDef)
	}
}

// TestRouter_NilStateDefWhenWorkflowsUnwired makes sure the existing
// dispatch path is unaffected for callers that have not yet wired
// SetWorkflows — Decision.StateDef stays nil and the preparer treats that
// as "no footer".
func TestRouter_NilStateDefWhenWorkflowsUnwired(t *testing.T) {
	agents := map[string]*config.AgentConfig{
		"dev-agent": {Name: "dev-agent", Role: "dev", Runtime: "claude-code", Command: "echo"},
	}
	r, fp, _ := newSchedulingRouter(t, agents)

	r.handleDispatch(context.Background(), statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  11,
		AgentName: "dev-agent",
		Workflow:  "default",
		State:     "developing",
	})

	if len(fp.got) != 1 {
		t.Fatalf("preparer invocations = %d, want 1", len(fp.got))
	}
	if fp.got[0].StateDef != nil {
		t.Fatalf("StateDef = %+v, want nil when workflows registry is unwired", fp.got[0].StateDef)
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

func TestRouter_EmitsEventWhenNoEligibleWorkerCanClaim(t *testing.T) {
	agents := map[string]*config.AgentConfig{
		"dev-agent": {Name: "dev-agent", Role: "dev", Runtime: "codex", Command: "echo"},
	}
	r, fp, _ := newSchedulingRouter(t, agents)
	rec := &fakeEventRecorder{}
	r.SetEventRecorder(rec)

	r.handleDispatch(context.Background(), statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  345,
		AgentName: "dev-agent",
		Workflow:  "default",
		State:     "developing",
	})

	if len(fp.got) != 1 {
		t.Fatalf("preparer invocations = %d, want 1 because the task is still enqueued", len(fp.got))
	}
	got := rec.find(eventlog.TypeDispatchNoEligibleWorker)
	if len(got) != 1 {
		t.Fatalf("expected exactly one %s event, got %d (all events=%v)", eventlog.TypeDispatchNoEligibleWorker, len(got), rec.events)
	}
	payload, ok := got[0].Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map[string]any", got[0].Payload)
	}
	if payload["reason"] != "no_online_worker_for_repo_role" {
		t.Fatalf("payload.reason = %v, want no_online_worker_for_repo_role", payload["reason"])
	}
	if payload["role"] != "dev" || payload["runtime"] != "codex" {
		t.Fatalf("payload role/runtime = %v/%v, want dev/codex", payload["role"], payload["runtime"])
	}
}

func TestRouter_EmitsRuntimeMismatchWhenWorkersCannotClaim(t *testing.T) {
	agents := map[string]*config.AgentConfig{
		"dev-agent": {Name: "dev-agent", Role: "dev", Runtime: "codex", Command: "echo"},
	}
	r, fp, st := newSchedulingRouter(t, agents)
	rec := &fakeEventRecorder{}
	r.SetEventRecorder(rec)
	if err := st.InsertWorker(store.WorkerRecord{
		ID:        "worker-claude",
		Repo:      "test/repo",
		ReposJSON: `["test/repo"]`,
		Roles:     `["dev"]`,
		Runtime:   "claude-code",
		Hostname:  "host",
		Status:    "online",
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}

	r.handleDispatch(context.Background(), statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  346,
		AgentName: "dev-agent",
		Workflow:  "default",
		State:     "developing",
	})

	if len(fp.got) != 1 {
		t.Fatalf("preparer invocations = %d, want 1 because runtime mismatch is observability-only", len(fp.got))
	}
	got := rec.find(eventlog.TypeDispatchNoEligibleWorker)
	if len(got) != 1 {
		t.Fatalf("expected exactly one %s event, got %d", eventlog.TypeDispatchNoEligibleWorker, len(got))
	}
	payload, ok := got[0].Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map[string]any", got[0].Payload)
	}
	if payload["reason"] != "runtime_mismatch" {
		t.Fatalf("payload.reason = %v, want runtime_mismatch", payload["reason"])
	}
}

func TestRouter_DoesNotEmitNoEligibleWorkerWhenWorkerCanClaim(t *testing.T) {
	agents := map[string]*config.AgentConfig{
		"dev-agent": {Name: "dev-agent", Role: "dev", Runtime: "", Command: "echo"},
	}
	r, fp, st := newSchedulingRouter(t, agents)
	rec := &fakeEventRecorder{}
	r.SetEventRecorder(rec)
	if err := st.InsertWorker(store.WorkerRecord{
		ID:        "worker-codex",
		Repo:      "test/repo",
		ReposJSON: `["test/repo"]`,
		Roles:     `["dev"]`,
		Runtime:   "codex",
		Hostname:  "host",
		Status:    "online",
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}

	r.handleDispatch(context.Background(), statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  347,
		AgentName: "dev-agent",
		Workflow:  "default",
		State:     "developing",
	})

	if len(fp.got) != 1 {
		t.Fatalf("preparer invocations = %d, want 1", len(fp.got))
	}
	if got := rec.find(eventlog.TypeDispatchNoEligibleWorker); len(got) != 0 {
		t.Fatalf("unexpected %s events: %+v", eventlog.TypeDispatchNoEligibleWorker, got)
	}
}

// TestRouter_EmitsEventOnAgentNotFound closes one of the three silent-skip
// gaps surfaced in issue #345: when a workflow names an agent the catalog
// doesn't know about, the router previously logged to stderr only. It must
// now publish a structured event the operator can see via `workbuddy status
// --events`.
func TestRouter_EmitsEventOnAgentNotFound(t *testing.T) {
	r, fp, _ := newSchedulingRouter(t, map[string]*config.AgentConfig{})
	rec := &fakeEventRecorder{}
	r.SetEventRecorder(rec)

	r.handleDispatch(context.Background(), statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  101,
		AgentName: "ghost-agent",
		Workflow:  "default",
		State:     "developing",
	})

	if len(fp.got) != 0 {
		t.Fatalf("expected no preparer invocation for unknown agent, got %d", len(fp.got))
	}
	got := rec.find(eventlog.TypeDispatchSkippedAgentNotFound)
	if len(got) != 1 {
		t.Fatalf("expected exactly one %s event, got %d (all events=%v)",
			eventlog.TypeDispatchSkippedAgentNotFound, len(got), rec.events)
	}
	ev := got[0]
	if ev.Repo != "test/repo" || ev.IssueNum != 101 {
		t.Fatalf("event identity mismatch: repo=%q issue=%d", ev.Repo, ev.IssueNum)
	}
	payload, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map[string]any", ev.Payload)
	}
	if payload["agent"] != "ghost-agent" {
		t.Fatalf("payload.agent = %v, want ghost-agent", payload["agent"])
	}
	if payload["workflow"] != "default" {
		t.Fatalf("payload.workflow = %v, want default", payload["workflow"])
	}
	if payload["state"] != "developing" {
		t.Fatalf("payload.state = %v, want developing", payload["state"])
	}
}

// TestRouter_EmitsEventOnDependencyBlock closes the second silent-skip gap:
// when the dependency gate reports a blocked verdict, the router-side
// dispatch-skip must surface as a TypeDispatchBlockedByDependency event with
// source="router" and the observed verdict so it can be distinguished from
// the state-machine-side emit of the same event type (REQ-149 / #345).
func TestRouter_EmitsEventOnDependencyBlock(t *testing.T) {
	agents := map[string]*config.AgentConfig{
		"dev-agent": {Name: "dev-agent", Role: "dev", Runtime: "claude-code", Command: "echo"},
	}
	r, fp, st := newSchedulingRouter(t, agents)
	rec := &fakeEventRecorder{}
	r.SetEventRecorder(rec)

	if err := st.UpsertIssueDependencyState(store.IssueDependencyState{
		Repo:     "test/repo",
		IssueNum: 77,
		Verdict:  store.DependencyVerdictBlocked,
	}); err != nil {
		t.Fatalf("UpsertIssueDependencyState: %v", err)
	}

	r.handleDispatch(context.Background(), statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  77,
		AgentName: "dev-agent",
		Workflow:  "default",
		State:     "developing",
	})

	if len(fp.got) != 0 {
		t.Fatalf("expected no preparer invocation when dep verdict is blocked, got %d", len(fp.got))
	}
	got := rec.find(eventlog.TypeDispatchBlockedByDependency)
	if len(got) != 1 {
		t.Fatalf("expected exactly one %s event, got %d", eventlog.TypeDispatchBlockedByDependency, len(got))
	}
	payload, ok := got[0].Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map[string]any", got[0].Payload)
	}
	if payload["source"] != "router" {
		t.Fatalf("payload.source = %v, want router", payload["source"])
	}
	if payload["agent"] != "dev-agent" {
		t.Fatalf("payload.agent = %v, want dev-agent", payload["agent"])
	}
	if payload["verdict"] != store.DependencyVerdictBlocked {
		t.Fatalf("payload.verdict = %v, want %q", payload["verdict"], store.DependencyVerdictBlocked)
	}
	if _, hasErr := payload["error"]; hasErr {
		t.Fatalf("payload.error should be absent on blocked-verdict path, got %v", payload["error"])
	}
}

// failingGateStore returns an error from QueryIssueDependencyState so the
// router takes the dep-gate error branch.
type failingGateStore struct {
	err error
}

func (f *failingGateStore) QueryIssueDependencyState(string, int) (*store.IssueDependencyState, error) {
	return nil, f.err
}

// TestRouter_EmitsEventOnDependencyGateError exercises the err != nil branch
// of the router's dep-gate. The unified REQ-149 payload schema requires
// {agent, workflow, state, source: "router", error} and no verdict key on
// this path.
func TestRouter_EmitsEventOnDependencyGateError(t *testing.T) {
	agents := map[string]*config.AgentConfig{
		"dev-agent": {Name: "dev-agent", Role: "dev", Runtime: "claude-code", Command: "echo"},
	}
	r, fp, _ := newSchedulingRouter(t, agents)
	rec := &fakeEventRecorder{}
	r.SetEventRecorder(rec)
	// Inject a failing gate store so checkDependencyBlocked returns err.
	r.gateStore = &failingGateStore{err: errBoom}

	r.handleDispatch(context.Background(), statemachine.DispatchRequest{
		Repo:      "test/repo",
		IssueNum:  78,
		AgentName: "dev-agent",
		Workflow:  "default",
		State:     "developing",
	})

	if len(fp.got) != 0 {
		t.Fatalf("expected no preparer invocation on dep-gate error, got %d", len(fp.got))
	}
	got := rec.find(eventlog.TypeDispatchBlockedByDependency)
	if len(got) != 1 {
		t.Fatalf("expected exactly one %s event, got %d", eventlog.TypeDispatchBlockedByDependency, len(got))
	}
	payload, ok := got[0].Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map[string]any", got[0].Payload)
	}
	if payload["source"] != "router" {
		t.Fatalf("payload.source = %v, want router", payload["source"])
	}
	errStr, ok := payload["error"].(string)
	if !ok || errStr == "" {
		t.Fatalf("payload.error = %v, want non-empty string", payload["error"])
	}
	if v, has := payload["verdict"]; has && v != "" {
		t.Fatalf("payload.verdict must be absent or empty on error path, got %v", v)
	}
}

// TestRouter_NilEventRecorderIsSafe defends the optionality contract: a
// router whose event recorder was never wired (the default state for many
// existing tests + non-coordinator call sites) must not panic when it
// reaches any of the three silent-skip branches (REQ-149 / #345).
func TestRouter_NilEventRecorderIsSafe(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(t *testing.T, r *Router, st store.Store)
		req         statemachine.DispatchRequest
		agents      map[string]*config.AgentConfig
		failingGate bool
	}{
		{
			name:   "agent_not_found",
			agents: map[string]*config.AgentConfig{},
			req: statemachine.DispatchRequest{
				Repo: "test/repo", IssueNum: 9, AgentName: "nobody",
			},
		},
		{
			name: "dep_blocked_true",
			agents: map[string]*config.AgentConfig{
				"dev-agent": {Name: "dev-agent", Role: "dev", Runtime: "claude-code", Command: "echo"},
			},
			setup: func(t *testing.T, _ *Router, st store.Store) {
				if err := st.UpsertIssueDependencyState(store.IssueDependencyState{
					Repo: "test/repo", IssueNum: 10, Verdict: store.DependencyVerdictBlocked,
				}); err != nil {
					t.Fatalf("UpsertIssueDependencyState: %v", err)
				}
			},
			req: statemachine.DispatchRequest{
				Repo: "test/repo", IssueNum: 10, AgentName: "dev-agent",
				Workflow: "default", State: "developing",
			},
		},
		{
			name: "dep_blocked_error",
			agents: map[string]*config.AgentConfig{
				"dev-agent": {Name: "dev-agent", Role: "dev", Runtime: "claude-code", Command: "echo"},
			},
			failingGate: true,
			req: statemachine.DispatchRequest{
				Repo: "test/repo", IssueNum: 11, AgentName: "dev-agent",
				Workflow: "default", State: "developing",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, fp, st := newSchedulingRouter(t, tc.agents)
			// Explicitly leave SetEventRecorder unset — the contract under test.
			if tc.setup != nil {
				tc.setup(t, r, st)
			}
			if tc.failingGate {
				r.gateStore = &failingGateStore{err: errBoom}
			}

			r.handleDispatch(context.Background(), tc.req)

			if len(fp.got) != 0 {
				t.Fatalf("expected no preparer invocation, got %d", len(fp.got))
			}
		})
	}
}
