package validate

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestValidateDir_CodeFixtures runs a fixture-based table covering one
// "bad" case per diagnostic code introduced in batch 1 (issue #204).
// Each case writes a minimal config dir with the offending input and
// asserts that exactly one diagnostic with the expected code is raised.
func TestValidateDir_CodeFixtures(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		opts    Options
		wantHas string // diagnostic code that must appear
		wantNot []string
	}{
		{
			name: "WB-X001 unknown agent reference",
			files: map[string]string{
				"config.yaml":         "repo: octo/x\n",
				"agents/dev-agent.md": fixtureAgent("dev-agent", "status:developing", "dev", "claude-code", ""),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:developing", Agent: "missing-agent"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeUnknownAgent,
		},
		{
			name: "WB-X002 basename mismatch",
			files: map[string]string{
				"config.yaml":         "repo: octo/x\n",
				"agents/dev-agent.md": fixtureAgent("dev-agent", "status:developing", "dev", "claude-code", ""),
				"agents/wrongname.md": fixtureAgent("review-agent", "status:reviewing", "review", "claude-code", ""),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:developing", Agent: "dev-agent"},
					"reviewing":  {EnterLabel: "status:reviewing", Agent: "review-agent"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeAgentBasenameMismatch,
		},
		{
			name: "WB-X003 unknown runtime",
			files: map[string]string{
				"config.yaml":         "repo: octo/x\n",
				"agents/dev-agent.md": fixtureAgent("dev-agent", "status:developing", "dev", "rust-agent", ""),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:developing", Agent: "dev-agent"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeUnknownRuntime,
		},
		{
			name: "WB-X004 unknown role",
			files: map[string]string{
				"config.yaml":         "repo: octo/x\n",
				"agents/dev-agent.md": fixtureAgent("dev-agent", "status:developing", "tester", "claude-code", ""),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:developing", Agent: "dev-agent"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeUnknownRole,
		},
		{
			name: "WB-X005 duplicate enter_label",
			files: map[string]string{
				"config.yaml":         "repo: octo/x\n",
				"agents/dev-agent.md": fixtureAgent("dev-agent", "status:developing", "dev", "claude-code", ""),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:dup", Agent: "dev-agent"},
					"reviewing":  {EnterLabel: "status:dup"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeDuplicateEnterLabel,
		},
		{
			name: "WB-X006 unbound trigger label",
			files: map[string]string{
				"config.yaml":         "repo: octo/x\n",
				"agents/dev-agent.md": fixtureAgent("dev-agent", "status:nowhere", "dev", "claude-code", ""),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:developing", Agent: "dev-agent"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeUnboundTriggerLabel,
		},
		{
			name: "WB-T001 prompt parse error",
			files: map[string]string{
				"config.yaml":         "repo: octo/x\n",
				"agents/dev-agent.md": fixtureAgent("dev-agent", "status:developing", "dev", "claude-code", "{{.Issue"),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:developing", Agent: "dev-agent"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodePromptParseError,
		},
		{
			name: "WB-T101 unknown template field",
			files: map[string]string{
				"config.yaml":         "repo: octo/x\n",
				"agents/dev-agent.md": fixtureAgent("dev-agent", "status:developing", "dev", "claude-code", "Hi {{.Bogus.Field}}"),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:developing", Agent: "dev-agent"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeUnknownTemplateField,
		},
		{
			name: "WB-S001 timeout below stale threshold",
			files: map[string]string{
				"config.yaml":         staleConfig("30m"),
				"agents/dev-agent.md": fixtureAgentWithTimeout("dev-agent", "status:developing", "dev", "claude-code", "", "10m"),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:developing", Agent: "dev-agent"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeTimeoutBelowStaleThreshold,
		},
		{
			name: "WB-S002 suspiciously large timeout",
			files: map[string]string{
				"config.yaml":         staleConfig("1m"),
				"agents/dev-agent.md": fixtureAgentWithTimeout("dev-agent", "status:developing", "dev", "claude-code", "", "60h"),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:developing", Agent: "dev-agent"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeTimeoutSuspiciouslyLarge,
		},
		{
			name: "WB-S004 agent in terminal state",
			files: map[string]string{
				"config.yaml":         "repo: octo/x\n",
				"agents/dev-agent.md": fixtureAgent("dev-agent", "status:developing", "dev", "claude-code", ""),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:developing", Agent: "dev-agent", Transitions: []string{"finished"}},
					"finished":   {EnterLabel: "status:finished", Agent: "dev-agent"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeAgentInTerminalState,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFixtures(t, tc.files)
			diags, err := ValidateDirWithOptions(dir, tc.opts)
			if err != nil {
				t.Fatalf("ValidateDirWithOptions: %v", err)
			}
			found := false
			for _, d := range diags {
				if d.Code == tc.wantHas {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected diagnostic with code %q, got: %s", tc.wantHas, diagSummary(diags))
			}
			for _, banned := range tc.wantNot {
				for _, d := range diags {
					if d.Code == banned {
						t.Errorf("did not expect code %q, but got: %s", banned, diagSummary(diags))
					}
				}
			}
		})
	}
}

// TestValidateDir_CleanFixtureNoDiagnostics is the positive case: a
// minimal but well-formed config produces zero diagnostics.
func TestValidateDir_CleanFixtureNoDiagnostics(t *testing.T) {
	files := map[string]string{
		"config.yaml":            staleConfig("10m"),
		"agents/dev-agent.md":    fixtureAgentWithTimeout("dev-agent", "status:developing", "dev", "claude-code", "Repo: {{.Repo}}", "30m"),
		"agents/review-agent.md": fixtureAgentWithTimeout("review-agent", "status:reviewing", "review", "claude-code", "Repo: {{.Repo}}", "30m"),
		"workflows/default.md": fixtureOrderedWorkflow([]orderedState{
			{Name: "developing", State: workflowState{EnterLabel: "status:developing", Agent: "dev-agent", Transitions: []string{"reviewing"}}},
			{Name: "reviewing", State: workflowState{EnterLabel: "status:reviewing", Agent: "review-agent", Transitions: []string{"done"}}},
			{Name: "done", State: workflowState{EnterLabel: "status:done"}},
		}),
	}
	dir := writeFixtures(t, files)
	diags, err := ValidateDirWithOptions(dir, Options{SkipRuntimeBinaryCheck: true})
	if err != nil {
		t.Fatalf("ValidateDirWithOptions: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("expected no diagnostics, got: %s", diagSummary(diags))
	}
}

func writeFixtures(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".github", "workbuddy")
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return dir
}

func diagSummary(diags []Diagnostic) string {
	if len(diags) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(diags))
	for _, d := range diags {
		parts = append(parts, d.String())
	}
	return strings.Join(parts, "\n  ")
}

type workflowState struct {
	EnterLabel  string
	Agent       string
	Transitions []string
}

func fixtureAgent(name, label, role, runtime, prompt string) string {
	return fixtureAgentWithTimeout(name, label, role, runtime, prompt, "")
}

func fixtureAgentWithTimeout(name, label, role, runtime, prompt, timeout string) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + name + "\n")
	b.WriteString("description: test agent\n")
	b.WriteString("triggers:\n")
	b.WriteString("  - label: \"" + label + "\"\n")
	b.WriteString("    event: labeled\n")
	if role != "" {
		b.WriteString("role: " + role + "\n")
	}
	if runtime != "" {
		b.WriteString("runtime: " + runtime + "\n")
	}
	if timeout != "" {
		b.WriteString("policy:\n")
		b.WriteString("  timeout: " + timeout + "\n")
	}
	if prompt != "" {
		b.WriteString("prompt: |\n")
		for _, ln := range strings.Split(prompt, "\n") {
			b.WriteString("  " + ln + "\n")
		}
	} else {
		b.WriteString("command: echo run\n")
	}
	b.WriteString("---\n## Agent\n")
	return b.String()
}

func fixtureWorkflow(states map[string]workflowState) string {
	// Sort by name to keep tests independent of map iteration order.
	names := make([]string, 0, len(states))
	for n := range states {
		names = append(names, n)
	}
	sort.Strings(names)
	ordered := make([]orderedState, 0, len(states))
	for _, n := range names {
		ordered = append(ordered, orderedState{Name: n, State: states[n]})
	}
	return fixtureOrderedWorkflow(ordered)
}

type orderedState struct {
	Name  string
	State workflowState
}

func fixtureOrderedWorkflow(states []orderedState) string {
	var b strings.Builder
	b.WriteString("---\nname: default\ndescription: t\ntrigger:\n  issue_label: \"workbuddy\"\n---\n## W\n\n```yaml\nstates:\n")
	for _, item := range states {
		s := item.State
		b.WriteString("  " + item.Name + ":\n")
		b.WriteString("    enter_label: \"" + s.EnterLabel + "\"\n")
		if s.Agent != "" {
			b.WriteString("    agent: " + s.Agent + "\n")
		}
		if len(s.Transitions) > 0 {
			b.WriteString("    transitions:\n")
			for _, to := range s.Transitions {
				b.WriteString("      - to: " + to + "\n")
			}
		}
	}
	b.WriteString("```\n")
	return b.String()
}

func staleConfig(idleThreshold string) string {
	return "repo: octo/x\nworker:\n  stale_inference:\n    idle_threshold: " + idleThreshold + "\n"
}
