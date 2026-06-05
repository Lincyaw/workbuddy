package control

import (
	"context"
	"errors"
	"testing"
)

// fakeObserver returns a sequence of observations, one per call.
type fakeObserver struct {
	observations []*Observation
	callCount    int
}

func (f *fakeObserver) Observe(_ context.Context, _ string, _ int, _ string) (*Observation, error) {
	if f.callCount >= len(f.observations) {
		return nil, errors.New("fakeObserver: no more observations")
	}
	obs := f.observations[f.callCount]
	f.callCount++
	return obs, nil
}

func TestRun_CompleteOnFirstRound(t *testing.T) {
	observer := &fakeObserver{
		observations: []*Observation{
			{BranchPushed: true, PROpen: true, PRNumber: 7, LabelCurrent: "status:reviewing", IssueCommented: true},
		},
	}

	var agentCalls int
	runAgent := func(_ context.Context, prompt, resumeID string) (string, error) {
		agentCalls++
		return "session-1", nil
	}

	cfg := &LoopConfig{
		Observer:   observer,
		Controller: &Controller{MaxRounds: 5},
		Repo:       "Lincyaw/workbuddy",
		IssueNum:   42,
		Branch:     "workbuddy/issue-42",
		Objective:  DevObjective(),
	}

	result, err := Run(context.Background(), cfg, runAgent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionComplete {
		t.Fatalf("expected ActionComplete, got %d", result.Action)
	}
	if result.Rounds != 1 {
		t.Fatalf("expected 1 round, got %d", result.Rounds)
	}
	if agentCalls != 1 {
		t.Fatalf("expected 1 agent call, got %d", agentCalls)
	}
}

func TestRun_ResumesThenCompletes(t *testing.T) {
	observer := &fakeObserver{
		observations: []*Observation{
			// Round 1: branch pushed but no PR yet
			{BranchPushed: true, PROpen: false, LabelCurrent: "status:developing"},
			// Round 2: everything met
			{BranchPushed: true, PROpen: true, PRNumber: 10, LabelCurrent: "status:reviewing", IssueCommented: true},
		},
	}

	var calls []struct{ prompt, resumeID string }
	runAgent := func(_ context.Context, prompt, resumeID string) (string, error) {
		calls = append(calls, struct{ prompt, resumeID string }{prompt, resumeID})
		return "session-1", nil
	}

	cfg := &LoopConfig{
		Observer:   observer,
		Controller: &Controller{MaxRounds: 5},
		Repo:       "Lincyaw/workbuddy",
		IssueNum:   42,
		Branch:     "workbuddy/issue-42",
		Objective:  DevObjective(),
	}

	result, err := Run(context.Background(), cfg, runAgent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionComplete {
		t.Fatalf("expected ActionComplete, got %d", result.Action)
	}
	if result.Rounds != 2 {
		t.Fatalf("expected 2 rounds, got %d", result.Rounds)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 agent calls, got %d", len(calls))
	}
	// First call: fresh session (no resume ID)
	if calls[0].resumeID != "" {
		t.Fatalf("first call should have empty resumeID, got %q", calls[0].resumeID)
	}
	// Second call: resume with feedback
	if calls[1].resumeID != "session-1" {
		t.Fatalf("second call resumeID = %q, want session-1", calls[1].resumeID)
	}
	if calls[1].prompt == "" {
		t.Fatalf("second call should have a feedback prompt")
	}
}

func TestRun_BlocksOnMaxRounds(t *testing.T) {
	// Observer always returns the same incomplete state.
	neverDone := &Observation{BranchPushed: true, PROpen: false, LabelCurrent: "status:developing"}
	observer := &fakeObserver{
		observations: []*Observation{neverDone, neverDone, neverDone},
	}

	runAgent := func(_ context.Context, _, _ string) (string, error) {
		return "session-1", nil
	}

	cfg := &LoopConfig{
		Observer:   observer,
		Controller: &Controller{MaxRounds: 3},
		Repo:       "Lincyaw/workbuddy",
		IssueNum:   42,
		Branch:     "workbuddy/issue-42",
		Objective:  DevObjective(),
	}

	result, err := Run(context.Background(), cfg, runAgent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionBlock {
		t.Fatalf("expected ActionBlock, got %d", result.Action)
	}
	if result.Rounds != 3 {
		t.Fatalf("expected 3 rounds, got %d", result.Rounds)
	}
}

func TestRun_AgentError(t *testing.T) {
	runAgent := func(_ context.Context, _, _ string) (string, error) {
		return "", errors.New("agent crashed")
	}

	cfg := &LoopConfig{
		Observer:   &fakeObserver{},
		Controller: &Controller{MaxRounds: 5},
		Repo:       "Lincyaw/workbuddy",
		IssueNum:   42,
		Branch:     "workbuddy/issue-42",
		Objective:  DevObjective(),
	}

	result, err := Run(context.Background(), cfg, runAgent)
	if err == nil {
		t.Fatalf("expected error")
	}
	if result.Action != ActionBlock {
		t.Fatalf("expected ActionBlock on agent error, got %d", result.Action)
	}
}
