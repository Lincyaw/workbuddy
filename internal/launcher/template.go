package launcher

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"text/template"
)

// shellEscape wraps a string in single quotes with proper escaping so it is
// safe to interpolate into a shell command. Interior single quotes are replaced
// with the sequence '\'' (end quote, escaped quote, start quote).
func shellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// safeTaskContext returns a shallow copy of TaskContext where user-controlled
// fields (Issue.Title, Issue.Body, Issue.Labels) are shell-escaped so that
// direct interpolation via {{.Issue.Title}} etc. cannot inject shell commands.
func safeTaskContext(task *TaskContext) *TaskContext {
	escaped := *task
	escaped.Issue = IssueContext{
		Number: task.Issue.Number,
		Title:  shellEscape(task.Issue.Title),
		Body:   shellEscape(task.Issue.Body),
		Labels: make([]string, len(task.Issue.Labels)),
	}
	for i, l := range task.Issue.Labels {
		escaped.Issue.Labels[i] = shellEscape(l)
	}
	return &escaped
}

// templateFuncMap provides helper functions available inside command templates.
var templateFuncMap = template.FuncMap{
	"shellEscape": shellEscape,
}

// renderCommand renders the agent command template with the given task context.
// User-controlled fields from the GitHub issue (Title, Body, Labels) are
// automatically shell-escaped to prevent command injection when the rendered
// string is passed to sh -c. The {{shellEscape .Field}} function is also
// available for explicit escaping of any value.
func renderCommand(cmdTemplate string, task *TaskContext) (string, error) {
	tmpl, err := template.New("command").Funcs(templateFuncMap).Parse(cmdTemplate)
	if err != nil {
		return "", fmt.Errorf("launcher: parse command template: %w", err)
	}

	safe := safeTaskContext(task)

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, safe); err != nil {
		return "", fmt.Errorf("launcher: render command template: %w", err)
	}

	return buf.String(), nil
}

// promptPattern matches "claude -p ..." or "claude --print -p ..." command prefixes.
var promptPattern = regexp.MustCompile(`^claude\s+(?:--print\s+)?-p\s+["']`)

// extractPrompt detects a "claude -p '...'" or 'claude -p "..."' command and
// extracts the prompt text. This allows passing the prompt via stdin instead of
// sh -c, avoiding shell quoting issues with issue bodies that contain quotes,
// backticks, or code blocks.
func extractPrompt(rendered string) (string, bool) {
	rendered = strings.TrimSpace(rendered)
	if !promptPattern.MatchString(rendered) {
		return "", false
	}
	idx := strings.IndexAny(rendered, `"'`)
	if idx < 0 {
		return "", false
	}
	quote := rendered[idx]
	rest := rendered[idx+1:]
	lastIdx := strings.LastIndexByte(rest, quote)
	if lastIdx < 0 {
		return "", false
	}
	return rest[:lastIdx], true
}
