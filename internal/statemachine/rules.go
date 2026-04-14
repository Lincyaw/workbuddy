// Package statemachine evaluates workflow transitions and manages issue state.
package statemachine

import (
	"log"
	"strings"
)

// EvalContext holds the data needed to evaluate a transition condition.
type EvalContext struct {
	EventType     string   // "label_added", "label_removed", "pr_created", etc.
	Labels        []string // current labels on the issue
	LabelAdded    string   // label that was just added (if event is label_added)
	LabelRemoved  string   // label that was just removed (if event is label_removed)
	PRState       string   // "open", "closed", "merged", "" if no PR
	ChecksState   string   // "passed", "failed", "" if unknown
	LatestComment string   // body of the most recent comment
}

// EvaluateCondition parses a `when` condition string and evaluates it against
// the given EvalContext. Returns true if the condition is satisfied.
//
// Supported conditions:
//   - labeled "<label>"       — label was just added
//   - pr_opened               — a PR was created
//   - checks_passed           — CI checks passed
//   - checks_failed           — CI checks failed
//   - approved                — PR was approved
//   - changes_requested       — PR review requested changes
//   - comment_command "<cmd>" — latest comment contains the command
//
// Unknown conditions log a warning and return false.
func EvaluateCondition(when string, ctx *EvalContext) bool {
	when = strings.TrimSpace(when)
	if when == "" {
		return false
	}

	// Try to parse conditions with quoted arguments first.
	if strings.HasPrefix(when, "labeled ") {
		arg := extractQuotedArg(when, "labeled")
		if arg == "" {
			log.Printf("[statemachine] warning: malformed labeled condition: %q", when)
			return false
		}
		return ctx.LabelAdded == arg
	}

	if strings.HasPrefix(when, "comment_command ") {
		arg := extractQuotedArg(when, "comment_command")
		if arg == "" {
			log.Printf("[statemachine] warning: malformed comment_command condition: %q", when)
			return false
		}
		return strings.Contains(ctx.LatestComment, arg)
	}

	// Simple keyword conditions.
	switch when {
	case "pr_opened":
		return ctx.EventType == "pr_created"
	case "checks_passed":
		return ctx.ChecksState == "passed"
	case "checks_failed":
		return ctx.ChecksState == "failed"
	case "approved":
		return ctx.EventType == "approved"
	case "changes_requested":
		return ctx.EventType == "changes_requested"
	default:
		log.Printf("[statemachine] warning: unknown condition type: %q", when)
		return false
	}
}

// extractQuotedArg extracts the quoted argument from a condition string.
// e.g. extractQuotedArg(`labeled "status:review"`, "labeled") → "status:review"
func extractQuotedArg(when, prefix string) string {
	rest := strings.TrimPrefix(when, prefix)
	rest = strings.TrimSpace(rest)
	if len(rest) < 2 || rest[0] != '"' || rest[len(rest)-1] != '"' {
		return ""
	}
	return rest[1 : len(rest)-1]
}
