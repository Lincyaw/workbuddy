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

// extractPrompt detects a "claude [flags] -p '...'" command and extracts the
// prompt text along with any extra flags before -p (e.g., --print). This allows
// passing the prompt via stdin instead of sh -c, avoiding shell quoting issues
// with issue bodies that contain quotes, backticks, or code blocks.
func extractPrompt(rendered string) (prompt string, extraArgs []string, ok bool) {
	rendered = strings.TrimSpace(rendered)
	if !promptPattern.MatchString(rendered) {
		return "", nil, false
	}

	// Parse flags between "claude" and the quoted prompt.
	// The regex guarantees the string starts with "claude" followed by
	// optional flags and then -p <quote>.
	afterClaude := strings.TrimSpace(rendered[len("claude"):])
	var args []string
	for {
		if strings.HasPrefix(afterClaude, "-p ") || strings.HasPrefix(afterClaude, "-p\t") {
			afterClaude = strings.TrimSpace(afterClaude[2:])
			break
		}
		// Consume the next whitespace-delimited token as an extra arg.
		spaceIdx := strings.IndexAny(afterClaude, " \t")
		if spaceIdx < 0 {
			return "", nil, false
		}
		args = append(args, afterClaude[:spaceIdx])
		afterClaude = strings.TrimSpace(afterClaude[spaceIdx:])
	}

	// afterClaude now starts with the opening quote.
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
