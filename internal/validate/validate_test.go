package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateDir_ValidConfig(t *testing.T) {
	dir := writeConfigFixture(t, fixtureFiles{
		"config.yaml":            "repo: octo/workbuddy\nport: 8090\n",
		"agents/dev-agent.md":    agentFixture("dev-agent", "status:developing"),
		"agents/review-agent.md": agentFixture("review-agent", "status:reviewing"),
		"workflows/default.md":   validWorkflowFixture(),
	})

	diags, err := ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("expected no diagnostics, got %v", diagnosticsText(diags))
	}
}

func TestValidateDir_MissingAgentReference(t *testing.T) {
	dir := writeConfigFixture(t, fixtureFiles{
		"config.yaml":            "repo: octo/workbuddy\n",
		"agents/review-agent.md": agentFixture("review-agent", "status:reviewing"),
		"workflows/default.md":   strings.Replace(validWorkflowFixture(), "agent: dev-agent", "agent: ghost-agent", 1),
	})

	diags, err := ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if !containsDiagnostic(diags, "ghost-agent") {
		t.Fatalf("expected missing agent diagnostic, got %v", diagnosticsText(diags))
	}
}

func TestValidateDir_UnreachableState(t *testing.T) {
	dir := writeConfigFixture(t, fixtureFiles{
		"config.yaml":            "repo: octo/workbuddy\n",
		"agents/dev-agent.md":    agentFixture("dev-agent", "status:developing"),
		"agents/review-agent.md": agentFixture("review-agent", "status:reviewing"),
		"workflows/default.md":   unreachableWorkflowFixture(),
	})

	diags, err := ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if !containsDiagnostic(diags, `unreachable state "orphan"`) {
		t.Fatalf("expected unreachable state diagnostic, got %v", diagnosticsText(diags))
	}
}

func TestValidateDir_DuplicateEdge(t *testing.T) {
	dir := writeConfigFixture(t, fixtureFiles{
		"config.yaml":            "repo: octo/workbuddy\n",
		"agents/dev-agent.md":    agentFixture("dev-agent", "status:developing"),
		"agents/review-agent.md": agentFixture("review-agent", "status:reviewing"),
		"workflows/default.md":   duplicateEdgeWorkflowFixture(),
	})

	diags, err := ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if !containsDiagnostic(diags, `duplicate edge "developing" -> "reviewing"`) {
		t.Fatalf("expected duplicate edge diagnostic, got %v", diagnosticsText(diags))
	}
}

func TestValidateDir_FallbackRequiresFailedState(t *testing.T) {
	dir := writeConfigFixture(t, fixtureFiles{
		"config.yaml":            "repo: octo/workbuddy\n",
		"agents/dev-agent.md":    agentFixture("dev-agent", "status:developing"),
		"agents/review-agent.md": agentFixture("review-agent", "status:reviewing"),
		"workflows/default.md":   missingFailedWorkflowFixture(),
	})

	diags, err := ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if !containsDiagnostic(diags, `requires a terminal "failed" state`) {
		t.Fatalf("expected failed-state diagnostic, got %v", diagnosticsText(diags))
	}
}

func TestValidateDir_TriggerLabelMismatch(t *testing.T) {
	dir := writeConfigFixture(t, fixtureFiles{
		"config.yaml":          "repo: octo/workbuddy\n",
		"agents/dev-agent.md":  agentFixture("dev-agent", "status:qa"),
		"workflows/default.md": validWorkflowFixture(),
	})

	diags, err := ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if !containsDiagnostic(diags, `trigger label "status:qa"`) {
		t.Fatalf("expected trigger label mismatch diagnostic, got %v", diagnosticsText(diags))
	}
}

type fixtureFiles map[string]string

func writeConfigFixture(t *testing.T, files fixtureFiles) string {
	t.Helper()

	root := t.TempDir()
	configDir := filepath.Join(root, ".github", "workbuddy")
	for rel, content := range files {
		path := filepath.Join(configDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return configDir
}

func agentFixture(name, label string) string {
	return `---
name: ` + name + `
description: test agent
triggers:
  - label: "` + label + `"
    event: labeled
role: dev
command: echo run
---
## Agent
`
}

func validWorkflowFixture() string {
	return `---
name: default
description: valid workflow
trigger:
  issue_label: "workbuddy"
---
## Workflow

` + "```yaml" + `
states:
  developing:
    enter_label: "status:developing"
    agent: dev-agent
    transitions:
      - to: reviewing
      - to: blocked
  reviewing:
    enter_label: "status:reviewing"
    agent: review-agent
    transitions:
      - to: done
      - to: developing
  blocked:
    enter_label: "status:blocked"
    transitions:
      - to: developing
  done:
    enter_label: "status:done"
  failed:
    enter_label: "status:failed"
` + "```" + `
`
}

func unreachableWorkflowFixture() string {
	return strings.Replace(validWorkflowFixture(), `  failed:
    enter_label: "status:failed"
`, `  orphan:
    enter_label: "status:orphan"
  failed:
    enter_label: "status:failed"
`, 1)
}

func duplicateEdgeWorkflowFixture() string {
	return strings.Replace(validWorkflowFixture(), `      - to: blocked`, `      - to: reviewing
      - to: blocked`, 1)
}

func missingFailedWorkflowFixture() string {
	return strings.Replace(validWorkflowFixture(), `  failed:
    enter_label: "status:failed"
`, "", 1)
}

func containsDiagnostic(diags []Diagnostic, needle string) bool {
	for _, diag := range diags {
		if strings.Contains(diag.String(), needle) {
			return true
		}
	}
	return false
}

func diagnosticsText(diags []Diagnostic) string {
	parts := make([]string, 0, len(diags))
	for _, diag := range diags {
		parts = append(parts, diag.String())
	}
	return strings.Join(parts, "\n")
}
