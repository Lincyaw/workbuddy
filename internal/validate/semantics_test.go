package validate

import (
	"strings"
	"testing"
	"time"
)

func TestValidateAgentSemantics_TimeoutBelowStaleThreshold(t *testing.T) {
	agent := &agentDoc{
		Path:              "/tmp/agents/dev-agent.md",
		Name:              "dev-agent",
		PolicyTimeout:     15 * time.Minute,
		PolicyTimeoutLine: 12,
	}
	got := validateAgentSemantics(agent, 30*time.Minute, semanticsOptions{SkipRuntimeBinaryCheck: true})
	requireOneCode(t, got, CodeTimeoutBelowStaleThreshold)
	if got[0].EffectiveSeverity() != SeverityError {
		t.Errorf("severity = %q, want error", got[0].EffectiveSeverity())
	}
}

func TestValidateAgentSemantics_TimeoutEqualStaleOK(t *testing.T) {
	agent := &agentDoc{
		Path:          "/tmp/agents/dev-agent.md",
		Name:          "dev-agent",
		PolicyTimeout: 30 * time.Minute,
	}
	got := validateAgentSemantics(agent, 30*time.Minute, semanticsOptions{SkipRuntimeBinaryCheck: true})
	if len(got) != 0 {
		t.Fatalf("equal timeout should not flag: %+v", got)
	}
}

func TestValidateAgentSemantics_SuspiciouslyLargeTimeout(t *testing.T) {
	agent := &agentDoc{
		Path:              "/tmp/agents/dev-agent.md",
		Name:              "dev-agent",
		PolicyTimeout:     60 * time.Hour,
		PolicyTimeoutLine: 12,
	}
	got := validateAgentSemantics(agent, 0, semanticsOptions{SkipRuntimeBinaryCheck: true})
	requireOneCode(t, got, CodeTimeoutSuspiciouslyLarge)
	if got[0].EffectiveSeverity() != SeverityWarning {
		t.Errorf("severity = %q, want warning", got[0].EffectiveSeverity())
	}
}

func TestValidateAgentSemantics_NormalTimeoutOK(t *testing.T) {
	agent := &agentDoc{
		Path:          "/tmp/agents/dev-agent.md",
		Name:          "dev-agent",
		PolicyTimeout: 60 * time.Minute,
	}
	got := validateAgentSemantics(agent, 30*time.Minute, semanticsOptions{SkipRuntimeBinaryCheck: true})
	if len(got) != 0 {
		t.Fatalf("normal timeout should not flag: %+v", got)
	}
}

func TestValidateAgentSemantics_RuntimeBinaryMissing(t *testing.T) {
	agent := &agentDoc{
		Path:        "/tmp/agents/dev-agent.md",
		Name:        "dev-agent",
		Runtime:     "claude-code",
		RuntimeLine: 8,
	}
	// Force the missing case by using an unrecognized fake runtime
	// pointing at a binary nobody has.
	mu := patchRuntimeBinaries(t, map[string]string{"claude-code": "wb-test-nonexistent-binary-xyz"})
	defer mu()

	got := validateAgentSemantics(agent, 0, semanticsOptions{})
	requireOneCode(t, got, CodeRuntimeBinaryMissing)
	if got[0].EffectiveSeverity() != SeverityWarning {
		t.Errorf("severity = %q, want warning", got[0].EffectiveSeverity())
	}
	if !strings.Contains(got[0].Message, "wb-test-nonexistent-binary-xyz") {
		t.Errorf("message missing binary name: %q", got[0].Message)
	}
}

func TestValidateAgentSemantics_RuntimeBinaryCheckSkipped(t *testing.T) {
	agent := &agentDoc{
		Path:    "/tmp/agents/dev-agent.md",
		Name:    "dev-agent",
		Runtime: "claude-code",
	}
	mu := patchRuntimeBinaries(t, map[string]string{"claude-code": "wb-test-nonexistent-binary-xyz"})
	defer mu()

	got := validateAgentSemantics(agent, 0, semanticsOptions{SkipRuntimeBinaryCheck: true})
	if len(got) != 0 {
		t.Fatalf("--no-runtime-check should suppress WB-S003: %+v", got)
	}
}

func TestValidateSemantics_AgentInTerminalState(t *testing.T) {
	agents := map[string]*agentDoc{
		"dev-agent": {Path: "/tmp/agents/dev-agent.md", Name: "dev-agent"},
	}
	wf := &workflowDoc{
		Path:       "/tmp/workflows/x.md",
		Name:       "x",
		StateOrder: []string{"running", "done"},
		States: map[string]*stateDoc{
			"running": {Name: "running", EnterLabel: "status:running", Agent: "dev-agent", AgentLine: 10, Transitions: []transitionDoc{{Label: "status:done", To: "done"}}},
			"done":    {Name: "done", EnterLabel: "status:done", Agent: "dev-agent", AgentLine: 16},
		},
	}
	got := validateSemantics("/tmp/configdir-does-not-exist", agents, []*workflowDoc{wf}, semanticsOptions{SkipRuntimeBinaryCheck: true})
	// Filter for WB-S004 only (agents map may produce semantic
	// diagnostics depending on test environment).
	var s004 []Diagnostic
	for _, d := range got {
		if d.Code == CodeAgentInTerminalState {
			s004 = append(s004, d)
		}
	}
	if len(s004) != 1 {
		t.Fatalf("expected exactly one WB-S004, got %d (all=%+v)", len(s004), got)
	}
	if s004[0].Line != 16 {
		t.Errorf("expected line 16 (the terminal state), got %d", s004[0].Line)
	}
}

// patchRuntimeBinaries swaps the package-level runtimeBinaries map for
// the duration of one test. Returns a restore func.
func patchRuntimeBinaries(t *testing.T, replacement map[string]string) func() {
	t.Helper()
	saved := runtimeBinaries
	runtimeBinaries = replacement
	return func() {
		runtimeBinaries = saved
	}
}
