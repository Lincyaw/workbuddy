package validate

import (
	"strings"
	"testing"
)

func TestValidateAgentCrossRefs_BasenameMismatch(t *testing.T) {
	agent := &agentDoc{
		Path:     "/tmp/agents/dev-agent.md",
		Name:     "dev",
		NameLine: 2,
	}
	got := validateAgentCrossRefs(agent)
	requireOneCode(t, got, CodeAgentBasenameMismatch)
	if got[0].EffectiveSeverity() != SeverityWarning {
		t.Errorf("severity = %q, want warning", got[0].EffectiveSeverity())
	}
}

func TestValidateAgentCrossRefs_BasenameMatchOK(t *testing.T) {
	agent := &agentDoc{
		Path: "/tmp/agents/dev-agent.md",
		Name: "dev-agent",
	}
	if got := validateAgentCrossRefs(agent); len(got) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", got)
	}
}

func TestValidateAgentCrossRefs_UnknownRuntime(t *testing.T) {
	agent := &agentDoc{
		Path:        "/tmp/agents/dev-agent.md",
		Name:        "dev-agent",
		Runtime:     "wrongruntime",
		RuntimeLine: 5,
	}
	got := validateAgentCrossRefs(agent)
	requireOneCode(t, got, CodeUnknownRuntime)
	if got[0].EffectiveSeverity() != SeverityError {
		t.Errorf("severity = %q, want error", got[0].EffectiveSeverity())
	}
	if !strings.Contains(got[0].Message, "wrongruntime") {
		t.Errorf("message missing offender: %q", got[0].Message)
	}
}

func TestValidateAgentCrossRefs_KnownRuntimes(t *testing.T) {
	for _, rt := range []string{"codex", "claude-code", "claude-agent-sdk"} {
		agent := &agentDoc{
			Path:    "/tmp/agents/dev-agent.md",
			Name:    "dev-agent",
			Runtime: rt,
		}
		if got := validateAgentCrossRefs(agent); len(got) != 0 {
			t.Errorf("runtime %q produced diagnostics: %+v", rt, got)
		}
	}
}

func TestValidateAgentCrossRefs_UnknownRole(t *testing.T) {
	agent := &agentDoc{
		Path: "/tmp/agents/dev-agent.md",
		Name: "dev-agent",
		Role: "tester",
	}
	got := validateAgentCrossRefs(agent)
	requireOneCode(t, got, CodeUnknownRole)
}

func TestValidateAgentCrossRefs_KnownRoles(t *testing.T) {
	for _, role := range []string{"dev", "review"} {
		agent := &agentDoc{
			Path: "/tmp/agents/dev-agent.md",
			Name: "dev-agent",
			Role: role,
		}
		if got := validateAgentCrossRefs(agent); len(got) != 0 {
			t.Errorf("role %q produced diagnostics: %+v", role, got)
		}
	}
}

func TestValidateEnterLabelUniqueness(t *testing.T) {
	wf := &workflowDoc{
		Path:       "/tmp/workflows/x.md",
		Name:       "x",
		StateOrder: []string{"a", "b", "c"},
		States: map[string]*stateDoc{
			"a": {Name: "a", EnterLabel: "status:foo", EnterLabelLine: 10},
			"b": {Name: "b", EnterLabel: "status:bar", EnterLabelLine: 14},
			"c": {Name: "c", EnterLabel: "status:foo", EnterLabelLine: 18},
		},
	}
	got := validateEnterLabelUniqueness(wf)
	requireOneCode(t, got, CodeDuplicateEnterLabel)
	if got[0].Line != 18 {
		t.Errorf("expected diagnostic on line 18 (the duplicate), got %d", got[0].Line)
	}
}

func TestValidateEnterLabelUniqueness_AllUniqueOK(t *testing.T) {
	wf := &workflowDoc{
		Path:       "/tmp/workflows/x.md",
		Name:       "x",
		StateOrder: []string{"a", "b"},
		States: map[string]*stateDoc{
			"a": {Name: "a", EnterLabel: "status:foo"},
			"b": {Name: "b", EnterLabel: "status:bar"},
		},
	}
	if got := validateEnterLabelUniqueness(wf); len(got) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", got)
	}
}

func requireOneCode(t *testing.T, diags []Diagnostic, code string) {
	t.Helper()
	if len(diags) != 1 {
		t.Fatalf("expected exactly one diagnostic with code %q, got %d: %+v", code, len(diags), diags)
	}
	if diags[0].Code != code {
		t.Fatalf("diagnostic code = %q, want %q (msg=%q)", diags[0].Code, code, diags[0].Message)
	}
}
