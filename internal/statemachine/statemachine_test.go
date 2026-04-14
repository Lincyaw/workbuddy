package statemachine

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/store"
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
				Transitions: []config.Transition{
					{To: "reviewing", When: `labeled "status:reviewing"`},
				},
			},
			"reviewing": {
				EnterLabel: "status:reviewing",
				Agent:      "review-agent",
				Transitions: []config.Transition{
					{To: "developing", When: `labeled "status:developing"`},
					{To: "done", When: `labeled "status:done"`},
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
	t.Cleanup(func() { s.Close() })
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
	)
	return sm, rec, dispatch
}

// Test 1: Normal transition (developing → reviewing)
func TestNormalTransition(t *testing.T) {
	sm, rec, dispatch := newTestSM(t)

	event := ChangeEvent{
		Type:     "label_added",
		Repo:     "test/repo",
		IssueNum: 1,
		Labels:   []string{"workbuddy", "status:developing"},
		Detail:   "status:reviewing",
	}

	if err := sm.HandleEvent(event); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	// Should have logged a transition.
	transitions := rec.find("transition")
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
}

// Test 2: Back-edge count increment (reviewing → developing)
func TestBackEdgeCount(t *testing.T) {
	sm, _, dispatch := newTestSM(t)

	// First: developing → reviewing (creates history for reviewing).
	ev1 := ChangeEvent{
		Type:     "label_added",
		Repo:     "test/repo",
		IssueNum: 2,
		Labels:   []string{"workbuddy", "status:developing"},
		Detail:   "status:reviewing",
	}
	if err := sm.HandleEvent(ev1); err != nil {
		t.Fatalf("HandleEvent 1: %v", err)
	}
	<-dispatch // drain

	// Mark agent complete so inflight is cleared.
	sm.MarkAgentCompleted("test/repo", 2, []string{"workbuddy", "status:reviewing"})
	sm.ResetDedup() // new poll cycle

	// Now: reviewing → developing (this is a back-edge since developing was visited).
	ev2 := ChangeEvent{
		Type:     "label_added",
		Repo:     "test/repo",
		IssueNum: 2,
		Labels:   []string{"workbuddy", "status:reviewing"},
		Detail:   "status:developing",
	}
	if err := sm.HandleEvent(ev2); err != nil {
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
	sm.HandleEvent(ChangeEvent{
		Type: "label_added", Repo: repo, IssueNum: issueNum,
		Labels: []string{"workbuddy", "status:developing"}, Detail: "status:reviewing",
	})
	<-dispatch
	sm.MarkAgentCompleted(repo, issueNum, []string{"workbuddy", "status:reviewing"})
	sm.ResetDedup()

	// Step 2: reviewing → developing (back-edge: "developing" was a source, but
	// not yet a target. Actually, let's check: is there any prior transition TO developing?
	// No — step 1 was TO reviewing. So this is NOT a back-edge.)
	sm.HandleEvent(ChangeEvent{
		Type: "label_added", Repo: repo, IssueNum: issueNum,
		Labels: []string{"workbuddy", "status:reviewing"}, Detail: "status:developing",
	})
	<-dispatch
	sm.MarkAgentCompleted(repo, issueNum, []string{"workbuddy", "status:developing"})
	sm.ResetDedup()

	// Step 3: developing → reviewing. "reviewing" already appeared as target in step 1.
	// This IS a back-edge. Count for developing→reviewing: was 1, increment to 2.
	// 2 >= max_retries (2) → cycle_limit_reached → transition to failed.
	err := sm.HandleEvent(ChangeEvent{
		Type: "label_added", Repo: repo, IssueNum: issueNum,
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
	if len(rec.find("cycle_limit_reached")) == 0 {
		t.Error("expected cycle_limit_reached event")
	}
	if len(rec.find("transition_to_failed")) == 0 {
		t.Error("expected transition_to_failed event")
	}
}

// Test 4: No matching workflow → skip silently
func TestNoMatchSkip(t *testing.T) {
	sm, rec, _ := newTestSM(t)

	err := sm.HandleEvent(ChangeEvent{
		Type:     "label_added",
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
	)

	err := sm.HandleEvent(ChangeEvent{
		Type:     "label_added",
		Repo:     "test/repo",
		IssueNum: 10,
		Labels:   []string{"workbuddy", "status:developing"},
		Detail:   "status:reviewing",
	})
	if err == nil {
		t.Error("expected error for multi-workflow match")
	}

	if len(rec.find("error_multi_workflow")) == 0 {
		t.Error("expected error_multi_workflow event")
	}
}

// Test 6: Idempotency — same event processed twice, only first takes effect
func TestIdempotent(t *testing.T) {
	sm, rec, dispatch := newTestSM(t)

	event := ChangeEvent{
		Type:     "label_added",
		Repo:     "test/repo",
		IssueNum: 5,
		Labels:   []string{"workbuddy", "status:developing"},
		Detail:   "status:reviewing",
	}

	if err := sm.HandleEvent(event); err != nil {
		t.Fatalf("HandleEvent 1: %v", err)
	}
	<-dispatch // drain first dispatch

	// Same event again.
	if err := sm.HandleEvent(event); err != nil {
		t.Fatalf("HandleEvent 2: %v", err)
	}

	// Should have only 1 transition event.
	transitions := rec.find("transition")
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
	sm.HandleEvent(ChangeEvent{
		Type: "label_added", Repo: "r", IssueNum: 7,
		Labels: []string{"workbuddy", "status:developing"}, Detail: "status:reviewing",
	})
	<-dispatch // drain; agent is now inflight

	// Don't mark complete — agent still running.
	sm.ResetDedup() // simulate new poll cycle

	// Now try reviewing → developing (which would dispatch dev-agent).
	sm.HandleEvent(ChangeEvent{
		Type: "label_added", Repo: "r", IssueNum: 7,
		Labels: []string{"workbuddy", "status:reviewing"}, Detail: "status:developing",
	})

	// Should not get a second dispatch.
	select {
	case <-dispatch:
		t.Error("should not dispatch while agent inflight")
	default:
		// good
	}

	// Should have logged the skip.
	if len(rec.find("dispatch_skipped_inflight")) == 0 {
		t.Error("expected dispatch_skipped_inflight event")
	}
}

// Test 8: Stuck detection
func TestStuckDetection(t *testing.T) {
	sm, rec, dispatch := newTestSM(t)
	sm.SetStuckTimeout(1 * time.Millisecond)

	// Trigger a transition.
	sm.HandleEvent(ChangeEvent{
		Type: "label_added", Repo: "test/repo", IssueNum: 8,
		Labels: []string{"workbuddy", "status:developing"}, Detail: "status:reviewing",
	})
	<-dispatch

	// Mark agent completed.
	labels := []string{"workbuddy", "status:reviewing"}
	sm.MarkAgentCompleted("test/repo", 8, labels)

	// Wait for stuck timeout.
	time.Sleep(5 * time.Millisecond)

	// Check stuck with same labels (unchanged).
	sm.CheckStuck("test/repo", 8, labels)

	if len(rec.find("stuck_detected")) == 0 {
		t.Error("expected stuck_detected event")
	}
}

// =============================================
// Rules engine tests (REQ-013)
// =============================================

func TestConditionLabeled_Positive(t *testing.T) {
	ctx := &EvalContext{
		EventType:  "label_added",
		LabelAdded: "status:reviewing",
		Labels:     []string{"status:reviewing"},
	}
	if !EvaluateCondition(`labeled "status:reviewing"`, ctx) {
		t.Error("labeled condition should match")
	}
}

func TestConditionLabeled_Negative(t *testing.T) {
	ctx := &EvalContext{
		EventType:  "label_added",
		LabelAdded: "status:developing",
		Labels:     []string{"status:developing"},
	}
	if EvaluateCondition(`labeled "status:reviewing"`, ctx) {
		t.Error("labeled condition should not match different label")
	}
}

func TestConditionPrOpened_Positive(t *testing.T) {
	ctx := &EvalContext{EventType: "pr_created"}
	if !EvaluateCondition("pr_opened", ctx) {
		t.Error("pr_opened should match pr_created event")
	}
}

func TestConditionPrOpened_Negative(t *testing.T) {
	ctx := &EvalContext{EventType: "label_added"}
	if EvaluateCondition("pr_opened", ctx) {
		t.Error("pr_opened should not match label_added event")
	}
}

func TestConditionChecksPassed_Positive(t *testing.T) {
	ctx := &EvalContext{ChecksState: "passed"}
	if !EvaluateCondition("checks_passed", ctx) {
		t.Error("checks_passed should match")
	}
}

func TestConditionChecksPassed_Negative(t *testing.T) {
	ctx := &EvalContext{ChecksState: "failed"}
	if EvaluateCondition("checks_passed", ctx) {
		t.Error("checks_passed should not match failed")
	}
}

func TestConditionChecksFailed_Positive(t *testing.T) {
	ctx := &EvalContext{ChecksState: "failed"}
	if !EvaluateCondition("checks_failed", ctx) {
		t.Error("checks_failed should match")
	}
}

func TestConditionChecksFailed_Negative(t *testing.T) {
	ctx := &EvalContext{ChecksState: "passed"}
	if EvaluateCondition("checks_failed", ctx) {
		t.Error("checks_failed should not match passed")
	}
}

func TestConditionApproved_Positive(t *testing.T) {
	ctx := &EvalContext{EventType: "approved"}
	if !EvaluateCondition("approved", ctx) {
		t.Error("approved should match")
	}
}

func TestConditionApproved_Negative(t *testing.T) {
	ctx := &EvalContext{EventType: "changes_requested"}
	if EvaluateCondition("approved", ctx) {
		t.Error("approved should not match changes_requested")
	}
}

func TestConditionChangesRequested_Positive(t *testing.T) {
	ctx := &EvalContext{EventType: "changes_requested"}
	if !EvaluateCondition("changes_requested", ctx) {
		t.Error("changes_requested should match")
	}
}

func TestConditionChangesRequested_Negative(t *testing.T) {
	ctx := &EvalContext{EventType: "approved"}
	if EvaluateCondition("changes_requested", ctx) {
		t.Error("changes_requested should not match approved")
	}
}

func TestConditionCommentCommand_Positive(t *testing.T) {
	ctx := &EvalContext{LatestComment: "/approve this looks good"}
	if !EvaluateCondition(`comment_command "/approve"`, ctx) {
		t.Error("comment_command should match")
	}
}

func TestConditionCommentCommand_Negative(t *testing.T) {
	ctx := &EvalContext{LatestComment: "great job"}
	if EvaluateCondition(`comment_command "/approve"`, ctx) {
		t.Error("comment_command should not match when command absent")
	}
}

func TestConditionUnknown(t *testing.T) {
	ctx := &EvalContext{}
	if EvaluateCondition("some_future_condition", ctx) {
		t.Error("unknown condition should return false")
	}
}

func TestConditionEmpty(t *testing.T) {
	ctx := &EvalContext{}
	if EvaluateCondition("", ctx) {
		t.Error("empty condition should return false")
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
