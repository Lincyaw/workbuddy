package validate

import (
	"fmt"
	"strings"
)

// Diagnostic codes emitted by the new-format structural checks (issue #204
// batch 2). These guard the hard-cut to body-as-prompt + required `context:`.
const (
	// CodeEmptyPromptBody — the markdown body (everything after the closing
	// frontmatter `---`) is empty or whitespace-only. The body IS the
	// prompt template now; an empty body means there's nothing to send.
	CodeEmptyPromptBody = "WB-F002"

	// CodeMissingContextField — the agent frontmatter does not declare a
	// `context:` list (or declares an empty one). Every TaskContext field
	// referenced by the prompt must be enumerated explicitly.
	CodeMissingContextField = "WB-CT001"
)

// validateAgentFormat runs WB-F002 and WB-CT001 on a single parsed
// agent document. The validator's parser already ran; we just inspect the
// agentDoc shape produced by parseAgentFile.
func validateAgentFormat(agent *agentDoc) []Diagnostic {
	if agent == nil {
		return nil
	}
	var diags []Diagnostic

	// WB-F002 — empty prompt body.
	if strings.TrimSpace(agent.Prompt) == "" {
		diags = append(diags, Diagnostic{
			Path:     agent.Path,
			Line:     orFallback(agent.PromptLine, agent.NameLine),
			Severity: SeverityError,
			Code:     CodeEmptyPromptBody,
			Message: fmt.Sprintf(
				"agent %q has an empty prompt body; the body after the closing frontmatter `---` is the prompt template",
				agent.Name,
			),
		})
	}

	// WB-CT001 — missing or empty context: declaration.
	if len(agent.Context) == 0 {
		line := agent.ContextDeclLine
		if line <= 0 {
			line = agent.NameLine
		}
		diags = append(diags, Diagnostic{
			Path:     agent.Path,
			Line:     orFallback(line, 1),
			Severity: SeverityError,
			Code:     CodeMissingContextField,
			Message: fmt.Sprintf(
				"agent %q is missing required \"context:\" declaration (list every TaskContext field path the prompt references)",
				agent.Name,
			),
		})
	}

	return diags
}
