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
