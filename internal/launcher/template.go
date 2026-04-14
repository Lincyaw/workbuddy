package launcher

import (
	"bytes"
	"fmt"
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
