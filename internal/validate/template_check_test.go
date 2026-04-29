package validate

import (
	"strings"
	"testing"
)

func TestValidateAgentTemplate_ValidPrompt(t *testing.T) {
	schema := BuildTaskContextSchema()
	agent := &agentDoc{
		Path:       "/tmp/agents/dev-agent.md",
		Name:       "dev-agent",
		Prompt:     "Issue {{.Issue.Number}} in {{.Repo}}: {{.Issue.Title}}",
		PromptLine: 13,
		Context:    []string{"Issue.Number", "Issue.Title", "Repo"},
	}
	if got := validateAgentTemplate(agent, schema); len(got) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", got)
	}
}

func TestValidateAgentTemplate_RangeOverComments(t *testing.T) {
	schema := BuildTaskContextSchema()
	agent := &agentDoc{
		Path:       "/tmp/agents/dev-agent.md",
		Name:       "dev-agent",
		Prompt:     "{{range .Issue.Comments}}- {{.Author}}: {{.Body}}\n{{end}}",
		PromptLine: 13,
		// Declaring the slice covers any iterator-element reference.
		Context: []string{"Issue.Comments"},
	}
	if got := validateAgentTemplate(agent, schema); len(got) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", got)
	}
}

func TestValidateAgentTemplate_UnknownField(t *testing.T) {
	schema := BuildTaskContextSchema()
	agent := &agentDoc{
		Path: "/tmp/agents/dev-agent.md",
		Name: "dev-agent",
		// `repo` (lowercase) is one transposition away from `Repo`
		// per case-insensitive Levenshtein, so the suggestion fires.
		Prompt:     "Repo: {{.repo}}",
		PromptLine: 10,
	}
	got := validateAgentTemplate(agent, schema)
	if len(got) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d: %+v", len(got), got)
	}
	if got[0].Code != CodeUnknownTemplateField {
		t.Fatalf("code = %q, want %q", got[0].Code, CodeUnknownTemplateField)
	}
	if !strings.Contains(got[0].Message, "did you mean") {
		t.Errorf("expected suggestion in message, got %q", got[0].Message)
	}
	if !strings.Contains(got[0].Message, "Repo") {
		t.Errorf("expected suggestion to include Repo, got %q", got[0].Message)
	}
	// promptLine 10 + tmpl line 1 - 1 = 10
	if got[0].Line != 10 {
		t.Errorf("expected line 10, got %d", got[0].Line)
	}
}

func TestValidateAgentTemplate_UnknownFieldNoSuggestion(t *testing.T) {
	schema := BuildTaskContextSchema()
	agent := &agentDoc{
		Path:       "/tmp/agents/dev-agent.md",
		Name:       "dev-agent",
		Prompt:     "{{.NonexistentLongName}}",
		PromptLine: 10,
	}
	got := validateAgentTemplate(agent, schema)
	if len(got) != 1 || got[0].Code != CodeUnknownTemplateField {
		t.Fatalf("expected one WB-T101, got %+v", got)
	}
	if strings.Contains(got[0].Message, "did you mean") {
		t.Errorf("did not expect a hint for far-off name, got %q", got[0].Message)
	}
}

func TestValidateAgentTemplate_UnknownNestedField(t *testing.T) {
	schema := BuildTaskContextSchema()
	agent := &agentDoc{
		Path:       "/tmp/agents/dev-agent.md",
		Name:       "dev-agent",
		Prompt:     "{{.Issue.Reporter}}",
		PromptLine: 10,
	}
	got := validateAgentTemplate(agent, schema)
	if len(got) != 1 || got[0].Code != CodeUnknownTemplateField {
		t.Fatalf("expected one WB-T101, got %+v", got)
	}
}

func TestValidateAgentTemplate_UnknownInRange(t *testing.T) {
	schema := BuildTaskContextSchema()
	agent := &agentDoc{
		Path:       "/tmp/agents/dev-agent.md",
		Name:       "dev-agent",
		Prompt:     "{{range .Issue.Comments}}{{.Username}}{{end}}",
		PromptLine: 10,
		Context:    []string{"Issue.Comments"},
	}
	got := validateAgentTemplate(agent, schema)
	if len(got) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d: %+v", len(got), got)
	}
	if got[0].Code != CodeUnknownTemplateField {
		t.Errorf("code = %q, want %q", got[0].Code, CodeUnknownTemplateField)
	}
	if !strings.Contains(got[0].Message, "Issue.Comments[].Username") {
		t.Errorf("expected resolved iteration path in message, got %q", got[0].Message)
	}
}

func TestValidateAgentTemplate_ParseError(t *testing.T) {
	schema := BuildTaskContextSchema()
	agent := &agentDoc{
		Path:       "/tmp/agents/dev-agent.md",
		Name:       "dev-agent",
		Prompt:     "{{.Issue.Number", // missing }}
		PromptLine: 10,
	}
	got := validateAgentTemplate(agent, schema)
	if len(got) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d: %+v", len(got), got)
	}
	if got[0].Code != CodePromptParseError {
		t.Errorf("code = %q, want %q", got[0].Code, CodePromptParseError)
	}
}

func TestValidateAgentTemplate_ShellEscapeFunc(t *testing.T) {
	schema := BuildTaskContextSchema()
	agent := &agentDoc{
		Path:       "/tmp/agents/dev-agent.md",
		Name:       "dev-agent",
		Prompt:     "{{shellEscape .Issue.Title}}",
		PromptLine: 10,
		Context:    []string{"Issue.Title"},
	}
	if got := validateAgentTemplate(agent, schema); len(got) != 0 {
		t.Fatalf("shellEscape func should be known: %+v", got)
	}
}

func TestValidateAgentTemplate_EmptyPromptOK(t *testing.T) {
	if got := validateAgentTemplate(&agentDoc{Name: "x"}, BuildTaskContextSchema()); len(got) != 0 {
		t.Fatalf("empty prompt produced diagnostics: %+v", got)
	}
}

func TestLevenshteinSmall(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "abc", 3},
		{"abc", "", 3},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"abc", "axc", 1},
		{"kitten", "sitting", 3},
	}
	for _, c := range cases {
		if got := levenshtein(c.a, c.b); got != c.want {
			t.Errorf("levenshtein(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
