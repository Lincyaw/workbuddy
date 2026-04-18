package cmd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunValidateWithOpts_ValidConfig(t *testing.T) {
	configDir := writeValidateFixture(t, validateFixtureFiles{
		"config.yaml":            "repo: octo/workbuddy\n",
		"agents/dev-agent.md":    validateAgentFixture("dev-agent", "status:developing"),
		"agents/review-agent.md": validateAgentFixture("review-agent", "status:reviewing"),
		"workflows/default.md":   validateWorkflowFixture(),
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := runValidateWithOpts(t.Context(), &validateOpts{configDir: configDir}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runValidateWithOpts: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected no stdout, got %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr, got %q", stderr.String())
	}
}

func TestRunValidateWithOpts_MissingAgentExitsOne(t *testing.T) {
	configDir := writeValidateFixture(t, validateFixtureFiles{
		"config.yaml":            "repo: octo/workbuddy\n",
		"agents/review-agent.md": validateAgentFixture("review-agent", "status:reviewing"),
		"workflows/default.md":   strings.Replace(validateWorkflowFixture(), "agent: dev-agent", "agent: ghost-agent", 1),
	})

	var stderr bytes.Buffer
	err := runValidateWithOpts(t.Context(), &validateOpts{configDir: configDir}, io.Discard, &stderr)
	if err == nil {
		t.Fatal("expected validation error")
	}
	var exitErr *cliExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("expected exit 1, got %v", err)
	}
	if !strings.Contains(stderr.String(), "ghost-agent") {
		t.Fatalf("stderr missing missing-agent detail: %q", stderr.String())
	}
}

func TestValidateHelp(t *testing.T) {
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"validate", "--help"})

	err := Execute()
	if err != nil {
		t.Fatalf("Execute help: %v", err)
	}
	help := out.String()
	if !strings.Contains(help, ".github/workbuddy/config.yaml") {
		t.Fatalf("help output missing command description: %q", help)
	}
	if !strings.Contains(help, "--config-dir") {
		t.Fatalf("help output missing --config-dir flag: %q", help)
	}
}

type validateFixtureFiles map[string]string

func writeValidateFixture(t *testing.T, files validateFixtureFiles) string {
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

func validateAgentFixture(name, label string) string {
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

func validateWorkflowFixture() string {
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
