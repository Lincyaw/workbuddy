package runtime

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/config"
)

func ResolvePromptBody(agentCfg *config.AgentConfig, task *TaskContext) string {
	if task != nil && task.WorkflowStateMode() == config.StateModeSynth {
		return buildSynthesisPrompt(task)
	}
	return strings.TrimSpace(agentCfg.Prompt)
}

func buildSynthesisPrompt(task *TaskContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the synthesis reviewer for repo %s, working on issue #%d.\n\n", task.Repo, task.Issue.Number)
	fmt.Fprintf(&b, "Title: %s\n", task.Issue.Title)
	b.WriteString("Body:\n")
	b.WriteString(task.Issue.Body)
	b.WriteString("\n\n")
	if task.Synthesis != nil {
		fmt.Fprintf(&b, "The previous rollout join required min_successes=%d. Some rollouts may be missing because they failed before opening a candidate PR.\n\n", task.Synthesis.MinSuccesses)
		b.WriteString("Candidate PRs:\n")
		if len(task.Synthesis.Candidates) == 0 {
			b.WriteString("(no candidate PRs were found)\n")
		}
		for _, candidate := range task.Synthesis.Candidates {
			fmt.Fprintf(&b, "\n## Rollout %d\n", candidate.RolloutIndex)
			fmt.Fprintf(&b, "- PR: #%d %s\n", candidate.PullRequest.Number, candidate.PullRequest.Title)
			fmt.Fprintf(&b, "- Branch: %s\n", candidate.PullRequest.HeadRefName)
			fmt.Fprintf(&b, "- Head SHA: %s\n", candidate.PullRequest.HeadSHA)
			fmt.Fprintf(&b, "- URL: %s\n", candidate.PullRequest.URL)
			if candidate.SessionURL != "" {
				fmt.Fprintf(&b, "- Session detail: %s\n", candidate.SessionURL)
			}
			if strings.TrimSpace(candidate.SessionSummary) != "" {
				b.WriteString("- Dev session summary:\n")
				b.WriteString(candidate.SessionSummary)
				b.WriteString("\n")
			}
			if strings.TrimSpace(candidate.Diff) != "" {
				b.WriteString("- Full diff:\n```diff\n")
				b.WriteString(candidate.Diff)
				if !strings.HasSuffix(candidate.Diff, "\n") {
					b.WriteByte('\n')
				}
				b.WriteString("```\n")
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("Choose exactly one outcome: `pick`, `cherry-pick`, or `escalate`.\n")
	b.WriteString("- `pick`: comment on every rejected candidate PR explaining why, close them, and leave exactly one chosen PR to move forward.\n")
	b.WriteString("- `cherry-pick`: create `workbuddy/issue-")
	fmt.Fprintf(&b, "%d/synth", task.Issue.Number)
	b.WriteString("` from `main`, cherry-pick or hand-edit the winning pieces, push it, open a new PR titled `[synth] ")
	b.WriteString(task.Issue.Title)
	b.WriteString("`, and close the candidate PRs.\n")
	b.WriteString("- `escalate`: comment on the issue explaining what was tried and why no candidate is acceptable, then add `needs-human`.\n\n")
	b.WriteString("Use `gh` CLI for all GitHub writes from inside this session. After you finish the GitHub side effects, print exactly one JSON object as your final message with this shape:\n")
	b.WriteString(`{"outcome":"pick|cherry-pick|escalate","chosen_pr":123,"synth_pr":456,"rejected_prs":[1,2],"reason":"short explanation"}`)
	b.WriteString("\nOnly include `chosen_pr` for `pick`, only include `synth_pr` for `cherry-pick`, and always include `rejected_prs` + `reason`.\n")
	return strings.TrimSpace(b.String())
}

func ParseSynthesisDecision(raw string) (*SynthesisDecision, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("missing structured output")
	}
	var decision SynthesisDecision
	if err := json.Unmarshal([]byte(raw), &decision); err != nil {
		return nil, fmt.Errorf("final output is not valid JSON")
	}
	switch decision.Outcome {
	case "pick":
		if decision.ChosenPR <= 0 {
			return nil, fmt.Errorf("pick requires chosen_pr")
		}
	case "cherry-pick":
		if decision.SynthPR <= 0 {
			return nil, fmt.Errorf("cherry-pick requires synth_pr")
		}
	case "escalate":
	default:
		return nil, fmt.Errorf("unknown outcome %q", decision.Outcome)
	}
	if strings.TrimSpace(decision.Reason) == "" {
		return nil, fmt.Errorf("reason is required")
	}
	return &decision, nil
}
