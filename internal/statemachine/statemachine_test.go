package statemachine

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/workflow"
)

// fakeRecorder collects events for assertions.
type fakeRecorder struct {
	mu     sync.Mutex
	events []fakeEvent
}

type fakeEvent struct {
	Type     string
	Repo     string
	IssueNum int
	Payload  interface{}
}

func (r *fakeRecorder) Log(eventType, repo string, issueNum int, payload interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fakeEvent{
		Type:     eventType,
		Repo:     repo,
		IssueNum: issueNum,
		Payload:  payload,
	})
}

func (r *fakeRecorder) find(eventType string) []fakeEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []fakeEvent
	for _, e := range r.events {
		if e.Type == eventType {
			out = append(out, e)
		}
	}
	return out
}

// testWorkflow returns a simple dev workflow for testing.
func testWorkflow() *config.WorkflowConfig {
	return &config.WorkflowConfig{
		Name:       "dev-flow",
		MaxRetries: 2,
		Trigger: config.WorkflowTrigger{
			IssueLabel: "workbuddy",
		},
		States: map[string]*config.State{
			"developing": {
				EnterLabel: "status:developing",
				Agent:      "dev-agent",
				Transitions: map[string]string{
					"status:reviewing": "reviewing",
				},
			},
			"reviewing": {
				EnterLabel: "status:reviewing",
				Agent:      "review-agent",
				Transitions: map[string]string{
					"status:developing": "developing",
					"status:done":       "done",
				},
			},
			"done": {
				EnterLabel: "status:done",
			},
			"failed": {
				EnterLabel: "status:failed",
			},
		},
	}
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTestSM(t *testing.T) (*StateMachine, *fakeRecorder, chan DispatchRequest) {
	t.Helper()
	st := newTestStore(t)
	rec := &fakeRecorder{}
	dispatch := make(chan DispatchRequest, 10)
	wf := testWorkflow()
	sm := NewStateMachine(
		map[string]*config.WorkflowConfig{"dev-flow": wf},
		st,
		dispatch,
		rec,
		nil,
	)
	return sm, rec, dispatch
}

func newParallelWorkflow(join string) *config.WorkflowConfig {
	return &config.WorkflowConfig{
		Name:       "parallel-flow",
		MaxRetries: 2,
		Trigger: config.WorkflowTrigger{
			IssueLabel: "workbuddy",
		},
		States: map[string]*config.State{
			"developing": {
				EnterLabel: "status:developing",
				Agents:     []string{"dev-agent", "review-agent"},
				Join:       config.JoinConfig{Strategy: join},
				Transitions: map[string]string{
					"status:done": "done",
				},
			},
			"done": {
				EnterLabel: "status:done",
			},
		},
	}
}

func TestMarkAgentCompletedUsesCanonicalTaskAgentName(t *testing.T) {
	sm, rec, _ := newTestSM(t)
	repo := "test/repo"
	issueNum := 99

	if err := sm.store.InsertTask(store.TaskRecord{
		ID:        "task-review-1",
		Repo:      repo,
		IssueNum:  issueNum,
		AgentName: "review-agent",
		Status:    store.TaskStatusRunning,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	sm.MarkAgentCompleted(repo, issueNum, "task-review-1", "dev-agent", 1, []string{"workbuddy", "status:reviewing"})

	completed := rec.find(eventlog.TypeCompleted)
	if len(completed) != 1 {
		t.Fatalf("completed events = %d, want 1", len(completed))
	}
	payload, ok := completed[0].Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T", completed[0].Payload)
	}
	if got := payload["agent_name"]; got != "review-agent" {
		t.Fatalf("agent_name = %v, want review-agent", got)
	}
}

// Test 1: Normal transition (developing → reviewing)
func TestNormalTransition(t *testing.T) {
	sm, rec, dispatch := newTestSM(t)

	event := ChangeEvent{
		Type:     poller.EventLabelAdded,
		Repo:     "test/repo",
		IssueNum: 1,
		Labels:   []string{"workbuddy", "status:developing"},
		Detail:   "status:reviewing",
	}

	if err := sm.HandleEvent(context.Background(), event); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	// Should have logged a transition.
	transitions := rec.find(eventlog.TypeTransition)
	if len(transitions) != 1 {
		t.Fatalf("expected 1 transition event, got %d", len(transitions))
	}

	// Should have dispatched to the review agent.
	select {
	case req := <-dispatch:
		if req.AgentName != "review-agent" {
			t.Errorf("expected agent review-agent, got %s", req.AgentName)
		}
		if req.State != "reviewing" {
			t.Errorf("expected state reviewing, got %s", req.State)
		}
	default:
		t.Error("expected dispatch request, got none")
	}

	instances, err := workflow.NewManager(sm.store).QueryByRepoIssue("test/repo", 1)
	if err != nil {
		t.Fatalf("workflow query: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("expected 1 workflow instance, got %d", len(instances))
	}
	if instances[0].CurrentState != "reviewing" {
		t.Fatalf("expected current state reviewing, got %q", instances[0].CurrentState)
	}
}

// Test 2: Back-edge count increment (reviewing → developing)
func TestBackEdgeCount(t *testing.T) {
	sm, _, dispatch := newTestSM(t)

	// First: developing → reviewing (creates history for reviewing).
	ev1 := ChangeEvent{
		Type:     poller.EventLabelAdded,
		Repo:     "test/repo",
		IssueNum: 2,
		Labels:   []string{"workbuddy", "status:developing"},
		Detail:   "status:reviewing",
	}
	if err := sm.HandleEvent(context.Background(), ev1); err != nil {
		t.Fatalf("HandleEvent 1: %v", err)
	}
	<-dispatch // drain

	// Mark agent complete so inflight is cleared.
	sm.MarkAgentCompleted("test/repo", 2, "task-review-1", "review-agent", 0, []string{"workbuddy", "status:reviewing"})
	sm.ResetDedup() // new poll cycle

	// Now: reviewing → developing (this is a back-edge since developing was visited).
	ev2 := ChangeEvent{
		Type:     poller.EventLabelAdded,
		Repo:     "test/repo",
		IssueNum: 2,
		Labels:   []string{"workbuddy", "status:reviewing"},
		Detail:   "status:developing",
	}
	if err := sm.HandleEvent(context.Background(), ev2); err != nil {
		t.Fatalf("HandleEvent 2: %v", err)
	}

	// Should still dispatch since count (1) < max_retries (2).
	select {
	case req := <-dispatch:
		if req.AgentName != "dev-agent" {
			t.Errorf("expected dev-agent, got %s", req.AgentName)
		}
	default:
		t.Error("expected dispatch on back-edge within limit")
	}

	// Verify the transition count is recorded.
	counts, err := sm.store.QueryTransitionCounts("test/repo", 2)
	if err != nil {
		t.Fatalf("QueryTransitionCounts: %v", err)
	}
	found := false
	for _, tc := range counts {
		if tc.FromState == "reviewing" && tc.ToState == "developing" {
			found = true
			if tc.Count != 1 {
				t.Errorf("expected count 1, got %d", tc.Count)
			}
		}
	}
	if !found {
		t.Error("back-edge transition count not recorded")
	}
}

// Test 3: Retry limit → failed
func TestRetryLimitFailed(t *testing.T) {
	sm, rec, dispatch := newTestSM(t)

	repo := "test/repo"
	issueNum := 3

	// max_retries=2. Back-edge detection works by checking if target state
	// already appears in transition_counts (as a "to" target).
	//
	// Step 1: developing→reviewing. First time visiting reviewing. Not a back-edge.
	//   transition_counts: developing→reviewing=1
	// Step 2: reviewing→developing. First time "developing" appears as target
	//   (it was source before but not target). Not a back-edge.
	//   transition_counts: reviewing→developing=1
	// Step 3: developing→reviewing. "reviewing" already appeared as target (step 1).
	//   This IS a back-edge. developing→reviewing count becomes 2. 2 >= max_retries=2 → FAILED.

	// Step 1: developing → reviewing (not a back-edge)
	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type: poller.EventLabelAdded, Repo: repo, IssueNum: issueNum,
		Labels: []string{"workbuddy", "status:developing"}, Detail: "status:reviewing",
	}); err != nil {
		t.Fatalf("HandleEvent step 1: %v", err)
	}
	<-dispatch
	sm.MarkAgentCompleted(repo, issueNum, "task-review-1", "review-agent", 0, []string{"workbuddy", "status:reviewing"})
	sm.ResetDedup()

	// Step 2: reviewing → developing (back-edge: "developing" was a source, but
	// not yet a target. Actually, let's check: is there any prior transition TO developing?
	// No — step 1 was TO reviewing. So this is NOT a back-edge.)
	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type: poller.EventLabelAdded, Repo: repo, IssueNum: issueNum,
		Labels: []string{"workbuddy", "status:reviewing"}, Detail: "status:developing",
	}); err != nil {
		t.Fatalf("HandleEvent step 2: %v", err)
	}
	<-dispatch
	sm.MarkAgentCompleted(repo, issueNum, "task-dev-1", "dev-agent", 0, []string{"workbuddy", "status:developing"})
	sm.ResetDedup()

	// Step 3: developing → reviewing. "reviewing" already appeared as target in step 1.
	// This IS a back-edge. Count for developing→reviewing: was 1, increment to 2.
	// 2 >= max_retries (2) → cycle_limit_reached → transition to failed.
	err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type: poller.EventLabelAdded, Repo: repo, IssueNum: issueNum,
		Labels: []string{"workbuddy", "status:developing"}, Detail: "status:reviewing",
	})
	if err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	// Should NOT have dispatched (rejected due to cycle limit).
	select {
	case <-dispatch:
		t.Error("should not dispatch when retry limit exceeded")
	default:
		// good
	}

	// Should have logged cycle_limit_reached and transition_to_failed.
	if len(rec.find(eventlog.TypeCycleLimitReached)) == 0 {
		t.Error("expected cycle_limit_reached event")
	}
	if len(rec.find(eventlog.TypeTransitionToFailed)) == 0 {
		t.Error("expected transition_to_failed event")
	}
}

// Test 4: No matching workflow → skip silently
func TestNoMatchSkip(t *testing.T) {
	sm, rec, _ := newTestSM(t)

	err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type:     poller.EventLabelAdded,
		Repo:     "test/repo",
		IssueNum: 99,
		Labels:   []string{"bug", "priority:high"}, // no "workbuddy" trigger label
		Detail:   "priority:high",
	})
	if err != nil {
		t.Fatalf("HandleEvent should not error on no match: %v", err)
	}

	if len(rec.events) != 0 {
		t.Errorf("expected no events logged, got %d", len(rec.events))
	}
}

// Test 5: Multiple workflow match → error event
func TestMultiWorkflowReject(t *testing.T) {
	st := newTestStore(t)
	rec := &fakeRecorder{}
	dispatch := make(chan DispatchRequest, 10)

	wf1 := testWorkflow()
	wf2 := &config.WorkflowConfig{
		Name:       "alt-flow",
		MaxRetries: 3,
		Trigger:    config.WorkflowTrigger{IssueLabel: "workbuddy"},
		States: map[string]*config.State{
			"init": {EnterLabel: "status:init"},
		},
	}

	sm := NewStateMachine(
		map[string]*config.WorkflowConfig{"dev-flow": wf1, "alt-flow": wf2},
		st,
		dispatch,
		rec,
		nil,
	)

	err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type:     poller.EventLabelAdded,
		Repo:     "test/repo",
		IssueNum: 10,
		Labels:   []string{"workbuddy", "status:developing"},
		Detail:   "status:reviewing",
	})
	if err == nil {
		t.Error("expected error for multi-workflow match")
	}

	if len(rec.find(eventlog.TypeErrorMultiWorkflow)) == 0 {
		t.Error("expected error_multi_workflow event")
	}
}

// Test 6: Idempotency — same event processed twice, only first takes effect
func TestIdempotent(t *testing.T) {
	sm, rec, dispatch := newTestSM(t)

	event := ChangeEvent{
		Type:     poller.EventLabelAdded,
		Repo:     "test/repo",
		IssueNum: 5,
		Labels:   []string{"workbuddy", "status:developing"},
		Detail:   "status:reviewing",
	}

	if err := sm.HandleEvent(context.Background(), event); err != nil {
		t.Fatalf("HandleEvent 1: %v", err)
	}
	<-dispatch // drain first dispatch

	// Same event again.
	if err := sm.HandleEvent(context.Background(), event); err != nil {
		t.Fatalf("HandleEvent 2: %v", err)
	}

	// Should have only 1 transition event.
	transitions := rec.find(eventlog.TypeTransition)
	if len(transitions) != 1 {
		t.Errorf("expected 1 transition (idempotent), got %d", len(transitions))
	}

	// Should have only 1 dispatch.
	select {
	case <-dispatch:
		t.Error("second event should not produce a dispatch")
	default:
		// good
	}
}

// Test 7: Execution mutex — don't dispatch while agent is running
func TestExecutionMutex(t *testing.T) {
	sm, rec, dispatch := newTestSM(t)

	// First event triggers dispatch (developing → reviewing, dispatches review-agent).
	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type: poller.EventLabelAdded, Repo: "r", IssueNum: 7,
		Labels: []string{"workbuddy", "status:developing"}, Detail: "status:reviewing",
	}); err != nil {
		t.Fatalf("HandleEvent 1: %v", err)
	}
	<-dispatch // drain; agent is now inflight

	// Don't mark complete — agent still running.
	sm.ResetDedup() // simulate new poll cycle

	// Now try reviewing → developing (which would dispatch dev-agent).
	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type: poller.EventLabelAdded, Repo: "r", IssueNum: 7,
		Labels: []string{"workbuddy", "status:reviewing"}, Detail: "status:developing",
	}); err != nil {
		t.Fatalf("HandleEvent 2: %v", err)
	}

	// Should not get a second dispatch.
	select {
	case <-dispatch:
		t.Error("should not dispatch while agent inflight")
	default:
		// good
	}

	// Should have logged the skip.
	if len(rec.find(eventlog.TypeDispatchSkippedInflight)) == 0 {
		t.Error("expected dispatch_skipped_inflight event")
	}
}

func TestParallelStateDispatchesAllAgents(t *testing.T) {
	st := newTestStore(t)
	rec := &fakeRecorder{}
	dispatch := make(chan DispatchRequest, 10)
	sm := NewStateMachine(
		map[string]*config.WorkflowConfig{"parallel-flow": newParallelWorkflow(config.JoinAllPassed)},
		st,
		dispatch,
		rec,
		nil,
	)

	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type:     poller.EventIssueCreated,
		Repo:     "test/repo",
		IssueNum: 9,
		Labels:   []string{"workbuddy", "status:developing"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	agents := make(map[string]struct{}, 2)
	for i := 0; i < 2; i++ {
		select {
		case req := <-dispatch:
			agents[req.AgentName] = struct{}{}
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("expected 2 dispatches, got %d", len(agents))
		}
	}

	if _, ok := agents["dev-agent"]; !ok {
		t.Errorf("expected dev-agent dispatch")
	}
	if _, ok := agents["review-agent"]; !ok {
		t.Errorf("expected review-agent dispatch")
	}
}

func TestParallelStateAllPassed_SucceedsWhenAllSuccess(t *testing.T) {
	st := newTestStore(t)
	rec := &fakeRecorder{}
	dispatch := make(chan DispatchRequest, 10)
	sm := NewStateMachine(
		map[string]*config.WorkflowConfig{"parallel-flow": newParallelWorkflow(config.JoinAllPassed)},
		st,
		dispatch,
		rec,
		nil,
	)

	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type:     poller.EventIssueCreated,
		Repo:     "test/repo",
		IssueNum: 10,
		Labels:   []string{"workbuddy", "status:developing"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	<-dispatch
	<-dispatch

	labels := []string{"workbuddy", "status:done"}
	sm.MarkAgentCompleted("test/repo", 10, "task-dev", "dev-agent", 0, labels)
	sm.MarkAgentCompleted("test/repo", 10, "task-review", "review-agent", 0, labels)

	transitions := rec.find(eventlog.TypeTransition)
	if len(transitions) != 1 {
		t.Fatalf("expected 1 transition after both agents succeed, got %d", len(transitions))
	}
}

func TestParallelStateAllPassed_FailsOnPartialFailure(t *testing.T) {
	st := newTestStore(t)
	rec := &fakeRecorder{}
	dispatch := make(chan DispatchRequest, 10)
	sm := NewStateMachine(
		map[string]*config.WorkflowConfig{"parallel-flow": newParallelWorkflow(config.JoinAllPassed)},
		st,
		dispatch,
		rec,
		nil,
	)

	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type:     poller.EventIssueCreated,
		Repo:     "test/repo",
		IssueNum: 11,
		Labels:   []string{"workbuddy", "status:developing"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	<-dispatch
	<-dispatch

	labels := []string{"workbuddy", "status:developing"}
	sm.MarkAgentCompleted("test/repo", 11, "task-dev", "dev-agent", 0, labels)
	sm.MarkAgentCompleted("test/repo", 11, "task-review", "review-agent", 1, labels)

	if got := len(rec.find(eventlog.TypeTransitionToFailed)); got != 1 {
		t.Fatalf("expected 1 transition_to_failed event, got %d", got)
	}
	if got := len(rec.find(eventlog.TypeTransition)); got != 0 {
		t.Fatalf("expected no transition event after failed completion, got %d", got)
	}
}

func TestParallelStateAnyPassed_ProgressesOnFirstSuccess(t *testing.T) {
	st := newTestStore(t)
	rec := &fakeRecorder{}
	dispatch := make(chan DispatchRequest, 10)
	sm := NewStateMachine(
		map[string]*config.WorkflowConfig{"parallel-flow": newParallelWorkflow(config.JoinAnyPassed)},
		st,
		dispatch,
		rec,
		nil,
	)

	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type:     poller.EventIssueCreated,
		Repo:     "test/repo",
		IssueNum: 12,
		Labels:   []string{"workbuddy", "status:developing"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	<-dispatch
	<-dispatch

	labels := []string{"workbuddy", "status:done"}
	sm.MarkAgentCompleted("test/repo", 12, "task-dev", "dev-agent", 0, labels)
	sm.MarkAgentCompleted("test/repo", 12, "task-review", "review-agent", 1, labels)

	transitions := rec.find(eventlog.TypeTransition)
	if len(transitions) != 1 {
		t.Fatalf("expected 1 transition after first success, got %d", len(transitions))
	}
}

// Test 8: Stuck detection
func TestStuckDetection(t *testing.T) {
	sm, rec, dispatch := newTestSM(t)
	sm.SetStuckTimeout(1 * time.Millisecond)

	// Trigger a transition.
	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type: poller.EventLabelAdded, Repo: "test/repo", IssueNum: 8,
		Labels: []string{"workbuddy", "status:developing"}, Detail: "status:reviewing",
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	<-dispatch

	// Mark agent completed.
	labels := []string{"workbuddy", "status:reviewing"}
	sm.MarkAgentCompleted("test/repo", 8, "task-review-1", "review-agent", 0, labels)

	// Wait for stuck timeout.
	time.Sleep(5 * time.Millisecond)

	// Check stuck with same labels (unchanged).
	sm.CheckStuck("test/repo", 8, labels)

	if len(rec.find(eventlog.TypeStuckDetected)) == 0 {
		t.Error("expected stuck_detected event")
	}
}

// =============================================
// Transition map tests (REQ-076 — issue #204 batch 2)
// =============================================

// TestTransitionMapLookup confirms the simplified map-based transition picks
// the correct target state for an arrived label and returns false otherwise.
func TestTransitionMapLookup(t *testing.T) {
	wf := testWorkflow()
	dev := wf.States["developing"]

	if got := dev.Transitions["status:reviewing"]; got != "reviewing" {
		t.Errorf("transition for status:reviewing = %q, want reviewing", got)
	}
	if _, ok := dev.Transitions["status:bogus"]; ok {
		t.Errorf("unexpected transition for status:bogus")
	}
}

// Test 9: Context cancellation prevents blocking on dispatch channel
func TestDispatchRespectsContext(t *testing.T) {
	st := newTestStore(t)
	rec := &fakeRecorder{}
	// Unbuffered channel — will block if nobody reads.
	dispatch := make(chan DispatchRequest)
	wf := testWorkflow()
	sm := NewStateMachine(
		map[string]*config.WorkflowConfig{"dev-flow": wf},
		st,
		dispatch,
		rec,
		nil,
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := sm.HandleEvent(ctx, ChangeEvent{
		Type:     poller.EventLabelAdded,
		Repo:     "test/repo",
		IssueNum: 50,
		Labels:   []string{"workbuddy", "status:developing"},
		Detail:   "status:reviewing",
	})

	// The dispatch should fail due to cancelled context.
	if err == nil {
		t.Error("expected error from cancelled context, got nil")
	}

	// The inflight flag should have been cleaned up.
	if sm.IsInflight("test/repo", 50) {
		t.Error("inflight flag should be cleared after context cancellation")
	}
}

func TestDispatchBlockedByDependencyVerdict(t *testing.T) {
	sm, _, dispatch := newTestSM(t)
	if err := sm.store.UpsertIssueDependencyState(store.IssueDependencyState{
		Repo:     "test/repo",
		IssueNum: 77,
		Verdict:  store.DependencyVerdictBlocked,
	}); err != nil {
		t.Fatalf("UpsertIssueDependencyState: %v", err)
	}

	err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type:     poller.EventLabelAdded,
		Repo:     "test/repo",
		IssueNum: 77,
		Labels:   []string{"workbuddy", "status:developing"},
		Detail:   "status:developing",
	})
	if err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	select {
	case req := <-dispatch:
		t.Fatalf("unexpected dispatch: %+v", req)
	default:
	}
}

// captureLog redirects the standard logger output to a buffer for the duration
// of the test. Returns the buffer and a cleanup function.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prev)
		log.SetFlags(prevFlags)
	})
	return buf
}

// TestStateEntryLogWordingAndDispatchedLog covers issue #241:
//   - state entry log uses neutral "candidate agents=" wording (intent), not
//     "dispatching agents" (outcome).
//   - on the gate-passed path, a "[statemachine] dispatched ..." line is
//     emitted, proving the agent actually entered the dispatch channel.
func TestStateEntryLogWordingAndDispatchedLog(t *testing.T) {
	sm, _, dispatch := newTestSM(t)
	buf := captureLog(t)

	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type:     poller.EventLabelAdded,
		Repo:     "test/repo",
		IssueNum: 241,
		Labels:   []string{"workbuddy", "status:developing"},
		Detail:   "status:developing",
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	// Drain dispatch so the goroutine doesn't block.
	select {
	case <-dispatch:
	case <-time.After(time.Second):
		t.Fatalf("expected dispatch on gate-passed path")
	}

	out := buf.String()
	if !strings.Contains(out, "candidate agents=") {
		t.Errorf("expected neutral 'candidate agents=' wording in log, got:\n%s", out)
	}
	if strings.Contains(out, "dispatching agents") {
		t.Errorf("old 'dispatching agents' wording must be removed; got:\n%s", out)
	}
	if !strings.Contains(out, "dispatched dev-agent for test/repo#241 state=developing") {
		t.Errorf("expected '[statemachine] dispatched ...' line on gate-passed path, got:\n%s", out)
	}
}

// TestDispatchBlockedByDependencyLogsReason covers issue #241:
// when the dependency gate blocks dispatch, a human-readable log line should
// be emitted in addition to the existing dispatch_blocked_by_dependency event,
// and the "dispatched ..." line must NOT appear (ascend_dispatched=false).
func TestDispatchBlockedByDependencyLogsReason(t *testing.T) {
	sm, rec, dispatch := newTestSM(t)
	if err := sm.store.UpsertIssueDependencyState(store.IssueDependencyState{
		Repo:     "test/repo",
		IssueNum: 241,
		Verdict:  store.DependencyVerdictBlocked,
	}); err != nil {
		t.Fatalf("UpsertIssueDependencyState: %v", err)
	}
	buf := captureLog(t)

	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type:     poller.EventLabelAdded,
		Repo:     "test/repo",
		IssueNum: 241,
		Labels:   []string{"workbuddy", "status:developing"},
		Detail:   "status:developing",
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	select {
	case req := <-dispatch:
		t.Fatalf("unexpected dispatch on gate-blocked path: %+v", req)
	default:
	}

	// The pre-existing event must still be recorded (backward compat).
	if got := rec.find(eventlog.TypeDispatchBlockedByDependency); len(got) != 1 {
		t.Fatalf("expected 1 dispatch_blocked_by_dependency event, got %d", len(got))
	}

	out := buf.String()
	if !strings.Contains(out, "dispatch blocked by dependency") {
		t.Errorf("expected '[statemachine] dispatch blocked by dependency ...' log, got:\n%s", out)
	}
	if !strings.Contains(out, "verdict=blocked") {
		t.Errorf("expected verdict=blocked in log, got:\n%s", out)
	}
	if strings.Contains(out, "[statemachine] dispatched ") {
		t.Errorf("'dispatched ...' line must not appear on gate-blocked path; got:\n%s", out)
	}
}

func TestDispatchAgentBlockedByDone(t *testing.T) {
	sm, rec, dispatch := newTestSM(t)
	if err := sm.store.UpsertIssueCache(store.IssueCache{
		Repo:     "test/repo",
		IssueNum: 101,
		Labels:   `["workbuddy","status:done"]`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}

	if err := sm.DispatchAgent(context.Background(), "test/repo", 101, "review-agent", "dev-flow", "reviewing"); err != nil {
		t.Fatalf("DispatchAgent: %v", err)
	}

	select {
	case req := <-dispatch:
		t.Fatalf("dispatch should have been blocked for done issue, got: %+v", req)
	default:
	}

	if got := rec.find(eventlog.TypeDispatchBlockedByDone); len(got) != 1 {
		t.Fatalf("expected 1 dispatch_blocked_by_done event, got %d", len(got))
	}
}

func TestDispatchAgentBlockedByDoneCorruptedCacheFallback(t *testing.T) {
	sm, rec, dispatch := newTestSM(t)
	// Cached labels field is not valid JSON but still contains the quoted
	// done-label substring. The fallback must block dispatch instead of
	// silently letting the loop reignite.
	if err := sm.store.UpsertIssueCache(store.IssueCache{
		Repo:     "test/repo",
		IssueNum: 404,
		Labels:   `workbuddy,"status:done" <-- malformed JSON`,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}

	if err := sm.DispatchAgent(context.Background(), "test/repo", 404, "review-agent", "dev-flow", "reviewing"); err != nil {
		t.Fatalf("DispatchAgent: %v", err)
	}

	select {
	case req := <-dispatch:
		t.Fatalf("dispatch should have been blocked by fallback substring scan, got: %+v", req)
	default:
	}

	if got := rec.find(eventlog.TypeDispatchBlockedByDone); len(got) != 1 {
		t.Fatalf("expected 1 dispatch_blocked_by_done event via fallback, got %d", len(got))
	}
	if got := rec.find(eventlog.TypeError); len(got) != 1 {
		t.Fatalf("expected 1 error event for unmarshal, got %d", len(got))
	}
}

func TestDispatchAgentBlockedByFailureCap(t *testing.T) {
	sm, rec, dispatch := newTestSM(t)

	// Seed exactly MaxConsecutiveAgentFailures failures for review-agent on the issue.
	for i := 0; i < MaxConsecutiveAgentFailures; i++ {
		if err := sm.store.InsertTask(store.TaskRecord{
			ID:        fmt.Sprintf("task-%d", i),
			Repo:      "test/repo",
			IssueNum:  202,
			AgentName: "review-agent",
			Status:    store.TaskStatusFailed,
		}); err != nil {
			t.Fatalf("InsertTask: %v", err)
		}
	}

	if err := sm.DispatchAgent(context.Background(), "test/repo", 202, "review-agent", "dev-flow", "reviewing"); err != nil {
		t.Fatalf("DispatchAgent: %v", err)
	}

	select {
	case req := <-dispatch:
		t.Fatalf("dispatch should have been blocked by failure cap, got: %+v", req)
	default:
	}

	events := rec.find(eventlog.TypeDispatchBlockedByFailureCap)
	if len(events) != 1 {
		t.Fatalf("expected 1 dispatch_blocked_by_failure_cap event, got %d", len(events))
	}
	payload, ok := events[0].Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map[string]any", events[0].Payload)
	}
	if payload["agent"] != "review-agent" {
		t.Fatalf("payload agent = %v, want review-agent", payload["agent"])
	}
	if payload["consecutive_fails"] != MaxConsecutiveAgentFailures {
		t.Fatalf("payload consecutive_fails = %v, want %d", payload["consecutive_fails"], MaxConsecutiveAgentFailures)
	}
}

func TestDispatchAgentResetsFailureCountAfterSuccess(t *testing.T) {
	sm, _, dispatch := newTestSM(t)

	// Past failures, followed by a success, followed by one more failure.
	// The cap should not fire because the success reset the count.
	statuses := []string{
		store.TaskStatusFailed,
		store.TaskStatusFailed,
		store.TaskStatusFailed,
		store.TaskStatusCompleted,
		store.TaskStatusFailed,
	}
	for i, st := range statuses {
		if err := sm.store.InsertTask(store.TaskRecord{
			ID:        fmt.Sprintf("task-%d", i),
			Repo:      "test/repo",
			IssueNum:  303,
			AgentName: "review-agent",
			Status:    st,
		}); err != nil {
			t.Fatalf("InsertTask: %v", err)
		}
	}

	if err := sm.DispatchAgent(context.Background(), "test/repo", 303, "review-agent", "dev-flow", "reviewing"); err != nil {
		t.Fatalf("DispatchAgent: %v", err)
	}

	select {
	case req := <-dispatch:
		if req.AgentName != "review-agent" || req.IssueNum != 303 {
			t.Fatalf("unexpected dispatch: %+v", req)
		}
	default:
		t.Fatal("expected dispatch to be sent when failure count reset after success")
	}
}

// Test 10: ResetDedup is safe for concurrent access
func TestResetDedupConcurrent(t *testing.T) {
	sm, _, _ := newTestSM(t)

	// Populate some entries.
	for i := 0; i < 100; i++ {
		sm.processedEvents.Store(fmt.Sprintf("key-%d", i), struct{}{})
	}

	var wg sync.WaitGroup
	// Concurrently call ResetDedup and LoadOrStore.
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			sm.ResetDedup()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			sm.processedEvents.LoadOrStore(fmt.Sprintf("concurrent-%d", i), struct{}{})
		}
	}()
	wg.Wait()
}

// TestDispatchAgentRespectsIssueClaim covers the integration of
// SetIssueClaim + DispatchAgent: a concurrent claimer must see the dispatch
// blocked (with a typed event) while the holder's own dispatch proceeds. This
// guards REQ-057 AC-5 (coordinator-side acquisition) end-to-end.
func TestDispatchAgentRespectsIssueClaim(t *testing.T) {
	// State machine #1 represents the coordinator that holds the claim.
	sm1, _, dispatch1 := newTestSM(t)
	sm1.SetIssueClaim("coordinator-a", 5*time.Minute)

	// State machine #2 shares the same Store — simulating a second
	// coordinator process attached to the same DB — and must be blocked.
	rec2 := &fakeRecorder{}
	dispatch2 := make(chan DispatchRequest, 4)
	wf := testWorkflow()
	sm2 := NewStateMachine(
		map[string]*config.WorkflowConfig{"dev-flow": wf},
		sm1.store,
		dispatch2,
		rec2,
		nil,
	)
	sm2.SetIssueClaim("coordinator-b", 5*time.Minute)

	repo := "test/repo"
	issueNum := 77

	if err := sm1.DispatchAgent(context.Background(), repo, issueNum, "dev-agent", "dev-flow", "developing"); err != nil {
		t.Fatalf("coord-a DispatchAgent: %v", err)
	}
	select {
	case <-dispatch1:
		// coord-a's dispatch went through.
	case <-time.After(time.Second):
		t.Fatal("expected dispatch from coord-a")
	}

	// coord-b must be blocked by the persisted claim.
	if err := sm2.DispatchAgent(context.Background(), repo, issueNum, "dev-agent", "dev-flow", "developing"); err != nil {
		t.Fatalf("coord-b DispatchAgent: %v", err)
	}
	select {
	case req := <-dispatch2:
		t.Fatalf("coord-b dispatch should have been blocked, got %+v", req)
	default:
	}
	skipped := rec2.find(eventlog.TypeDispatchSkippedClaim)
	if len(skipped) != 1 {
		t.Fatalf("expected 1 dispatch_skipped_claim event on coord-b, got %d", len(skipped))
	}

	// When coord-a completes the group, it releases the claim so coord-b's
	// next attempt succeeds.
	sm1.MarkAgentCompleted(repo, issueNum, "task-1", "dev-agent", 0, []string{"workbuddy", "status:reviewing"})

	if err := sm2.DispatchAgent(context.Background(), repo, issueNum, "dev-agent", "dev-flow", "developing"); err != nil {
		t.Fatalf("coord-b DispatchAgent after release: %v", err)
	}
	select {
	case <-dispatch2:
		// coord-b now has the claim.
	case <-time.After(time.Second):
		t.Fatal("expected dispatch from coord-b after release")
	}
}

func TestMarkAgentCompletedKeepsReplacedIssueClaim(t *testing.T) {
	sm, _, dispatch := newTestSM(t)
	sm.SetIssueClaim("coordinator-a", time.Minute)

	repo := "test/repo"
	issueNum := 78

	if err := sm.DispatchAgent(context.Background(), repo, issueNum, "dev-agent", "dev-flow", "developing"); err != nil {
		t.Fatalf("DispatchAgent: %v", err)
	}
	select {
	case <-dispatch:
	case <-time.After(time.Second):
		t.Fatal("expected initial dispatch")
	}

	claim, err := sm.store.QueryIssueClaim(repo, issueNum)
	if err != nil {
		t.Fatalf("QueryIssueClaim before replace: %v", err)
	}
	if claim == nil {
		t.Fatal("expected persisted issue claim")
	}

	replacementToken := "replacement-token"
	_, err = sm.store.DB().Exec(
		`UPDATE issue_claim
		 SET claim_token = ?, acquired_at = ?, expires_at = ?
		 WHERE repo = ? AND issue_num = ? AND worker_id = ?`,
		replacementToken,
		"2026-04-18 12:00:00",
		"2026-04-18 13:00:00",
		repo,
		issueNum,
		claim.WorkerID,
	)
	if err != nil {
		t.Fatalf("replace issue claim token: %v", err)
	}

	sm.MarkAgentCompleted(repo, issueNum, "task-1", "dev-agent", 0, []string{"workbuddy", "status:reviewing"})

	after, err := sm.store.QueryIssueClaim(repo, issueNum)
	if err != nil {
		t.Fatalf("QueryIssueClaim after completion: %v", err)
	}
	if after == nil {
		t.Fatal("expected replacement claim to remain after stale completion")
	}
	if after.ClaimToken != replacementToken {
		t.Fatalf("expected replacement token %q to remain, got %+v", replacementToken, after)
	}

	sm.claimTokensMu.Lock()
	_, stillTracked := sm.claimTokens[sm.issueKey(repo, issueNum)]
	sm.claimTokensMu.Unlock()
	if stillTracked {
		t.Fatal("expected stale in-memory claim token to be removed after completion")
	}
}

// TestDispatchStateAgentsRespectsIssueClaim covers the Copilot-flagged gap:
// workflow-driven multi-agent dispatch goes through dispatchStateAgents, not
// DispatchAgent. Without the claim check on that path, two coordinators
// sharing the same DB could both dispatch the same state group. This test
// exercises the path by triggering a label event that maps to a parallel
// state; the second coordinator must be blocked.
func TestDispatchStateAgentsRespectsIssueClaim(t *testing.T) {
	st := newTestStore(t)

	rec1 := &fakeRecorder{}
	dispatch1 := make(chan DispatchRequest, 4)
	sm1 := NewStateMachine(
		map[string]*config.WorkflowConfig{"parallel-flow": newParallelWorkflow(config.JoinAllPassed)},
		st,
		dispatch1,
		rec1,
		nil,
	)
	sm1.SetIssueClaim("coordinator-a", 5*time.Minute)

	rec2 := &fakeRecorder{}
	dispatch2 := make(chan DispatchRequest, 4)
	sm2 := NewStateMachine(
		map[string]*config.WorkflowConfig{"parallel-flow": newParallelWorkflow(config.JoinAllPassed)},
		st,
		dispatch2,
		rec2,
		nil,
	)
	sm2.SetIssueClaim("coordinator-b", 5*time.Minute)

	event := ChangeEvent{
		Type:     poller.EventLabelAdded,
		Repo:     "test/repo",
		IssueNum: 88,
		Labels:   []string{"workbuddy", "status:developing"},
		Detail:   "status:developing",
	}

	if err := sm1.HandleEvent(context.Background(), event); err != nil {
		t.Fatalf("coord-a HandleEvent: %v", err)
	}
	// Drain whatever coord-a dispatched.
	deadline := time.After(500 * time.Millisecond)
drain:
	for {
		select {
		case <-dispatch1:
		case <-deadline:
			break drain
		}
	}

	if err := sm2.HandleEvent(context.Background(), event); err != nil {
		t.Fatalf("coord-b HandleEvent: %v", err)
	}
	select {
	case req := <-dispatch2:
		t.Fatalf("coord-b should be blocked by issue claim, got %+v", req)
	case <-time.After(200 * time.Millisecond):
	}
	if got := rec2.find(eventlog.TypeDispatchSkippedClaim); len(got) != 1 {
		t.Fatalf("expected 1 dispatch_skipped_claim event on coord-b via dispatchStateAgents, got %d", len(got))
	}
}

// TestDispatchAgentReleasesOnContextCancel ensures a cancelled dispatch does
// not leave the issue claim stuck — another coordinator can acquire afterwards.
func TestDispatchAgentReleasesOnContextCancel(t *testing.T) {
	sm, _, dispatch := newTestSM(t)
	sm.SetIssueClaim("coord-a", time.Minute)

	// Fill the dispatch channel so the next send blocks.
	for i := 0; i < cap(dispatch); i++ {
		dispatch <- DispatchRequest{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sm.DispatchAgent(ctx, "test/repo", 1, "dev-agent", "dev-flow", "developing")
	if err == nil {
		t.Fatal("expected context.Canceled error from DispatchAgent")
	}

	// After cancellation the claim should have been released.
	claim, err := sm.store.QueryIssueClaim("test/repo", 1)
	if err != nil {
		t.Fatalf("QueryIssueClaim: %v", err)
	}
	if claim != nil {
		t.Fatalf("expected claim to be released after cancelled dispatch, got %+v", claim)
	}
}

func TestReleaseAllIssueClaims(t *testing.T) {
	sm, _, dispatch := newTestSM(t)
	sm.SetIssueClaim("coordinator-a", time.Minute)

	if err := sm.DispatchAgent(context.Background(), "test/repo", 7, "dev-agent", "dev-flow", "developing"); err != nil {
		t.Fatalf("DispatchAgent: %v", err)
	}
	select {
	case <-dispatch:
	case <-time.After(time.Second):
		t.Fatal("expected dispatch request")
	}

	claim, err := sm.store.QueryIssueClaim("test/repo", 7)
	if err != nil {
		t.Fatalf("QueryIssueClaim before release: %v", err)
	}
	if claim == nil {
		t.Fatal("expected claim before ReleaseAllIssueClaims")
	}

	sm.ReleaseAllIssueClaims()

	claim, err = sm.store.QueryIssueClaim("test/repo", 7)
	if err != nil {
		t.Fatalf("QueryIssueClaim after release: %v", err)
	}
	if claim != nil {
		t.Fatalf("expected claim to be deleted on shutdown, got %+v", claim)
	}
}

func TestRolloutDispatch_UsesLabelOverrideAndSharedGroupID(t *testing.T) {
	sm, rec, dispatch := newTestSM(t)
	sm.workflows["dev-flow"].States["developing"].Rollouts = 1
	sm.workflows["dev-flow"].States["developing"].Join = config.JoinConfig{Strategy: config.JoinRollouts, MinSuccesses: 2}

	err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type:     poller.EventIssueCreated,
		Repo:     "test/repo",
		IssueNum: 77,
		Labels:   []string{"workbuddy", "status:developing", "rollouts:3"},
	})
	if err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	var got []DispatchRequest
	for i := 0; i < 3; i++ {
		select {
		case req := <-dispatch:
			got = append(got, req)
		default:
			t.Fatalf("expected rollout dispatch %d", i+1)
		}
	}
	if extra := len(rec.find(eventlog.TypeRolloutDispatched)); extra != 3 {
		t.Fatalf("rollout_dispatched events = %d, want 3", extra)
	}
	groupID := got[0].RolloutGroupID
	if groupID == "" {
		t.Fatal("expected rollout group id")
	}
	for i, req := range got {
		if req.RolloutIndex != i+1 {
			t.Fatalf("dispatch %d rollout_index = %d, want %d", i, req.RolloutIndex, i+1)
		}
		if req.RolloutsTotal != 3 {
			t.Fatalf("dispatch %d rollouts_total = %d, want 3", i, req.RolloutsTotal)
		}
		if req.RolloutGroupID != groupID {
			t.Fatalf("dispatch %d group_id = %q, want %q", i, req.RolloutGroupID, groupID)
		}
	}
}

func TestRolloutCompletion_TransitionsAfterMinSuccessesAndTerminalSiblings(t *testing.T) {
	sm, rec, dispatch := newTestSM(t)
	sm.workflows["dev-flow"].States["developing"].Rollouts = 3
	sm.workflows["dev-flow"].States["developing"].Join = config.JoinConfig{Strategy: config.JoinRollouts, MinSuccesses: 2}
	sm.workflows["dev-flow"].States["developing"].Transitions = map[string]string{"status:reviewing": "reviewing"}
	sm.workflows["dev-flow"].States["reviewing"] = &config.State{EnterLabel: "status:reviewing"}

	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type:     poller.EventIssueCreated,
		Repo:     "test/repo",
		IssueNum: 78,
		Labels:   []string{"workbuddy", "status:developing"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	reqs := make([]DispatchRequest, 0, 3)
	for i := 0; i < 3; i++ {
		req := <-dispatch
		reqs = append(reqs, req)
		if err := sm.store.InsertTask(store.TaskRecord{
			ID:             fmt.Sprintf("task-%d", i+1),
			Repo:           req.Repo,
			IssueNum:       req.IssueNum,
			AgentName:      req.AgentName,
			Workflow:       req.Workflow,
			State:          req.State,
			RolloutIndex:   req.RolloutIndex,
			RolloutsTotal:  req.RolloutsTotal,
			RolloutGroupID: req.RolloutGroupID,
			Status:         store.TaskStatusPending,
		}); err != nil {
			t.Fatalf("InsertTask %d: %v", i+1, err)
		}
	}

	if err := sm.store.UpdateTaskStatus("task-1", store.TaskStatusCompleted); err != nil {
		t.Fatalf("UpdateTaskStatus task-1: %v", err)
	}
	sm.MarkAgentCompleted("test/repo", 78, "task-1", "dev-agent", 0, []string{"workbuddy", "status:reviewing"})
	if got := len(rec.find(eventlog.TypeTransition)); got != 0 {
		t.Fatalf("transitions after first completion = %d, want 0", got)
	}

	if err := sm.store.UpdateTaskStatus("task-2", store.TaskStatusFailed); err != nil {
		t.Fatalf("UpdateTaskStatus task-2: %v", err)
	}
	sm.MarkAgentCompleted("test/repo", 78, "task-2", "dev-agent", 1, []string{"workbuddy", "status:reviewing"})
	if got := len(rec.find(eventlog.TypeTransition)); got != 0 {
		t.Fatalf("transitions after second completion = %d, want 0", got)
	}

	if err := sm.store.UpdateTaskStatus("task-3", store.TaskStatusCompleted); err != nil {
		t.Fatalf("UpdateTaskStatus task-3: %v", err)
	}
	sm.MarkAgentCompleted("test/repo", 78, "task-3", "dev-agent", 0, []string{"workbuddy", "status:reviewing"})

	if got := len(rec.find(eventlog.TypeRolloutCompleted)); got != 3 {
		t.Fatalf("rollout_completed events = %d, want 3", got)
	}
	transitions := rec.find(eventlog.TypeTransition)
	if len(transitions) != 1 {
		t.Fatalf("transitions = %d, want 1", len(transitions))
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
