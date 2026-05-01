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
				"agents/dev-agent.md": fixtureAgent("dev-agent", "developing", "dev", "claude-code", "Repo: {{.Repo}}"),
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
				"agents/dev-agent.md": fixtureAgent("dev-agent", "developing", "dev", "claude-code", "Repo: {{.Repo}}"),
				"agents/wrongname.md": fixtureAgent("review-agent", "reviewing", "review", "claude-code", "Repo: {{.Repo}}"),
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
				"agents/dev-agent.md": fixtureAgent("dev-agent", "developing", "dev", "rust-agent", "Repo: {{.Repo}}"),
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
				"agents/dev-agent.md": fixtureAgent("dev-agent", "developing", "tester", "claude-code", "Repo: {{.Repo}}"),
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
				"agents/dev-agent.md": fixtureAgent("dev-agent", "developing", "dev", "claude-code", "Repo: {{.Repo}}"),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:dup", Agent: "dev-agent"},
					"reviewing":  {EnterLabel: "status:dup"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeDuplicateEnterLabel,
		},
		{
			name: "WB-X007 unknown trigger state",
			files: map[string]string{
				"config.yaml":         "repo: octo/x\n",
				"agents/dev-agent.md": fixtureAgent("dev-agent", "missingstate", "dev", "claude-code", "Repo: {{.Repo}}"),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:developing", Agent: "dev-agent"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeUnknownTriggerState,
		},
		{
			name: "WB-F002 empty prompt body",
			files: map[string]string{
				"config.yaml":         "repo: octo/x\n",
				"agents/dev-agent.md": fixtureAgent("dev-agent", "developing", "dev", "claude-code", ""),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:developing", Agent: "dev-agent"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeEmptyPromptBody,
		},
		{
			name: "WB-CT001 missing context field",
			files: map[string]string{
				"config.yaml":         "repo: octo/x\n",
				"agents/dev-agent.md": fixtureAgentNoContext("dev-agent", "developing", "dev", "claude-code", "Repo: {{.Repo}}"),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:developing", Agent: "dev-agent"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeMissingContextField,
		},
		{
			name: "WB-CT002 prompt uses field not in context",
			files: map[string]string{
				"config.yaml": "repo: octo/x\n",
				"agents/dev-agent.md": fixtureAgentWithContext(
					"dev-agent", "developing", "dev", "claude-code",
					"Issue {{.Issue.Number}} in {{.Repo}}",
					[]string{"Repo"},
				),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:developing", Agent: "dev-agent"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeContextFieldUndeclared,
		},
		{
			name: "WB-CT003 context entry never used",
			files: map[string]string{
				"config.yaml": "repo: octo/x\n",
				"agents/dev-agent.md": fixtureAgentWithContext(
					"dev-agent", "developing", "dev", "claude-code",
					"Repo: {{.Repo}}",
					[]string{"Repo", "Issue.Title"},
				),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:developing", Agent: "dev-agent"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeContextFieldUnused,
		},
		{
			name: "WB-T001 prompt parse error",
			files: map[string]string{
				"config.yaml":         "repo: octo/x\n",
				"agents/dev-agent.md": fixtureAgent("dev-agent", "developing", "dev", "claude-code", "{{.Issue"),
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
				"agents/dev-agent.md": fixtureAgent("dev-agent", "developing", "dev", "claude-code", "Hi {{.Bogus.Field}}"),
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
				"agents/dev-agent.md": fixtureAgentWithTimeout("dev-agent", "developing", "dev", "claude-code", "Repo: {{.Repo}}", "10m"),
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
				"agents/dev-agent.md": fixtureAgentWithTimeout("dev-agent", "developing", "dev", "claude-code", "Repo: {{.Repo}}", "60h"),
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
				"agents/dev-agent.md": fixtureAgent("dev-agent", "developing", "dev", "claude-code", "Repo: {{.Repo}}"),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:developing", Agent: "dev-agent", Transitions: []string{"finished"}},
					"finished":   {EnterLabel: "status:finished", Agent: "dev-agent"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeAgentInTerminalState,
		},
		{
			name: "WB-S005 no worker advertises agent runtime",
			files: map[string]string{
				"config.yaml":         "repo: octo/x\n",
				"agents/dev-agent.md": fixtureAgent("dev-agent", "developing", "dev", "codex", "Repo: {{.Repo}}"),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:developing", Agent: "dev-agent", Transitions: []string{"reviewing"}},
					"reviewing":  {EnterLabel: "status:reviewing"},
				}),
			},
			opts: Options{
				SkipRuntimeBinaryCheck: true,
				CheckWorkerRuntimes:    true,
				WorkerRuntimes:         []string{"claude-code"},
			},
			wantHas: CodeRuntimeNoWorkerSupport,
		},
		{
			name: "WB-L001 prompt body inlines gh issue edit",
			files: map[string]string{
				"config.yaml": "repo: octo/x\n",
				"agents/dev-agent.md": fixtureAgent(
					"dev-agent", "developing", "dev", "claude-code",
					"Repo: {{.Repo}}\nWhen finished, run `gh issue edit 1 --add-label foo`.",
				),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:developing", Agent: "dev-agent", Transitions: []string{"reviewing"}},
					"reviewing":  {EnterLabel: "status:reviewing"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeAgentPromptInlinesGhEdit,
		},
		{
			name: "WB-L002 prompt body inlines status: label",
			files: map[string]string{
				"config.yaml": "repo: octo/x\n",
				"agents/dev-agent.md": fixtureAgent(
					"dev-agent", "developing", "dev", "claude-code",
					"Repo: {{.Repo}}\nIf the criterion fails, set status:blocked and stop.",
				),
				"workflows/default.md": fixtureWorkflow(map[string]workflowState{
					"developing": {EnterLabel: "status:developing", Agent: "dev-agent", Transitions: []string{"reviewing"}},
					"reviewing":  {EnterLabel: "status:reviewing"},
				}),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeAgentPromptInlinesStatusLabel,
		},
		{
			name: "WB-L003 workflow prose contains template expression",
			files: map[string]string{
				"config.yaml":         "repo: octo/x\n",
				"agents/dev-agent.md": fixtureAgent("dev-agent", "developing", "dev", "claude-code", "Repo: {{.Repo}}"),
				"workflows/default.md": fixtureWorkflowWithProse(
					map[string]workflowState{
						"developing": {EnterLabel: "status:developing", Agent: "dev-agent", Transitions: []string{"reviewing"}},
						"reviewing":  {EnterLabel: "status:reviewing"},
					},
					"\nIssue {{.Issue.Number}} flows through this graph.\n",
				),
			},
			opts:    Options{SkipRuntimeBinaryCheck: true},
			wantHas: CodeWorkflowProseTemplateExpr,
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
		"agents/dev-agent.md":    fixtureAgentWithTimeout("dev-agent", "developing", "dev", "claude-code", "Repo: {{.Repo}}", "30m"),
		"agents/review-agent.md": fixtureAgentWithTimeout("review-agent", "reviewing", "review", "claude-code", "Repo: {{.Repo}}", "30m"),
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
	EnterLabel string
	Agent      string
	// Transitions is keyed by target-state-name; the entry value is the
	// label that drives the transition. We invert at render time so the
	// emitted YAML matches the new map form (label → target).
	Transitions []string
}

// fixtureAgent emits a new-format agent file: triggers use `state:`, the
// markdown body holds the prompt template, and `context:` is auto-derived
// from the dotted field references in the prompt body. `triggerState` is the
// workflow state name (NOT a label).
func fixtureAgent(name, triggerState, role, runtime, prompt string) string {
	return fixtureAgentWithTimeout(name, triggerState, role, runtime, prompt, "")
}

// fixtureAgentWithTimeout is fixtureAgent + a policy.timeout. Same body-as-
// prompt shape; context list is auto-derived.
func fixtureAgentWithTimeout(name, triggerState, role, runtime, prompt, timeout string) string {
	context := autoContextFromPrompt(prompt)
	return buildAgentFixture(name, triggerState, role, runtime, prompt, timeout, context, false)
}

// fixtureAgentWithContext lets a test pin the `context:` list explicitly so
// the WB-CT002/WB-CT003 cases can express the diff under test.
func fixtureAgentWithContext(name, triggerState, role, runtime, prompt string, context []string) string {
	return buildAgentFixture(name, triggerState, role, runtime, prompt, "", context, false)
}

// fixtureAgentNoContext emits an otherwise-valid agent that omits `context:`
// entirely so the WB-CT001 case can fire.
func fixtureAgentNoContext(name, triggerState, role, runtime, prompt string) string {
	return buildAgentFixture(name, triggerState, role, runtime, prompt, "", nil, true)
}


func buildAgentFixture(name, triggerState, role, runtime, prompt, timeout string, context []string, omitContext bool) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + name + "\n")
	b.WriteString("description: test agent\n")
	b.WriteString("triggers:\n")
	b.WriteString("  - state: " + triggerState + "\n")
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
	if !omitContext && len(context) > 0 {
		b.WriteString("context:\n")
		for _, c := range context {
			b.WriteString("  - " + c + "\n")
		}
	}
	b.WriteString("---\n")
	if prompt != "" {
		b.WriteString(prompt + "\n")
	}
	return b.String()
}

// autoContextFromPrompt extracts the unique top-level dotted-field roots from
// a prompt body, e.g. "Repo: {{.Repo}}, Title: {{.Issue.Title}}" → ["Repo",
// "Issue.Title"]. Only used by helper fixtures so they satisfy WB-CT002 by
// default; tests that need to exercise the diff use fixtureAgentWithContext.
func autoContextFromPrompt(prompt string) []string {
	if strings.TrimSpace(prompt) == "" {
		return []string{"Repo"}
	}
	seen := map[string]struct{}{}
	var out []string
	i := 0
	for i < len(prompt) {
		idx := strings.Index(prompt[i:], "{{.")
		if idx < 0 {
			break
		}
		j := i + idx + 3 // skip "{{."
		end := j
		for end < len(prompt) {
			c := prompt[end]
			if c == '}' || c == ' ' || c == '\t' || c == '\n' || c == '|' {
				break
			}
			end++
		}
		path := prompt[j:end]
		if path != "" {
			if _, ok := seen[path]; !ok {
				seen[path] = struct{}{}
				out = append(out, path)
			}
		}
		i = end
	}
	if len(out) == 0 {
		out = []string{"Repo"}
	}
	return out
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

// fixtureOrderedWorkflow emits a workflow markdown using the new transitions
// map form. Each entry in `Transitions` is a target state name; the test
// helper synthesises the driving label as `status:<target>` so the tests
// remain compact.
func fixtureOrderedWorkflow(states []orderedState) string {
	return fixtureOrderedWorkflowWithTrailing(states, "")
}

// fixtureWorkflowWithProse appends `trailing` text after the closing fence
// of the states YAML block — used by lint cases (WB-L003) that need to put
// arbitrary prose into the workflow body.
func fixtureWorkflowWithProse(states map[string]workflowState, trailing string) string {
	names := make([]string, 0, len(states))
	for n := range states {
		names = append(names, n)
	}
	sort.Strings(names)
	ordered := make([]orderedState, 0, len(states))
	for _, n := range names {
		ordered = append(ordered, orderedState{Name: n, State: states[n]})
	}
	return fixtureOrderedWorkflowWithTrailing(ordered, trailing)
}

func fixtureOrderedWorkflowWithTrailing(states []orderedState, trailing string) string {
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
			for _, target := range s.Transitions {
				label := "status:" + target
				b.WriteString("      \"" + label + "\": " + target + "\n")
			}
		}
	}
	b.WriteString("```\n")
	if trailing != "" {
		b.WriteString(trailing)
	}
	return b.String()
}

func staleConfig(idleThreshold string) string {
	return "repo: octo/x\nworker:\n  stale_inference:\n    idle_threshold: " + idleThreshold + "\n"
}
