package runtime

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/config"
)

// BuildTransitionFooter returns the footer text to append to an agent prompt
// based on the state's outgoing transitions. It is rendered per dispatch from
// the workflow's `transitions` map (issue #204 batch 3) so prompt bodies do
// not have to hard-code label syntax.
//
// The returned string is itself a Go template — it embeds {{.Issue.Number}}
// and {{.Repo}} so a single text/template Execute call can render the body
// and footer together against the same TaskContext.
//
// Terminal states (no outgoing transitions) return the empty string; the
// caller should append nothing in that case.
func BuildTransitionFooter(currentStateName, currentStateLabel string, transitions map[string]string) string {
	if len(transitions) == 0 {
		return ""
	}

	// Sort by label string for deterministic output. Tests diff this string.
	labels := make([]string, 0, len(transitions))
	for label := range transitions {
		if strings.TrimSpace(label) == "" {
			continue
		}
		labels = append(labels, label)
	}
	if len(labels) == 0 {
		return ""
	}
	sort.Strings(labels)

	var b strings.Builder
	b.WriteString("---\n\n")
	b.WriteString("## Transition\n\n")
	b.WriteString("When you finish, transition this issue by editing labels.\n\n")
	b.WriteString("| Add this label | Resulting state |\n")
	b.WriteString("|----------------|-----------------|\n")
	for _, label := range labels {
		target := transitions[label]
		fmt.Fprintf(&b, "| `%s` | `%s` |\n", label, target)
	}
	b.WriteString("\nRun:\n\n")
	b.WriteString("```\n")
	b.WriteString("gh issue edit {{.Issue.Number}} --repo {{.Repo}}")
	if strings.TrimSpace(currentStateLabel) != "" {
		fmt.Fprintf(&b, " --remove-label \"%s\"", currentStateLabel)
	} else {
		b.WriteString(" --remove-label \"<current-label>\"")
	}
	b.WriteString(" --add-label \"<chosen-label>\"\n")
	b.WriteString("```\n\n")
	if strings.TrimSpace(currentStateLabel) != "" {
		fmt.Fprintf(&b, "The label you remove is `%s` (the enter_label of the `%s` state). "+
			"Pick the `<chosen-label>` from the table above that matches your outcome.\n",
			currentStateLabel, currentStateName)
	} else {
		fmt.Fprintf(&b,
			"Replace `<current-label>` with the enter_label of the `%s` state. "+
				"Pick the `<chosen-label>` from the table above that matches your outcome.\n",
			currentStateName)
	}
	return b.String()
}

// BuildTransitionFooterFromState is a thin convenience wrapper that pulls the
// fields out of a *config.State. Returns "" for nil/terminal states.
func BuildTransitionFooterFromState(stateName string, state *config.State) string {
	if state == nil {
		return ""
	}
	return BuildTransitionFooter(stateName, state.EnterLabel, state.Transitions)
}

// AssemblePrompt concatenates the agent prompt body and the runtime-generated
// transition footer. The combined string is still a Go template — both halves
// are rendered together so the footer can use TaskContext expressions such as
// {{.Issue.Number}}. Empty footers (terminal state) return the body unchanged.
func AssemblePrompt(body, footer string) string {
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(footer) == "" {
		return body
	}
	return body + "\n\n" + footer
}
