package validate

import (
	"testing"
)

// TestValidateAgentFormat_LegacyPromptField fires WB-F001 when the agent file
// still carries a top-level `prompt:` frontmatter key.
func TestValidateAgentFormat_LegacyPromptField(t *testing.T) {
	agent := &agentDoc{
		Path:                 "/tmp/agents/dev-agent.md",
		Name:                 "dev-agent",
		Prompt:               "Body",
		PromptLine:           10,
		Context:              []string{"Repo"},
		HasLegacyPromptField: true,
		LegacyPromptLine:     7,
	}
	got := validateAgentFormat(agent)
	requireOneCode(t, got, CodeLegacyPromptField)
	if got[0].Line != 7 {
		t.Errorf("expected diagnostic on line 7, got %d", got[0].Line)
	}
	if got[0].EffectiveSeverity() != SeverityError {
		t.Errorf("severity = %q, want error", got[0].EffectiveSeverity())
	}
}

// TestValidateAgentFormat_EmptyBody fires WB-F002 when the body is whitespace-
// only or absent. The body IS the prompt template now.
func TestValidateAgentFormat_EmptyBody(t *testing.T) {
	agent := &agentDoc{
		Path:       "/tmp/agents/dev-agent.md",
		Name:       "dev-agent",
		Prompt:     "   \n\n",
		PromptLine: 12,
		Context:    []string{"Repo"},
	}
	got := validateAgentFormat(agent)
	requireOneCode(t, got, CodeEmptyPromptBody)
}

// TestValidateAgentFormat_MissingContext fires WB-CT001 when the frontmatter
// has no context: declaration.
func TestValidateAgentFormat_MissingContext(t *testing.T) {
	agent := &agentDoc{
		Path:       "/tmp/agents/dev-agent.md",
		Name:       "dev-agent",
		NameLine:   2,
		Prompt:     "Repo: {{.Repo}}",
		PromptLine: 9,
	}
	got := validateAgentFormat(agent)
	requireOneCode(t, got, CodeMissingContextField)
}

// TestValidateAgentFormat_AllOK passes when the new format is satisfied:
// no legacy prompt:, non-empty body, declared context.
func TestValidateAgentFormat_AllOK(t *testing.T) {
	agent := &agentDoc{
		Path:       "/tmp/agents/dev-agent.md",
		Name:       "dev-agent",
		Prompt:     "Repo: {{.Repo}}",
		PromptLine: 9,
		Context:    []string{"Repo"},
	}
	if got := validateAgentFormat(agent); len(got) != 0 {
		t.Fatalf("expected no diagnostics, got: %+v", got)
	}
}

// TestValidateContextCoverage_UndeclaredFiresCT002 covers the heart of the
// "explicitly declare your inputs" feature: a prompt that references a
// TaskContext field path the frontmatter did not declare emits WB-CT002.
func TestValidateContextCoverage_UndeclaredFiresCT002(t *testing.T) {
	schema := BuildTaskContextSchema()
	agent := &agentDoc{
		Path:       "/tmp/agents/dev-agent.md",
		Name:       "dev-agent",
		Prompt:     "Issue {{.Issue.Number}}: {{.Issue.Title}} in {{.Repo}}",
		PromptLine: 10,
		// Issue.Title not declared on purpose.
		Context: []string{"Issue.Number", "Repo"},
	}
	got := validateAgentTemplate(agent, schema)
	hasCT002 := false
	for _, d := range got {
		if d.Code == CodeContextFieldUndeclared && contains(d.Message, "Issue.Title") {
			hasCT002 = true
		}
	}
	if !hasCT002 {
		t.Fatalf("expected WB-CT002 for Issue.Title, got: %+v", got)
	}
}

// TestValidateContextCoverage_UnusedFiresCT003 — declared context entry that
// the prompt never references emits WB-CT003 (warning).
func TestValidateContextCoverage_UnusedFiresCT003(t *testing.T) {
	schema := BuildTaskContextSchema()
	agent := &agentDoc{
		Path:       "/tmp/agents/dev-agent.md",
		Name:       "dev-agent",
		Prompt:     "Repo: {{.Repo}}",
		PromptLine: 10,
		Context:    []string{"Repo", "Issue.CommentsText"},
	}
	got := validateAgentTemplate(agent, schema)
	hasCT003 := false
	for _, d := range got {
		if d.Code == CodeContextFieldUnused && contains(d.Message, "Issue.CommentsText") {
			hasCT003 = true
			if d.EffectiveSeverity() != SeverityWarning {
				t.Errorf("WB-CT003 severity = %q, want warning", d.EffectiveSeverity())
			}
		}
	}
	if !hasCT003 {
		t.Fatalf("expected WB-CT003 for unused Issue.CommentsText, got: %+v", got)
	}
}

// TestValidateContextCoverage_SliceDeclarationCoversIterator — declaring a
// slice path covers any iterator-element reference inside a `range` block.
func TestValidateContextCoverage_SliceDeclarationCoversIterator(t *testing.T) {
	schema := BuildTaskContextSchema()
	agent := &agentDoc{
		Path:       "/tmp/agents/dev-agent.md",
		Name:       "dev-agent",
		Prompt:     "{{range .Issue.Comments}}- {{.Author}}: {{.Body}}\n{{end}}",
		PromptLine: 10,
		Context:    []string{"Issue.Comments"},
	}
	got := validateAgentTemplate(agent, schema)
	for _, d := range got {
		if d.Code == CodeContextFieldUndeclared || d.Code == CodeContextFieldUnused {
			t.Fatalf("unexpected coverage diagnostic: %+v", d)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
