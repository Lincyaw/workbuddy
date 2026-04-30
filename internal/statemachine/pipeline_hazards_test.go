package statemachine

import (
	"context"
	"strings"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// Scenario A (REQ #255): issue carries status:* but no workflow trigger label
// matches. The state machine must record an INFO event + persistent hazard so
// the issue surfaces in `workbuddy status` / `diagnose`. Repeat events with
// the same labels must NOT log a duplicate event (idempotent).
func TestHandleEvent_NoWorkflowMatch_LogsHazardIdempotent(t *testing.T) {
	sm, rec, _ := newTestSM(t)
	repo := "test/repo"
	issueNum := 42

	event := ChangeEvent{
		Type:     poller.EventLabelAdded,
		Repo:     repo,
		IssueNum: issueNum,
		Labels:   []string{"bug", "status:developing"}, // no workflow trigger label
		Detail:   "status:developing",
	}

	if err := sm.HandleEvent(context.Background(), event); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	got := rec.find(eventlog.TypeIssueNoWorkflowMatch)
	if len(got) != 1 {
		t.Fatalf("expected 1 issue_no_workflow_match event, got %d", len(got))
	}

	hazard, err := sm.store.QueryIssuePipelineHazard(repo, issueNum)
	if err != nil {
		t.Fatalf("query hazard: %v", err)
	}
	if hazard == nil {
		t.Fatalf("expected hazard row to be persisted")
	}
	if hazard.Kind != store.HazardKindNoWorkflowMatch {
		t.Errorf("hazard kind = %q, want %q", hazard.Kind, store.HazardKindNoWorkflowMatch)
	}

	// Reset HandleEvent's per-poll dedup so the same event can flow again
	// (simulates a subsequent poll cycle observing identical labels).
	sm.ResetDedup()

	// Same labels — no new event should be logged.
	event2 := event
	event2.Type = poller.EventIssueCreated
	event2.Detail = "ignored"
	if err := sm.HandleEvent(context.Background(), event2); err != nil {
		t.Fatalf("HandleEvent (second): %v", err)
	}
	got2 := rec.find(eventlog.TypeIssueNoWorkflowMatch)
	if len(got2) != 1 {
		t.Fatalf("expected hazard event to remain idempotent, got %d events", len(got2))
	}

	// Adding a new label changes the fingerprint — a fresh event MUST fire.
	sm.ResetDedup()
	event3 := event
	event3.Labels = append([]string{"newlabel"}, event.Labels...)
	if err := sm.HandleEvent(context.Background(), event3); err != nil {
		t.Fatalf("HandleEvent (third): %v", err)
	}
	if got3 := rec.find(eventlog.TypeIssueNoWorkflowMatch); len(got3) != 2 {
		t.Errorf("expected fingerprint change to re-emit hazard event, got %d total", len(got3))
	}

	// Adding the trigger label clears the hazard.
	sm.ResetDedup()
	event4 := event
	event4.Labels = []string{"workbuddy", "status:developing"}
	if err := sm.HandleEvent(context.Background(), event4); err != nil {
		t.Fatalf("HandleEvent (fourth): %v", err)
	}
	hazard, err = sm.store.QueryIssuePipelineHazard(repo, issueNum)
	if err != nil {
		t.Fatalf("query hazard after recovery: %v", err)
	}
	if hazard != nil {
		t.Errorf("expected hazard cleared after trigger label added, got %+v", hazard)
	}
}

// Scenario B (REQ #255): workflow trigger label present + depends_on declared
// in body, but no status:* label. State machine must record an INFO event +
// hazard. Repeat events with unchanged labels+body must be idempotent.
func TestHandleEvent_DependencyUnentered_LogsHazardIdempotent(t *testing.T) {
	sm, rec, _ := newTestSM(t)
	repo := "test/repo"
	issueNum := 7

	body := "Some prelude.\n\n```yaml\nworkbuddy:\n  depends_on:\n    - \"#3\"\n    - \"#5\"\n```\n"
	if err := sm.store.UpsertIssueCache(store.IssueCache{
		Repo:     repo,
		IssueNum: issueNum,
		Labels:   `["workbuddy"]`,
		Body:     body,
		State:    "open",
	}); err != nil {
		t.Fatalf("seed issue cache: %v", err)
	}

	event := ChangeEvent{
		Type:     poller.EventLabelAdded,
		Repo:     repo,
		IssueNum: issueNum,
		Labels:   []string{"workbuddy"}, // trigger present, no status:* label
		Detail:   "workbuddy",
	}
	if err := sm.HandleEvent(context.Background(), event); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	got := rec.find(eventlog.TypeIssueDependencyUnentered)
	if len(got) != 1 {
		t.Fatalf("expected 1 issue_dependency_unentered event, got %d", len(got))
	}
	payload, _ := got[0].Payload.(map[string]any)
	deps, _ := payload["depends_on"].([]string)
	if len(deps) != 2 || !strings.Contains(strings.Join(deps, ","), "#3") {
		t.Errorf("expected depends_on payload to include parsed refs, got %v", payload["depends_on"])
	}

	hazard, err := sm.store.QueryIssuePipelineHazard(repo, issueNum)
	if err != nil {
		t.Fatalf("query hazard: %v", err)
	}
	if hazard == nil || hazard.Kind != store.HazardKindAwaitingStatusLabel {
		t.Fatalf("expected awaiting-status-label hazard, got %+v", hazard)
	}

	// Same labels + body — idempotent.
	sm.ResetDedup()
	event2 := event
	event2.Type = poller.EventIssueCreated
	event2.Detail = "noop"
	if err := sm.HandleEvent(context.Background(), event2); err != nil {
		t.Fatalf("HandleEvent (second): %v", err)
	}
	if got2 := rec.find(eventlog.TypeIssueDependencyUnentered); len(got2) != 1 {
		t.Fatalf("expected hazard event to remain idempotent, got %d events", len(got2))
	}

	// Adding status:developing clears the hazard once the issue enters the
	// workflow state machine.
	sm.ResetDedup()
	event3 := event
	event3.Labels = []string{"workbuddy", "status:developing"}
	event3.Detail = "status:developing"
	if err := sm.HandleEvent(context.Background(), event3); err != nil {
		t.Fatalf("HandleEvent (third): %v", err)
	}
	hazard, err = sm.store.QueryIssuePipelineHazard(repo, issueNum)
	if err != nil {
		t.Fatalf("query hazard after recovery: %v", err)
	}
	if hazard != nil {
		t.Errorf("expected hazard cleared after status:* added, got %+v", hazard)
	}
}
