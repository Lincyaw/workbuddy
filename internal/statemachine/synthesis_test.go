package statemachine

import (
	"context"
	"fmt"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/poller"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/workflow"
)

func synthWorkflow() *config.WorkflowConfig {
	wf := testWorkflow()
	wf.MaxRetries = 99
	wf.States["developing"].Rollouts = 3
	wf.States["developing"].Join = config.JoinConfig{Strategy: config.JoinRollouts, MinSuccesses: 2}
	wf.States["developing"].Transitions = map[string]string{
		"status:synthesizing": "synthesizing",
		"status:reviewing":    "reviewing",
		"status:blocked":      "blocked",
	}
	wf.States["synthesizing"] = &config.State{
		EnterLabel: "status:synthesizing",
		Agent:      "review-agent",
		Mode:       config.StateModeSynth,
		Transitions: map[string]string{
			"status:reviewing": "reviewing",
		},
	}
	return wf
}

func newSynthSM(t *testing.T) (*StateMachine, *fakeRecorder, chan DispatchRequest) {
	t.Helper()
	st := newTestStore(t)
	rec := &fakeRecorder{}
	dispatch := make(chan DispatchRequest, 16)
	sm := NewStateMachine(map[string]*config.WorkflowConfig{"dev-flow": synthWorkflow()}, st, dispatch, rec, nil)
	return sm, rec, dispatch
}

func TestSynthesisFlow_PickTransitionsToReviewing(t *testing.T) {
	sm, rec, dispatch := newSynthSM(t)
	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type:     "issue_created",
		Repo:     "test/repo",
		IssueNum: 293,
		Labels:   []string{"workbuddy", "status:developing"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	var devReqs []DispatchRequest
	for i := 0; i < 3; i++ {
		devReqs = append(devReqs, <-dispatch)
		if err := sm.store.InsertTask(store.TaskRecord{
			ID:             fmt.Sprintf("dev-%d", i+1),
			Repo:           "test/repo",
			IssueNum:       293,
			AgentName:      "dev-agent",
			Workflow:       "dev-flow",
			State:          "developing",
			RolloutIndex:   i + 1,
			RolloutsTotal:  3,
			RolloutGroupID: devReqs[i].RolloutGroupID,
			Status:         store.TaskStatusCompleted,
		}); err != nil {
			t.Fatalf("InsertTask dev-%d: %v", i+1, err)
		}
		sm.MarkAgentCompleted("test/repo", 293, fmt.Sprintf("dev-%d", i+1), "dev-agent", 0, []string{"workbuddy", "status:synthesizing"})
	}

	synthReq := <-dispatch
	if synthReq.State != "synthesizing" || synthReq.AgentName != "review-agent" {
		t.Fatalf("unexpected synth dispatch: %+v", synthReq)
	}
	if err := sm.store.InsertTask(store.TaskRecord{
		ID:        "synth-1",
		Repo:      "test/repo",
		IssueNum:  293,
		AgentName: "review-agent",
		Workflow:  "dev-flow",
		State:     "synthesizing",
		Status:    store.TaskStatusCompleted,
	}); err != nil {
		t.Fatalf("InsertTask synth: %v", err)
	}
	sm.MarkAgentCompletedWithDecision("test/repo", 293, "synth-1", "review-agent", 0, []string{"workbuddy", "status:reviewing"}, &runtimepkg.SynthesisDecision{
		Outcome:     "pick",
		ChosenPR:    101,
		RejectedPRs: []int{102, 103},
		Reason:      "best candidate",
	})

	reviewReq := <-dispatch
	if reviewReq.State != "reviewing" {
		t.Fatalf("expected reviewing dispatch, got %+v", reviewReq)
	}
	events := rec.find(eventlog.TypeSynthesisDecision)
	if len(events) != 1 {
		t.Fatalf("synthesis_decision events = %d, want 1", len(events))
	}
	if err := sm.store.InsertTask(store.TaskRecord{
		ID:        "review-1",
		Repo:      "test/repo",
		IssueNum:  293,
		AgentName: "review-agent",
		Workflow:  "dev-flow",
		State:     "reviewing",
		Status:    store.TaskStatusCompleted,
	}); err != nil {
		t.Fatalf("InsertTask review: %v", err)
	}
	sm.MarkAgentCompleted("test/repo", 293, "review-1", "review-agent", 0, []string{"workbuddy", "status:done"})

	instances, err := workflow.NewManager(sm.store).QueryByRepoIssue("test/repo", 293)
	if err != nil {
		t.Fatalf("workflow query: %v", err)
	}
	if len(instances) != 1 || instances[0].CurrentState != "done" {
		t.Fatalf("workflow current_state = %+v, want done", instances)
	}
}

func TestSynthesisFlow_CherryPickTransitionsToReviewing(t *testing.T) {
	sm, rec, dispatch := newSynthSM(t)
	wm := workflow.NewManager(sm.store)
	if err := wm.CreateIfMissing("test/repo", 294, "dev-flow", "developing"); err != nil {
		t.Fatalf("CreateIfMissing: %v", err)
	}
	if err := wm.Advance("test/repo", 294, "dev-flow", "developing", "synthesizing", "review-agent"); err != nil {
		t.Fatalf("Advance developing->synthesizing: %v", err)
	}
	if err := sm.store.InsertTask(store.TaskRecord{
		ID:        "synth-cherry",
		Repo:      "test/repo",
		IssueNum:  294,
		AgentName: "review-agent",
		Workflow:  "dev-flow",
		State:     "synthesizing",
		Status:    store.TaskStatusCompleted,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	sm.inflight[sm.issueKey("test/repo", 294)] = newDispatchGroup("dev-flow", "synthesizing", config.StateModeSynth, config.JoinConfig{Strategy: config.JoinAllPassed}, map[string]string{"review-agent": "review-agent"})
	sm.inflight[sm.issueKey("test/repo", 294)].dispatchedSlots["review-agent"] = struct{}{}
	sm.MarkAgentCompletedWithDecision("test/repo", 294, "synth-cherry", "review-agent", 0, []string{"workbuddy", "status:reviewing"}, &runtimepkg.SynthesisDecision{
		Outcome:     "cherry-pick",
		SynthPR:     201,
		RejectedPRs: []int{101, 102, 103},
		Reason:      "combined the best parts into a new synth PR",
	})

	select {
	case req := <-dispatch:
		if req.State != "reviewing" || req.AgentName != "review-agent" {
			t.Fatalf("unexpected reviewing dispatch after cherry-pick: %+v", req)
		}
	default:
		t.Fatal("expected reviewing dispatch after cherry-pick")
	}

	events := rec.find(eventlog.TypeSynthesisDecision)
	if len(events) != 1 {
		t.Fatalf("synthesis_decision events = %d, want 1", len(events))
	}
	payload, ok := events[0].Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T", events[0].Payload)
	}
	if got := payload["outcome"]; got != "cherry-pick" {
		t.Fatalf("outcome = %v, want cherry-pick", got)
	}
	if got := payload["synth_pr"]; got != 201 {
		t.Fatalf("synth_pr = %v, want 201", got)
	}
}

func TestSynthesisFlow_EscalateBlocksFurtherDispatch(t *testing.T) {
	sm, rec, dispatch := newSynthSM(t)
	if err := sm.store.InsertTask(store.TaskRecord{ID: "synth-esc", Repo: "test/repo", IssueNum: 9, AgentName: "review-agent", Workflow: "dev-flow", State: "synthesizing", Status: store.TaskStatusCompleted}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	sm.inflight[sm.issueKey("test/repo", 9)] = newDispatchGroup("dev-flow", "synthesizing", config.StateModeSynth, config.JoinConfig{Strategy: config.JoinAllPassed}, map[string]string{"review-agent": "review-agent"})
	sm.inflight[sm.issueKey("test/repo", 9)].dispatchedSlots["review-agent"] = struct{}{}
	sm.MarkAgentCompletedWithDecision("test/repo", 9, "synth-esc", "review-agent", 0, []string{"workbuddy", "status:synthesizing"}, &runtimepkg.SynthesisDecision{
		Outcome:     "escalate",
		RejectedPRs: []int{1, 2, 3},
		Reason:      "none acceptable",
	})
	select {
	case req := <-dispatch:
		t.Fatalf("unexpected follow-up dispatch after escalate: %+v", req)
	default:
	}
	if len(rec.find(eventlog.TypeSynthesisDecision)) != 1 {
		t.Fatalf("expected synthesis_decision event")
	}
}

func TestSynthesisFlow_MalformedOutputFallsBackToEscalate(t *testing.T) {
	sm, rec, dispatch := newSynthSM(t)
	if err := sm.store.InsertTask(store.TaskRecord{ID: "synth-bad", Repo: "test/repo", IssueNum: 10, AgentName: "review-agent", Workflow: "dev-flow", State: "synthesizing", Status: store.TaskStatusFailed}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	sm.inflight[sm.issueKey("test/repo", 10)] = newDispatchGroup("dev-flow", "synthesizing", config.StateModeSynth, config.JoinConfig{Strategy: config.JoinAllPassed}, map[string]string{"review-agent": "review-agent"})
	sm.inflight[sm.issueKey("test/repo", 10)].dispatchedSlots["review-agent"] = struct{}{}
	sm.MarkAgentCompletedWithDecision("test/repo", 10, "synth-bad", "review-agent", 1, []string{"workbuddy", "status:synthesizing"}, nil)
	select {
	case req := <-dispatch:
		t.Fatalf("unexpected dispatch after malformed synth output: %+v", req)
	default:
	}
	events := rec.find(eventlog.TypeSynthesisDecision)
	if len(events) != 1 {
		t.Fatalf("synthesis_decision events = %d, want 1", len(events))
	}
}

func TestSynthesisFlow_ReviewBounceIncrementsSynthCycleCount(t *testing.T) {
	sm, _, dispatch := newSynthSM(t)
	const repo = "test/repo"
	const issue = 295

	wm := workflow.NewManager(sm.store)
	if err := wm.CreateIfMissing(repo, issue, "dev-flow", "developing"); err != nil {
		t.Fatalf("CreateIfMissing: %v", err)
	}
	if err := wm.Advance(repo, issue, "dev-flow", "developing", "synthesizing", "review-agent"); err != nil {
		t.Fatalf("Advance developing->synthesizing: %v", err)
	}
	if err := wm.Advance(repo, issue, "dev-flow", "synthesizing", "reviewing", "review-agent"); err != nil {
		t.Fatalf("Advance synthesizing->reviewing: %v", err)
	}

	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type:     poller.EventLabelAdded,
		Repo:     repo,
		IssueNum: issue,
		Labels:   []string{"workbuddy", "status:developing"},
		Detail:   "status:developing",
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	select {
	case req := <-dispatch:
		if req.AgentName != "dev-agent" {
			t.Fatalf("dispatch agent = %s, want dev-agent", req.AgentName)
		}
	default:
		t.Fatal("expected dev-agent redispatch after synth review rejection")
	}

	state, err := sm.store.QueryIssueCycleState(repo, issue)
	if err != nil {
		t.Fatalf("QueryIssueCycleState: %v", err)
	}
	if state == nil || state.SynthCycleCount != 1 {
		t.Fatalf("synth_cycle_count = %+v, want 1", state)
	}
	if state.DevReviewCycleCount != 0 {
		t.Fatalf("dev_review_cycle_count = %d, want 0", state.DevReviewCycleCount)
	}
}

func TestSynthesisFlow_SynthCycleCapBlocksRedispatch(t *testing.T) {
	sm, _, dispatch := newSynthSM(t)
	const repo = "test/repo"
	const issue = 296

	rep := &fakeCycleCapReporter{}
	sm.SetCycleCapReporter(rep)

	if _, err := sm.store.IncrementSynthCycleCount(repo, issue); err != nil {
		t.Fatalf("IncrementSynthCycleCount seed: %v", err)
	}

	wm := workflow.NewManager(sm.store)
	if err := wm.CreateIfMissing(repo, issue, "dev-flow", "developing"); err != nil {
		t.Fatalf("CreateIfMissing: %v", err)
	}
	if err := wm.Advance(repo, issue, "dev-flow", "developing", "synthesizing", "review-agent"); err != nil {
		t.Fatalf("Advance developing->synthesizing: %v", err)
	}
	if err := wm.Advance(repo, issue, "dev-flow", "synthesizing", "reviewing", "review-agent"); err != nil {
		t.Fatalf("Advance synthesizing->reviewing: %v", err)
	}

	if err := sm.HandleEvent(context.Background(), ChangeEvent{
		Type:     poller.EventLabelAdded,
		Repo:     repo,
		IssueNum: issue,
		Labels:   []string{"workbuddy", "status:developing"},
		Detail:   "status:developing",
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	select {
	case req := <-dispatch:
		t.Fatalf("unexpected redispatch after hitting synth cap: %+v", req)
	default:
	}

	state, err := sm.store.QueryIssueCycleState(repo, issue)
	if err != nil {
		t.Fatalf("QueryIssueCycleState: %v", err)
	}
	if state == nil || state.SynthCycleCount != DefaultMaxSynthCycles {
		t.Fatalf("synth_cycle_count = %+v, want %d", state, DefaultMaxSynthCycles)
	}
	if state.SynthCapHitAt.IsZero() {
		t.Fatalf("expected synth_cap_hit_at to be recorded")
	}
	if len(rep.calls) != 1 {
		t.Fatalf("cap reporter calls = %d, want 1", len(rep.calls))
	}
}
