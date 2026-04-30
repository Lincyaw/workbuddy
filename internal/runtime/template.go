package runtime

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"text/template"
)

// ShellEscape wraps a string in single quotes with proper escaping so it is
// safe to interpolate into a shell command.
func ShellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func safeTaskContext(task *TaskContext) *TaskContext {
	escaped := *task
	escaped.Issue = IssueContext{
		Number:       task.Issue.Number,
		Title:        ShellEscape(task.Issue.Title),
		Body:         ShellEscape(task.Issue.Body),
		Labels:       make([]string, len(task.Issue.Labels)),
		CommentsText: ShellEscape(task.Issue.CommentsText),
	}
	for i, l := range task.Issue.Labels {
		escaped.Issue.Labels[i] = ShellEscape(l)
	}
	escaped.RelatedPRsText = ShellEscape(task.RelatedPRsText)
	return &escaped
}

var templateFuncMap = template.FuncMap{
	"shellEscape": ShellEscape,
}

// TemplateFuncMap returns a copy of the template FuncMap registered for
// agent prompt/command rendering. Callers (e.g. the validator) use this to
// parse templates with the same set of helpers used at execution time, so
// references like `{{shellEscape ...}}` are not flagged as parse errors.
func TemplateFuncMap() template.FuncMap {
	out := make(template.FuncMap, len(templateFuncMap))
	for k, v := range templateFuncMap {
		out[k] = v
	}
	return out
}

func RenderCommand(cmdTemplate string, task *TaskContext) (string, error) {
	tmpl, err := template.New("command").Funcs(templateFuncMap).Parse(cmdTemplate)
	if err != nil {
		return "", fmt.Errorf("runtime: parse command template: %w", err)
	}

	safe := safeTaskContext(task)

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, safe); err != nil {
		return "", fmt.Errorf("runtime: render command template: %w", err)
	}

	return buf.String(), nil
}

func RenderCommandRaw(cmdTemplate string, task *TaskContext) (string, error) {
	tmpl, err := template.New("command").Funcs(templateFuncMap).Parse(cmdTemplate)
	if err != nil {
		return "", fmt.Errorf("runtime: parse command template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, task); err != nil {
		return "", fmt.Errorf("runtime: render command template: %w", err)
	}

	return buf.String(), nil
}

// AssembleAgentPrompt combines the agent's prompt body with the runtime-
// generated transition footer (from the workflow state metadata attached to
// task) and returns the unrendered template string. Callers that want the
// final rendered prompt should follow up with RenderCommandRaw or
// RenderCommand on the result.
//
// When task carries no workflow-state metadata, or the state is terminal
// (no outgoing transitions), the body is returned unchanged. The combined
// output preserves the body's leading content and appends the footer with a
// single blank line of separation, matching AssemblePrompt.
func AssembleAgentPrompt(promptBody string, task *TaskContext) string {
	body := promptBody
	if task == nil {
		return body
	}
	if prelude := rolloutPromptPrelude(task); prelude != "" {
		body = strings.TrimRight(prelude+"\n"+body, "\n")
	}
	enterLabel, transitions := task.WorkflowStateMetadata()
	footer := BuildTransitionFooter(task.WorkflowStateName(), enterLabel, transitions)
	return AssemblePrompt(body, footer)
}

func rolloutPromptPrelude(task *TaskContext) string {
	if task == nil || task.Rollout.Index <= 0 || task.Rollout.Total <= 1 {
		return ""
	}
	return fmt.Sprintf("You are rollout %d of %d independent attempts at this issue. Other rollouts are running in parallel; do not coordinate with them. Make the choices you believe in -- don't play it safe just because someone else might do that. The synthesizer will pick or combine across rollouts later.", task.Rollout.Index, task.Rollout.Total)
}

// RenderAgentPrompt is a convenience wrapper that assembles body+footer and
// renders the result against task. Used at dispatch boundaries that send the
// rendered prompt directly to a runtime adapter (claude / codex bridge).
func RenderAgentPrompt(promptBody string, task *TaskContext) (string, error) {
	combined := AssembleAgentPrompt(promptBody, task)
	if strings.TrimSpace(combined) == "" {
		return "", nil
	}
	return RenderCommandRaw(combined, task)
}

var promptPattern = regexp.MustCompile(`^claude\s+.*-p\s+["']`)

func ExtractPrompt(rendered string) (prompt string, extraArgs []string, ok bool) {
	rendered = strings.TrimSpace(rendered)
	if !promptPattern.MatchString(rendered) {
		return "", nil, false
	}

	afterClaude := strings.TrimSpace(rendered[len("claude"):])
	var args []string
	for {
		if strings.HasPrefix(afterClaude, "-p ") || strings.HasPrefix(afterClaude, "-p\t") {
			afterClaude = strings.TrimSpace(afterClaude[2:])
			break
		}
		spaceIdx := strings.IndexAny(afterClaude, " \t")
		if spaceIdx < 0 {
			return "", nil, false
		}
		args = append(args, afterClaude[:spaceIdx])
		afterClaude = strings.TrimSpace(afterClaude[spaceIdx:])
	}

	if len(afterClaude) == 0 {
		return "", nil, false
	}
	quote := afterClaude[0]
	rest := afterClaude[1:]
	lastIdx := strings.LastIndexByte(rest, quote)
	if lastIdx < 0 {
		return "", nil, false
	}
	return rest[:lastIdx], args, true
}
