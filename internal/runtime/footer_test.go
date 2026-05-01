package runtime

import (
	"strings"
	"testing"
	"text/template"
)

// TestBuildTransitionFooter_Developing pins the exact rendered footer for the
// canonical `developing` state in the default workflow. This is the snapshot
// that callers (skill docs, agent prompts) refer to when reasoning about what
// the agent actually sees.
func TestBuildTransitionFooter_Developing(t *testing.T) {
	transitions := map[string]string{
		"status:reviewing": "reviewing",
		"status:blocked":   "blocked",
	}
	got := BuildTransitionFooter("developing", "status:developing", transitions)

	want := "---\n\n" +
		"## Transition\n\n" +
		"When you finish, transition this issue by editing labels.\n\n" +
		"| Add this label | Resulting state |\n" +
		"|----------------|-----------------|\n" +
		"| `status:blocked` | `blocked` |\n" +
		"| `status:reviewing` | `reviewing` |\n" +
		"\nRun:\n\n" +
		"```\n" +
		"gh issue edit {{.Issue.Number}} --repo {{.Repo}} --remove-label \"status:developing\" --add-label \"<chosen-label>\"\n" +
		"```\n\n" +
		"The label you remove is `status:developing` (the enter_label of the `developing` state). " +
		"Pick the `<chosen-label>` from the table above that matches your outcome.\n"

	if got != want {
		t.Fatalf("footer mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestBuildTransitionFooter_Terminal — terminal states (no transitions) emit
// the empty string so the dispatch path appends nothing.
func TestBuildTransitionFooter_Terminal(t *testing.T) {
	if got := BuildTransitionFooter("done", "status:done", nil); got != "" {
		t.Fatalf("terminal footer should be empty, got %q", got)
	}
	if got := BuildTransitionFooter("done", "status:done", map[string]string{}); got != "" {
		t.Fatalf("terminal footer (empty map) should be empty, got %q", got)
	}
}

// TestBuildTransitionFooter_DeterministicOrder — the table rows must be
// sorted by label so the rendered string is byte-stable for diff-style tests.
func TestBuildTransitionFooter_DeterministicOrder(t *testing.T) {
	transitions := map[string]string{
		"status:zzz": "z",
		"status:aaa": "a",
		"status:mmm": "m",
	}
	got := BuildTransitionFooter("s", "status:s", transitions)
	idxA := strings.Index(got, "status:aaa")
	idxM := strings.Index(got, "status:mmm")
	idxZ := strings.Index(got, "status:zzz")
	if !(idxA >= 0 && idxA < idxM && idxM < idxZ) {
		t.Fatalf("expected aaa < mmm < zzz in rendered footer, got order:\n%s", got)
	}
}

// TestAssembleAgentPrompt_RenderableTemplate ensures the assembled body+footer
// parses + executes as a single Go text/template against TaskContext, with
// {{.Issue.Number}} / {{.Repo}} resolving correctly.
func TestAssembleAgentPrompt_RenderableTemplate(t *testing.T) {
	body := "You are dev-agent for repo {{.Repo}}, issue #{{.Issue.Number}}."
	task := &TaskContext{
		Repo:  "octo/example",
		Issue: IssueContext{Number: 42},
	}
	task.SetWorkflowState("developing", "status:developing", map[string]string{
		"status:reviewing": "reviewing",
		"status:blocked":   "blocked",
	})

	combined := AssembleAgentPrompt(body, task)
	tmpl, err := template.New("t").Funcs(TemplateFuncMap()).Parse(combined)
	if err != nil {
		t.Fatalf("combined prompt failed to parse: %v", err)
	}
	var sb strings.Builder
	if err := tmpl.Execute(&sb, task); err != nil {
		t.Fatalf("combined prompt failed to execute: %v", err)
	}
	rendered := sb.String()

	for _, want := range []string{
		"octo/example",
		"issue #42",
		"## Transition",
		"`status:reviewing` | `reviewing`",
		"`status:blocked` | `blocked`",
		"gh issue edit 42 --repo octo/example --remove-label \"status:developing\"",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered prompt missing %q:\n%s", want, rendered)
		}
	}
}

// TestAssembleAgentPrompt_NoStateMetadata — a TaskContext with no workflow
// state attached returns the body unchanged (back-compat for callers that
// have not been migrated to thread state metadata).
func TestAssembleAgentPrompt_NoStateMetadata(t *testing.T) {
	body := "Hello {{.Repo}}"
	task := &TaskContext{Repo: "octo/x"}
	combined := AssembleAgentPrompt(body, task)
	if combined != strings.TrimRight(body, "\n") {
		t.Fatalf("expected unchanged body for state-less task, got: %q", combined)
	}
}

// TestRenderAgentPrompt_E2E — exercise the rendering convenience wrapper end
// to end so dispatch sites can rely on a single call.
func TestRenderAgentPrompt_E2E(t *testing.T) {
	body := "Repo: {{.Repo}}\nIssue: #{{.Issue.Number}}"
	task := &TaskContext{Repo: "octo/x", Issue: IssueContext{Number: 7}}
	task.SetWorkflowState("developing", "status:developing", map[string]string{
		"status:reviewing": "reviewing",
	})
	got, err := RenderAgentPrompt(body, task)
	if err != nil {
		t.Fatalf("RenderAgentPrompt: %v", err)
	}
	if !strings.Contains(got, "Repo: octo/x") {
		t.Fatalf("rendered prompt missing repo line:\n%s", got)
	}
	if !strings.Contains(got, "## Transition") {
		t.Fatalf("rendered prompt missing transition footer:\n%s", got)
	}
	if !strings.Contains(got, "gh issue edit 7 --repo octo/x") {
		t.Fatalf("rendered prompt missing rendered gh command:\n%s", got)
	}
}

func TestRenderAgentPrompt_DevelopingRolloutShowsSynthTransition(t *testing.T) {
	body := "Repo: {{.Repo}}"
	task := &TaskContext{
		Repo:    "octo/x",
		Issue:   IssueContext{Number: 7},
		Rollout: RolloutContext{Index: 1, Total: 3, GroupID: "g"},
	}
	task.SetWorkflowState("developing", "status:developing", map[string]string{
		"status:synthesizing": "synthesizing",
		"status:reviewing":    "reviewing",
		"status:blocked":      "blocked",
	})
	got, err := RenderAgentPrompt(body, task)
	if err != nil {
		t.Fatalf("RenderAgentPrompt: %v", err)
	}
	if !strings.Contains(got, "`status:synthesizing` | `synthesizing`") {
		t.Fatalf("expected synth transition in rollout prompt:\n%s", got)
	}
	if strings.Contains(got, "`status:reviewing` | `reviewing`") {
		t.Fatalf("unexpected review transition in rollout prompt:\n%s", got)
	}
}

func TestAssembleAgentPrompt_PrependsRolloutPreludeOnce(t *testing.T) {
	body := "Repo: {{.Repo}}"
	task := &TaskContext{
		Repo:    "octo/x",
		Rollout: RolloutContext{Index: 2, Total: 3, GroupID: "g-1"},
	}
	got := AssembleAgentPrompt(body, task)
	want := "You are rollout 2 of 3 independent attempts at this issue."
	if !strings.HasPrefix(got, want) {
		t.Fatalf("prompt missing rollout prelude prefix:\n%s", got)
	}
	if strings.Count(got, want) != 1 {
		t.Fatalf("expected rollout prelude once, got %d copies", strings.Count(got, want))
	}
}
