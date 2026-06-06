package workerclient

import (
	"encoding/json"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/config"
)

// TestTaskAgentRoundTrip verifies the per-repo Agent config travels over the
// wire DTO cleanly in both directions (ADR 2026-06-06 §2). This is the
// contract the coordinator dispatch side and the worker consume side rely on.
func TestTaskAgentRoundTrip(t *testing.T) {
	original := Task{
		TaskID:    "t-1",
		Repo:      "owner/repo",
		IssueNum:  42,
		AgentName: "dev",
		Agent: &config.AgentConfig{
			Name:              "dev",
			Runtime:           "agentm",
			DevContainerImage: "owner/dev:9",
			// Prompt is the agent's instructions and the most load-bearing
			// field on the wire. It has no json tag (no json:"-"), so it must
			// survive the round-trip; if it ever silently drops, dispatched
			// agents run with empty instructions.
			Prompt: "You are the dev agent. Implement the issue.",
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Task
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Agent == nil {
		t.Fatal("expected Agent to survive round-trip, got nil")
	}
	if got.Agent.Name != "dev" || got.Agent.Runtime != "agentm" || got.Agent.DevContainerImage != "owner/dev:9" {
		t.Fatalf("agent config not preserved: %+v", got.Agent)
	}
	if got.Agent.Prompt != "You are the dev agent. Implement the issue." {
		t.Fatalf("agent Prompt did not survive the wire round-trip: %q", got.Agent.Prompt)
	}
}

// TestTaskAgentOmittedWhenNil verifies the wire stays backward-compatible:
// a nil Agent is omitted from JSON so older coordinators/workers are unaffected.
func TestTaskAgentOmittedWhenNil(t *testing.T) {
	data, err := json.Marshal(Task{TaskID: "t-2", AgentName: "dev"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(data, &asMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := asMap["agent"]; present {
		t.Fatalf("expected agent field omitted when nil, got %s", data)
	}
}
