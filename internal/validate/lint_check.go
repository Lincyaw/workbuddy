package validate

import (
	"fmt"
	"regexp"
	"strings"
)

// L-layer diagnostic codes ("lint") flag drift between content and control
// flow introduced in issue #204 batch 3. These are warnings — they fire on
// `workbuddy validate` but only fail the exit code under `--strict`.
const (
	// CodeAgentPromptInlinesGhEdit — the agent prompt body contains the
	// literal substring `gh issue edit`. Label transitions are now footer-
	// injected by the runtime (BuildTransitionFooter); embedding the same
	// command in the prompt body duplicates intent and causes drift when the
	// workflow's transitions map is updated.
	CodeAgentPromptInlinesGhEdit = "WB-L001"

	// CodeAgentPromptInlinesStatusLabel — the agent prompt body contains a
	// `status:<word>` literal. Status labels live exclusively in the
	// workflow's transitions map; the footer surfaces them at dispatch time.
	CodeAgentPromptInlinesStatusLabel = "WB-L002"

	// CodeWorkflowProseTemplateExpr — the workflow markdown body, outside
	// the fenced `states:` YAML block, contains a `{{.X}}` Go template
	// expression. Workflow prose is documentation, not a prompt template;
	// `{{...}}` will not be evaluated and is almost always a copy/paste from
	// an agent prompt.
	CodeWorkflowProseTemplateExpr = "WB-L003"
)

// statusLabelPattern matches `status:<token>` anywhere in text — used by
// WB-L002. The trailing token must contain at least one non-whitespace, non-
// punctuation character; we keep this lenient (e.g. `status:reviewing`,
// `status:blocked`, `status:done`) and let the message clarify why it fired.
var statusLabelPattern = regexp.MustCompile(`status:[A-Za-z][A-Za-z0-9_-]*`)

// templateExprPattern matches `{{ .Field }}` style Go template expressions.
// We require the leading `.` so we don't false-positive on `{{`-style
// shorthand used in unrelated documentation (e.g. shell parameter expansion
// like `${{ ... }}` is excluded by the literal `{{.`).
var templateExprPattern = regexp.MustCompile(`\{\{\s*\.[A-Za-z_]`)

// validateAgentPromptLint runs WB-L001 / WB-L002 against an agent prompt body.
//
// Both checks are pure substring scans — code fences inside the prompt are
// NOT excluded, since agents commonly hide the offending example commands in
// fences and then accidentally rely on them. We lean toward false positives
// over false negatives; the diagnostic message documents the rule.
func validateAgentPromptLint(agent *agentDoc) []Diagnostic {
	if agent == nil {
		return nil
	}
	if strings.TrimSpace(agent.Prompt) == "" {
		return nil
	}

	var diags []Diagnostic
	lines := strings.Split(agent.Prompt, "\n")

	// WB-L001 — `gh issue edit` literal anywhere in the body.
	for i, line := range lines {
		if strings.Contains(line, "gh issue edit") {
			diags = append(diags, Diagnostic{
				Path:     agent.Path,
				Line:     promptLineFor(agent.PromptLine, i+1),
				Severity: SeverityWarning,
				Code:     CodeAgentPromptInlinesGhEdit,
				Message: fmt.Sprintf(
					"agent %q prompt body contains literal %q; label transitions are now injected by the runtime as a footer (issue #204 batch 3) — remove the inline command",
					agent.Name, "gh issue edit",
				),
			})
			break // one diagnostic per agent is enough; the cleanup is structural.
		}
	}

	// WB-L002 — `status:<word>` literal anywhere in the body. We only fire
	// once per agent so the diagnostic list stays readable; the message
	// names the first offending match.
	for i, line := range lines {
		match := statusLabelPattern.FindString(line)
		if match == "" {
			continue
		}
		diags = append(diags, Diagnostic{
			Path:     agent.Path,
			Line:     promptLineFor(agent.PromptLine, i+1),
			Severity: SeverityWarning,
			Code:     CodeAgentPromptInlinesStatusLabel,
			Message: fmt.Sprintf(
				"agent %q prompt body contains the label literal %q; status labels live in the workflow's transitions map and are surfaced at dispatch by the footer (issue #204 batch 3) — remove the inline label reference",
				agent.Name, match,
			),
		})
		break
	}

	return diags
}

// validateWorkflowProseLint runs WB-L003 against a workflow markdown body.
// We scan every line of the body that is NOT inside the fenced `states:`
// YAML block; matching `{{.X}}` patterns are reported once per line.
func validateWorkflowProseLint(wf *workflowDoc) []Diagnostic {
	if wf == nil || wf.Body == "" {
		return nil
	}
	bodyLines := strings.Split(wf.Body, "\n")
	yamlStart := wf.StatesYAMLStartLine
	yamlEnd := wf.StatesYAMLEndLine

	var diags []Diagnostic
	for i, line := range bodyLines {
		absLine := wf.BodyStartLine + i
		if yamlStart > 0 && yamlEnd >= yamlStart {
			// The fenced YAML block runs from yamlStart to yamlEnd inclusive;
			// the opening ```yaml fence is at yamlStart - 1. Skip the entire
			// fenced range so authors can write `{{.X}}` examples inside the
			// workflow's structured YAML without tripping the lint.
			if absLine >= yamlStart-1 && absLine <= yamlEnd {
				continue
			}
		}
		if !templateExprPattern.MatchString(line) {
			continue
		}
		diags = append(diags, Diagnostic{
			Path:     wf.Path,
			Line:     absLine,
			Severity: SeverityWarning,
			Code:     CodeWorkflowProseTemplateExpr,
			Message: fmt.Sprintf(
				"workflow %q prose contains a Go-template expression (%q); workflow markdown is documentation, not a prompt template — agents only render `{{.X}}` from agent prompt bodies",
				wf.Name, strings.TrimSpace(templateExprPattern.FindString(line)),
			),
		})
	}
	return diags
}
