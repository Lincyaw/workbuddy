package runtime

import (
	"strings"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/config"
)

func TestResolvePromptBody_UsesBuiltinSynthesisPrompt(t *testing.T) {
	agent := &config.AgentConfig{Prompt: "plain review prompt"}
	task := &TaskContext{
		Repo:  "owner/repo",
		Issue: IssueContext{Number: 293, Title: "Synthesize", Body: "issue body"},
		Synthesis: &SynthesisContext{
			MinSuccesses: 2,
			Candidates: []SynthesisCandidate{{
				RolloutIndex: 1,
				PullRequest: PRSummary{
					Number:      11,
					Title:       "rollout pr",
					HeadRefName: "workbuddy/issue-293/rollout-1",
					HeadSHA:     "abc123",
					URL:         "https://example/pr/11",
				},
				SessionSummary: "did the thing",
				SessionURL:     "/sessions/sess-1",
				Diff:           "diff --git a/x b/x\n",
			}},
		},
	}
	task.SetWorkflowStateMode(config.StateModeSynth)

	got := ResolvePromptBody(agent, task)
	for _, want := range []string{
		"issue body",
		"min_successes=2",
		"workbuddy/issue-293/rollout-1",
		"/sessions/sess-1",
		"diff --git a/x b/x",
		`{"outcome":"pick|cherry-pick|escalate"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("builtin synth prompt missing %q:\n%s", want, got)
		}
	}
}

func TestParseSynthesisDecision(t *testing.T) {
	decision, err := ParseSynthesisDecision(`{"outcome":"pick","chosen_pr":12,"rejected_prs":[10,11],"reason":"best diff"}`)
	if err != nil {
		t.Fatalf("ParseSynthesisDecision: %v", err)
	}
	if decision.Outcome != "pick" || decision.ChosenPR != 12 {
		t.Fatalf("unexpected decision: %+v", decision)
	}
	if _, err := ParseSynthesisDecision(`{"outcome":"nope","reason":"bad"}`); err == nil {
		t.Fatal("expected invalid outcome to fail")
	}
}
