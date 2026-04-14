package launcher

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"text/template"
)

// renderCommand renders the agent command template with the given task context.
func renderCommand(cmdTemplate string, task *TaskContext) (string, error) {
	tmpl, err := template.New("command").Parse(cmdTemplate)
	if err != nil {
		return "", fmt.Errorf("launcher: parse command template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, task); err != nil {
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
