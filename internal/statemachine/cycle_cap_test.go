package statemachine

import (
	"context"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/alertbus"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/poller"
)

// fakeCycleCapReporter records ReportDevReviewCycleCap calls so tests can
// assert the cap-hit handler invoked the comment side-effect.
type fakeCycleCapReporter struct {
	calls []CycleCapInfo
}

func (f *fakeCycleCapReporter) ReportDevReviewCycleCap(_ context.Context, _ string, _ int, info CycleCapInfo) error {
	f.calls = append(f.calls, info)
	return nil
}

// newCapTestSM builds a StateMachine wired with the default-style workflow
// (developing/reviewing/done/blocked) and the supplied max_review_cycles.
func newCapTestSM(t *testing.T, maxReviewCycles int) (*StateMachine, *fakeRecorder, chan DispatchRequest, *alertbus.Bus, *fakeCycleCapReporter) {
	t.Helper()
	st := newTestStore(t)
	rec := &fakeRecorder{}
	dispatch := make(chan DispatchRequest, 16)
	bus := alertbus.NewBus(64)
	wf := &config.WorkflowConfig{
		Name:            "dev-flow",
		MaxRetries:      99, // do not interfere with the new cap
		MaxReviewCycles: maxReviewCycles,
		Trigger:         config.WorkflowTrigger{IssueLabel: "workbuddy"},
		States: map[string]*config.State{
			"developing": {
				EnterLabel: "status:developing",
				Agent:      "dev-agent",
				Transitions: map[string]string{
					"status:reviewing": "reviewing",
					"status:blocked":   "blocked",
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
			"blocked": {EnterLabel: "status:blocked"},
			"done":    {EnterLabel: "status:done"},
		},
	}
	sm := NewStateMachine(map[string]*config.WorkflowConfig{"dev-flow": wf}, st, dispatch, rec, bus)
	rep := &fakeCycleCapReporter{}
	sm.SetCycleCapReporter(rep)
	return sm, rec, dispatch, bus, rep
}

// stepStateEntry simulates a production state-entry (atomic label swap)
// for a single workflow state. Use this between MarkAgentCompleted and
// the next HandleEvent to advance the state machine the way the poller
// would after an agent flips labels.
func stepStateEntry(t *testing.T, sm *StateMachine, repo string, issueNum int, enterLabel string) {
	t.Helper()
	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type:     poller.EventLabelAdded,
		Repo:     repo,
		IssueNum: issueNum,
		Labels:   []string{"workbuddy", enterLabel},
		Detail:   enterLabel,
	}); err != nil {
		t.Fatalf("HandleEvent %s: %v", enterLabel, err)
	}
	sm.ResetDedup()
}

// TestCycleCapCleanIssueNoExtraCycle: a fresh issue entering developing once
// must not increment dev_review_cycle_count.
func TestCycleCapCleanIssueNoExtraCycle(t *testing.T) {
	sm, _, dispatch, _, rep := newCapTestSM(t, 3)
	const repo = "test/repo"
	const issue = 100

	stepStateEntry(t, sm, repo, issue, "status:developing")
	<-dispatch
	sm.MarkAgentCompleted(repo, issue, "task-dev-1", "dev-agent", 0, []string{"workbuddy", "status:developing"})

	state, err := sm.store.QueryIssueCycleState(repo, issue)
	if err != nil {
		t.Fatalf("QueryIssueCycleState: %v", err)
	}
	if state != nil && state.DevReviewCycleCount != 0 {
		t.Fatalf("dev_review_cycle_count = %d, want 0", state.DevReviewCycleCount)
	}
	if len(rep.calls) != 0 {
		t.Fatalf("unexpected cap-hit calls: %+v", rep.calls)
	}
}

// TestCycleCapOneCycleIncrementsCounter: developing→reviewing→developing
// (one full round-trip) must set dev_review_cycle_count to 1 and still
// dispatch the second dev-agent run.
func TestCycleCapOneCycleIncrementsCounter(t *testing.T) {
	sm, _, dispatch, _, rep := newCapTestSM(t, 3)
	const repo = "test/repo"
	const issue = 200

	// Initial entry into developing.
	stepStateEntry(t, sm, repo, issue, "status:developing")
	<-dispatch
	sm.MarkAgentCompleted(repo, issue, "t1", "dev-agent", 0, []string{"workbuddy", "status:developing"})

	// Dev-agent flipped to reviewing.
	stepStateEntry(t, sm, repo, issue, "status:reviewing")
	<-dispatch
	sm.MarkAgentCompleted(repo, issue, "t2", "review-agent", 1, []string{"workbuddy", "status:reviewing"})

	// Review-agent rejected → flipped back to developing.
	stepStateEntry(t, sm, repo, issue, "status:developing")
	select {
	case req := <-dispatch:
		if req.AgentName != "dev-agent" {
			t.Fatalf("expected dev-agent dispatch on second developing entry, got %q", req.AgentName)
		}
	default:
		t.Fatalf("expected dev-agent dispatch on second developing entry")
	}

	state, err := sm.store.QueryIssueCycleState(repo, issue)
	if err != nil {
		t.Fatalf("QueryIssueCycleState: %v", err)
	}
	if state == nil || state.DevReviewCycleCount != 1 {
		t.Fatalf("dev_review_cycle_count = %+v, want 1", state)
	}
	if len(rep.calls) != 0 {
		t.Fatalf("unexpected cap-hit calls at cycle 1: %+v", rep.calls)
	}
}

// TestCycleCapApproachingAlertAtCapMinusOne: at cycle == cap - 1 the
// state machine must publish a heads-up alert AND continue to dispatch.
func TestCycleCapApproachingAlertAtCapMinusOne(t *testing.T) {
	sm, _, dispatch, bus, _ := newCapTestSM(t, 3)
	subID, ch := bus.Subscribe()
	t.Cleanup(func() { bus.Unsubscribe(subID) })

	const repo = "test/repo"
	const issue = 300

	// Initial entry.
	stepStateEntry(t, sm, repo, issue, "status:developing")
	<-dispatch
	sm.MarkAgentCompleted(repo, issue, "t1", "dev-agent", 0, []string{"workbuddy", "status:developing"})

	// Cycle 1: review→dev.
	stepStateEntry(t, sm, repo, issue, "status:reviewing")
	<-dispatch
	sm.MarkAgentCompleted(repo, issue, "t2", "review-agent", 1, []string{"workbuddy", "status:reviewing"})
	stepStateEntry(t, sm, repo, issue, "status:developing")
	<-dispatch // dev-agent re-dispatched
	sm.MarkAgentCompleted(repo, issue, "t3", "dev-agent", 0, []string{"workbuddy", "status:developing"})

	// Cycle 2 (cap-1=2 should trigger heads-up).
	stepStateEntry(t, sm, repo, issue, "status:reviewing")
	<-dispatch
	sm.MarkAgentCompleted(repo, issue, "t4", "review-agent", 1, []string{"workbuddy", "status:reviewing"})
	stepStateEntry(t, sm, repo, issue, "status:developing")
	select {
	case req := <-dispatch:
		if req.AgentName != "dev-agent" {
			t.Fatalf("expected dev-agent dispatch at cap-1, got %q", req.AgentName)
		}
	default:
		t.Fatalf("expected dev-agent dispatch at cap-1")
	}

	// Drain alert events looking for the approaching kind.
	gotApproaching := false
	for drain := true; drain; {
		select {
		case ev := <-ch:
			if ev.Kind == alertbus.KindDevReviewCycleApproaching {
				gotApproaching = true
			}
		default:
			drain = false
		}
	}
	if !gotApproaching {
		t.Fatalf("expected dev_review_cycle_approaching alert at cycle %d/%d", 2, 3)
	}
}

// TestCycleCapHitBlocksDispatchAndPostsComment: at cycle == cap the
// state machine must NOT dispatch, must record cap_hit_at, and must
// invoke the cycle-cap reporter callback.
func TestCycleCapHitBlocksDispatchAndPostsComment(t *testing.T) {
	sm, rec, dispatch, _, rep := newCapTestSM(t, 2) // cap=2 so we hit it on the second dev re-entry
	const repo = "test/repo"
	const issue = 400

	stepStateEntry(t, sm, repo, issue, "status:developing")
	<-dispatch
	sm.MarkAgentCompleted(repo, issue, "t1", "dev-agent", 0, []string{"workbuddy", "status:developing"})

	// Cycle 1.
	stepStateEntry(t, sm, repo, issue, "status:reviewing")
	<-dispatch
	sm.MarkAgentCompleted(repo, issue, "t2", "review-agent", 1, []string{"workbuddy", "status:reviewing"})
	stepStateEntry(t, sm, repo, issue, "status:developing")
	<-dispatch
	sm.MarkAgentCompleted(repo, issue, "t3", "dev-agent", 0, []string{"workbuddy", "status:developing"})

	// Cycle 2 — should hit cap=2.
	stepStateEntry(t, sm, repo, issue, "status:reviewing")
	<-dispatch
	sm.MarkAgentCompleted(repo, issue, "t4", "review-agent", 1, []string{"workbuddy", "status:reviewing"})
	stepStateEntry(t, sm, repo, issue, "status:developing")

	select {
	case req := <-dispatch:
		t.Fatalf("dispatch must be blocked at cap, got %+v", req)
	default:
	}

	state, err := sm.store.QueryIssueCycleState(repo, issue)
	if err != nil {
		t.Fatalf("QueryIssueCycleState: %v", err)
	}
	if state == nil {
		t.Fatalf("issue_cycle_state row missing")
	}
	if state.DevReviewCycleCount != 2 {
		t.Fatalf("dev_review_cycle_count = %d, want 2", state.DevReviewCycleCount)
	}
	if state.CapHitAt.IsZero() {
		t.Fatalf("cap_hit_at not recorded")
	}
	if len(rep.calls) != 1 {
		t.Fatalf("cap-hit reporter calls = %d, want 1", len(rep.calls))
	}
	info := rep.calls[0]
	if info.WorkflowName != "dev-flow" || info.MaxReviewCycles != 2 || info.CycleCount != 2 {
		t.Fatalf("CycleCapInfo = %+v", info)
	}
	if len(rec.find(eventlog.TypeDevReviewCycleCapReached)) != 1 {
		t.Fatalf("expected dev_review_cycle_cap_reached event")
	}
}

// TestCycleCapResetOnBlockedToDeveloping: after the cap is hit and a human
// flips status:blocked → status:developing, the cycle counter must reset
// to 0 and a subsequent review→developing round-trip must increment to 1
// (not cap+1). This is the Option A semantic from REQ-085 maintainer
// override: "Counter resets when a human transitions status:blocked →
// status:developing".
func TestCycleCapResetOnBlockedToDeveloping(t *testing.T) {
	sm, rec, dispatch, _, rep := newCapTestSM(t, 2) // cap=2 so we hit it on the second dev re-entry
	const repo = "test/repo"
	const issue = 600

	// Drive developing → reviewing → developing → reviewing → developing
	// up to the cap.
	stepStateEntry(t, sm, repo, issue, "status:developing")
	<-dispatch
	sm.MarkAgentCompleted(repo, issue, "t1", "dev-agent", 0, []string{"workbuddy", "status:developing"})

	stepStateEntry(t, sm, repo, issue, "status:reviewing")
	<-dispatch
	sm.MarkAgentCompleted(repo, issue, "t2", "review-agent", 1, []string{"workbuddy", "status:reviewing"})

	// Cycle 1 (count=1).
	stepStateEntry(t, sm, repo, issue, "status:developing")
	<-dispatch
	sm.MarkAgentCompleted(repo, issue, "t3", "dev-agent", 0, []string{"workbuddy", "status:developing"})

	stepStateEntry(t, sm, repo, issue, "status:reviewing")
	<-dispatch
	sm.MarkAgentCompleted(repo, issue, "t4", "review-agent", 1, []string{"workbuddy", "status:reviewing"})

	// Cycle 2 — cap-hit, dispatch blocked.
	stepStateEntry(t, sm, repo, issue, "status:developing")
	select {
	case req := <-dispatch:
		t.Fatalf("dispatch must be blocked at cap, got %+v", req)
	default:
	}

	state, err := sm.store.QueryIssueCycleState(repo, issue)
	if err != nil {
		t.Fatalf("QueryIssueCycleState pre-reset: %v", err)
	}
	if state == nil || state.DevReviewCycleCount != 2 || state.CapHitAt.IsZero() {
		t.Fatalf("expected cap-hit state pre-reset, got %+v", state)
	}
	if len(rep.calls) != 1 {
		t.Fatalf("expected 1 cap-hit reporter call before reset, got %d", len(rep.calls))
	}

	// Human flips status:blocked. With the workflow_instance now advancing
	// through stateless states, this records "blocked" as the new
	// current_state in workflow_instance.
	stepStateEntry(t, sm, repo, issue, "status:blocked")
	select {
	case req := <-dispatch:
		t.Fatalf("status:blocked must not dispatch, got %+v", req)
	default:
	}

	// Human flips back to status:developing — Option A reset must fire.
	stepStateEntry(t, sm, repo, issue, "status:developing")
	select {
	case req := <-dispatch:
		if req.AgentName != "dev-agent" {
			t.Fatalf("expected dev-agent dispatch after reset, got %q", req.AgentName)
		}
	default:
		t.Fatalf("expected dev-agent dispatch after blocked→developing reset")
	}

	// Counter must be 0 (or row absent) and cap_hit_at must be cleared.
	state, err = sm.store.QueryIssueCycleState(repo, issue)
	if err != nil {
		t.Fatalf("QueryIssueCycleState post-reset: %v", err)
	}
	if state != nil {
		if state.DevReviewCycleCount != 0 {
			t.Fatalf("dev_review_cycle_count post-reset = %d, want 0", state.DevReviewCycleCount)
		}
		if !state.CapHitAt.IsZero() {
			t.Fatalf("cap_hit_at post-reset = %v, want zero", state.CapHitAt)
		}
	}

	// The reset event must have been logged.
	if len(rec.find(eventlog.TypeDevReviewCycleCountReset)) != 1 {
		t.Fatalf("expected dev_review_cycle_count_reset event, got %d", len(rec.find(eventlog.TypeDevReviewCycleCountReset)))
	}

	// A subsequent review→developing round-trip must increment to 1, not cap+1.
	sm.MarkAgentCompleted(repo, issue, "t5", "dev-agent", 0, []string{"workbuddy", "status:developing"})
	stepStateEntry(t, sm, repo, issue, "status:reviewing")
	<-dispatch
	sm.MarkAgentCompleted(repo, issue, "t6", "review-agent", 1, []string{"workbuddy", "status:reviewing"})

	stepStateEntry(t, sm, repo, issue, "status:developing")
	select {
	case req := <-dispatch:
		if req.AgentName != "dev-agent" {
			t.Fatalf("expected dev-agent dispatch on first post-reset cycle, got %q", req.AgentName)
		}
	default:
		t.Fatalf("expected dev-agent dispatch on first post-reset cycle")
	}

	state, err = sm.store.QueryIssueCycleState(repo, issue)
	if err != nil {
		t.Fatalf("QueryIssueCycleState post-cycle: %v", err)
	}
	if state == nil || state.DevReviewCycleCount != 1 {
		t.Fatalf("dev_review_cycle_count post-cycle = %+v, want 1", state)
	}

	// No new cap-hit reporter call: the post-reset cycle is at 1, well below cap=2.
	if len(rep.calls) != 1 {
		t.Fatalf("unexpected extra cap-hit reporter calls: %d", len(rep.calls))
	}
}

// TestCycleCapTouchFirstDispatch: state-entry into developing must record
// first_dispatch_at on the first encounter so the long-flight stuck
// detector has a baseline.
func TestCycleCapTouchFirstDispatch(t *testing.T) {
	sm, _, dispatch, _, _ := newCapTestSM(t, 3)
	const repo = "test/repo"
	const issue = 500

	stepStateEntry(t, sm, repo, issue, "status:developing")
	<-dispatch
	sm.MarkAgentCompleted(repo, issue, "t1", "dev-agent", 0, []string{"workbuddy", "status:developing"})

	state, err := sm.store.QueryIssueCycleState(repo, issue)
	if err != nil {
		t.Fatalf("QueryIssueCycleState: %v", err)
	}
	if state == nil || state.FirstDispatchAt.IsZero() {
		t.Fatalf("first_dispatch_at not recorded for %+v", state)
	}
}
