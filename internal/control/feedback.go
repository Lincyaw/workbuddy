package control

import (
	"fmt"
	"strings"
)

// Delta is the gap between objective and observation.
type Delta struct {
	Missing []string // human-readable list of unmet conditions
	Met     []string // conditions that passed
	AllMet  bool     // true when nothing is missing
}

// Feedback computes the delta between objective and observation.
func Feedback(obj *Objective, obs *Observation) *Delta {
	d := &Delta{}

	if obj.BranchPushed {
		if obs.BranchPushed {
			d.Met = append(d.Met, "branch pushed")
		} else {
			d.Missing = append(d.Missing, "branch not pushed")
		}
	}

	if obj.PROpen {
		if obs.PROpen {
			d.Met = append(d.Met, fmt.Sprintf("PR open (#%d)", obs.PRNumber))
		} else {
			d.Missing = append(d.Missing, "PR not open")
		}
	}

	if obj.LabelTarget != "" {
		if obs.LabelCurrent == obj.LabelTarget {
			d.Met = append(d.Met, fmt.Sprintf("label is %s", obs.LabelCurrent))
		} else {
			d.Missing = append(d.Missing, fmt.Sprintf("label is %q, want %q", obs.LabelCurrent, obj.LabelTarget))
		}
	}

	if obj.LabelNotEqual != "" {
		if obs.LabelCurrent != obj.LabelNotEqual && obs.LabelCurrent != "" {
			d.Met = append(d.Met, fmt.Sprintf("label moved from %s to %s", obj.LabelNotEqual, obs.LabelCurrent))
		} else {
			d.Missing = append(d.Missing, fmt.Sprintf("label is still %q, want any other status", obs.LabelCurrent))
		}
	}

	if obj.IssueCommented {
		if obs.IssueCommented {
			d.Met = append(d.Met, "issue commented")
		} else {
			d.Missing = append(d.Missing, "no comment posted on issue")
		}
	}

	d.AllMet = len(d.Missing) == 0
	return d
}

// Message renders the delta as a natural language feedback message for the
// agent, including the repo/issue/branch context so the agent knows what
// to fix.
func (d *Delta) Message(repo string, issueNum int, branch string) string {
	var b strings.Builder
	b.WriteString("Post-condition check for ")
	fmt.Fprintf(&b, "%s#%d (branch %s):\n", repo, issueNum, branch)

	for _, m := range d.Met {
		fmt.Fprintf(&b, "  [ok] %s\n", m)
	}
	for _, m := range d.Missing {
		fmt.Fprintf(&b, "  [MISSING] %s\n", m)
	}

	if len(d.Missing) > 0 {
		b.WriteString("\nPlease address the missing conditions above.")
	}
	return b.String()
}
