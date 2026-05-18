package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// periodicRecoveryFixture is the minimal setup the W2-C tests need: a real
// PollerManager backed by a real SQLite store, with one repo runtime wired to
// a real StateMachine + dispatch channel. The state machine is loaded with a
// trivial workflow that has a state with an agent so that a recovered
// EventIssueCreated produces an observable DispatchRequest.
type periodicRecoveryFixture struct {
	pm         *PollerManager
	store      store.Store
	evlog      *eventlog.EventLogger
	dispatchCh chan statemachine.DispatchRequest
	runtimeCtx context.Context
	cancelRT   context.CancelFunc
}

func newPeriodicRecoveryFixture(t *testing.T, repo string) *periodicRecoveryFixture {
	t.Helper()
	st, err := store.NewStore(filepath.Join(t.TempDir(), "periodic-recovery.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	evlog := eventlog.NewEventLogger(st)
	dispatchCh := make(chan statemachine.DispatchRequest, 16)

	// Trivial workflow: trigger "workbuddy"; one state "reviewing" with
	// EnterLabel status:reviewing and agent review-agent.
	wf := &config.WorkflowConfig{
		Name:    "dev-flow",
		Trigger: config.WorkflowTrigger{IssueLabel: "workbuddy"},
		States: map[string]*config.State{
			"reviewing": {
				EnterLabel: "status:reviewing",
				Agent:      "review-agent",
			},
		},
	}
	workflows := map[string]*config.WorkflowConfig{"dev-flow": wf}
	cfg := &config.FullConfig{
		Global:    config.GlobalConfig{Repo: repo},
		Agents:    map[string]*config.AgentConfig{},
		Workflows: workflows,
	}

	rootCtx, cancelRoot := context.WithCancel(context.Background())
	t.Cleanup(cancelRoot)
	sm := statemachine.NewStateMachine(workflows, st, dispatchCh, evlog, nil)

	pm := &PollerManager{
		rootCtx:      rootCtx,
		store:        st,
		eventlog:     evlog,
		pollInterval: time.Minute,
		runtimes:     make(map[string]*RepoRuntime),
		events:       make(chan poller.ChangeEvent, 16),
	}

	runtimeCtx, cancelRT := context.WithCancel(rootCtx)
	rt := &RepoRuntime{
		Registration: store.RepoRegistrationRecord{Repo: repo, Status: "active"},
		Config:       cfg,
		StateMachine: sm,
		DispatchCh:   dispatchCh,
		ctx:          runtimeCtx,
		cancel:       cancelRT,
		done:         make(chan struct{}),
	}
	pm.runtimes[repo] = rt

	return &periodicRecoveryFixture{
		pm:         pm,
		store:      st,
		evlog:      evlog,
		dispatchCh: dispatchCh,
		runtimeCtx: runtimeCtx,
		cancelRT:   cancelRT,
	}
}

// seedOrphanedReviewingIssue inserts an open issue_cache row labelled
// workbuddy + status:reviewing with no live task_queue row. This is the
// canonical "orphaned active state" that periodic recovery must re-dispatch.
func (f *periodicRecoveryFixture) seedOrphanedReviewingIssue(t *testing.T, repo string, issueNum int) {
	t.Helper()
	labels, err := json.Marshal([]string{"workbuddy", "status:reviewing"})
	if err != nil {
		t.Fatalf("marshal labels: %v", err)
	}
	if err := f.store.UpsertIssueCache(store.IssueCache{
		Repo:     repo,
		IssueNum: issueNum,
		Labels:   string(labels),
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
}

// TestPeriodicRecoveryReDispatchesOrphanedIssues spins up the periodic
// recovery loop at a short interval and asserts that an orphaned
// status:reviewing issue produces at least one DispatchRequest on the state
// machine's dispatch channel. This is the W2-C functional contract: after a
// zombie task is reaped, periodic recovery must re-dispatch without waiting
// for a coordinator restart. See REQ-152.
func TestPeriodicRecoveryReDispatchesOrphanedIssues(t *testing.T) {
	const (
		repo     = "owner/repo"
		issueNum = 42
	)
	f := newPeriodicRecoveryFixture(t, repo)
	f.seedOrphanedReviewingIssue(t, repo, issueNum)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		f.pm.recoverAllPeriodically(ctx, 50*time.Millisecond)
	}()

	// Wait until a dispatch is observed or the deadline elapses.
	select {
	case req := <-f.dispatchCh:
		if req.Repo != repo || req.IssueNum != issueNum {
			t.Fatalf("dispatch mismatch: got repo=%q issue=%d", req.Repo, req.IssueNum)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("periodic recovery did not re-dispatch %s#%d within deadline", repo, issueNum)
	}

	cancel()
	wg.Wait()
}

// TestPeriodicRecoveryEmitsTickEvent confirms the audit-trail contract:
// every recovery tick must record a TypePeriodicRecoveryTick event with
// repos_swept and issues_redispatched payload fields. Operators rely on this
// to confirm the sweep is alive in production (REQ-152).
func TestPeriodicRecoveryEmitsTickEvent(t *testing.T) {
	const (
		repo     = "owner/repo"
		issueNum = 7
	)
	f := newPeriodicRecoveryFixture(t, repo)
	f.seedOrphanedReviewingIssue(t, repo, issueNum)

	// Drive a single deterministic tick rather than racing the ticker.
	f.pm.runPeriodicRecoveryOnce()

	events, err := f.evlog.Query(eventlog.EventFilter{Type: eventlog.TypePeriodicRecoveryTick})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 %s event, got %d", eventlog.TypePeriodicRecoveryTick, len(events))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(events[0].Payload), &payload); err != nil {
		t.Fatalf("payload not valid JSON: %v (%q)", err, events[0].Payload)
	}
	if got, ok := payload["repos_swept"].(float64); !ok || int(got) != 1 {
		t.Fatalf("repos_swept = %v, want 1: %+v", payload["repos_swept"], payload)
	}
	if got, ok := payload["issues_redispatched"].(float64); !ok || int(got) < 1 {
		t.Fatalf("issues_redispatched = %v, want >= 1: %+v", payload["issues_redispatched"], payload)
	}
}

// TestPeriodicRecoverySkipsCancelledRuntime asserts that a runtime whose ctx
// has been cancelled (i.e. the repo was deregistered mid-tick, or shutdown
// is in flight) is silently skipped and does NOT count toward repos_swept.
// No panic, no dispatch. This protects against racing the cleanup goroutine
// during graceful shutdown.
func TestPeriodicRecoverySkipsCancelledRuntime(t *testing.T) {
	const (
		repo     = "owner/repo"
		issueNum = 99
	)
	f := newPeriodicRecoveryFixture(t, repo)
	f.seedOrphanedReviewingIssue(t, repo, issueNum)

	// Cancel the runtime BEFORE the tick fires. The sweep must skip it.
	f.cancelRT()

	f.pm.runPeriodicRecoveryOnce()

	select {
	case req := <-f.dispatchCh:
		t.Fatalf("unexpected dispatch for cancelled runtime: %+v", req)
	default:
	}

	events, err := f.evlog.Query(eventlog.EventFilter{Type: eventlog.TypePeriodicRecoveryTick})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 tick event, got %d", len(events))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(events[0].Payload), &payload); err != nil {
		t.Fatalf("payload JSON: %v", err)
	}
	if got, ok := payload["repos_swept"].(float64); !ok || int(got) != 0 {
		t.Fatalf("repos_swept = %v, want 0 (runtime cancelled): %+v", payload["repos_swept"], payload)
	}
	if got, ok := payload["issues_redispatched"].(float64); !ok || int(got) != 0 {
		t.Fatalf("issues_redispatched = %v, want 0: %+v", payload["issues_redispatched"], payload)
	}
}
