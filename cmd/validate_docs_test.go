package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunValidateDocsWithOpts_ValidRepo(t *testing.T) {
	repoRoot := writeValidateDocsFixture(t, validateDocsFixtureFiles{
		"project-index.yaml":                              "requirements: []\n",
		"scripts/sync_codex_plugin.py":                    "#!/usr/bin/env python3\nraise SystemExit(0)\n",
		".codex/skills/example-skill/SKILL.md":            validSkillFixture(),
		".codex/skills/example-skill/references/guide.md": "# ok\n",
		"cmd/initdata/agents/dev-agent.md":                validateDocsAgentFixture("dev-agent", "developing"),
		".github/workbuddy/agents/dev-agent.md":           validateDocsAgentFixture("dev-agent", "developing"),
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runValidateDocsWithOpts(t.Context(), &validateDocsOpts{repoRoot: repoRoot}, &stdout, &stderr); err != nil {
		t.Fatalf("runValidateDocsWithOpts: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected no stdout, got %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr, got %q", stderr.String())
	}
}

func TestRunValidateDocsWithOpts_StrictPromotesWarnings(t *testing.T) {
	repoRoot := writeValidateDocsFixture(t, validateDocsFixtureFiles{
		"project-index.yaml":           "requirements: []\n",
		"scripts/sync_codex_plugin.py": "#!/usr/bin/env python3\nraise SystemExit(0)\n",
		".codex/skills/wrong-name/SKILL.md": `---
name: wrong-name
description: short
---
`,
		"cmd/initdata/agents/dev-agent.md":      validateDocsAgentFixture("dev-agent", "developing"),
		".github/workbuddy/agents/dev-agent.md": validateDocsAgentFixture("dev-agent", "developing"),
	})

	var stderr bytes.Buffer
	err := runValidateDocsWithOpts(t.Context(), &validateDocsOpts{repoRoot: repoRoot, strict: true}, io.Discard, &stderr)
	if err == nil {
		t.Fatal("expected strict warning failure")
	}
	var exitErr *cliExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("expected exit 1, got %v", err)
	}
	if !strings.Contains(stderr.String(), "WB-D302") {
		t.Fatalf("stderr missing WB-D302: %q", stderr.String())
	}
}

func TestRunValidateDocsWithOpts_JSON(t *testing.T) {
	repoRoot := writeValidateDocsFixture(t, validateDocsFixtureFiles{
		"project-index.yaml":                    "requirements: []\n",
		"scripts/sync_codex_plugin.py":          "#!/usr/bin/env python3\nprint('generated plugin drift detected')\nraise SystemExit(1)\n",
		"cmd/initdata/agents/dev-agent.md":      validateDocsAgentFixture("dev-agent", "developing"),
		".github/workbuddy/agents/dev-agent.md": validateDocsAgentFixture("dev-agent", "developing"),
	})

	var stdout bytes.Buffer
	err := runValidateDocsWithOpts(t.Context(), &validateDocsOpts{repoRoot: repoRoot, format: outputFormatJSON}, &stdout, io.Discard)
	if err == nil {
		t.Fatal("expected validation error")
	}

	var result validateDocsResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if result.Valid {
		t.Fatalf("expected invalid result, got %+v", result)
	}
	if len(result.Diagnostics) == 0 || result.Diagnostics[0].Code != "WB-D101" {
		t.Fatalf("unexpected diagnostics: %+v", result.Diagnostics)
	}
}

func TestValidateDocsHelp(t *testing.T) {
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"validate-docs", "--help"})

	if err := Execute(); err != nil {
		t.Fatalf("Execute help: %v", err)
	}
	help := out.String()
	if !strings.Contains(help, "--repo-root") {
		t.Fatalf("help missing --repo-root: %q", help)
	}
	if !strings.Contains(help, "documentation-shaped surfaces") {
		t.Fatalf("help missing command description: %q", help)
	}
}

type validateDocsFixtureFiles map[string]string

func writeValidateDocsFixture(t *testing.T, files validateDocsFixtureFiles) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return root
}

func validateDocsAgentFixture(name, state string) string {
	return `---
name: ` + name + `
description: Agent description that is long enough to avoid short-description linting.
triggers:
  - state: ` + state + `
role: dev
runtime: codex
context:
  - Repo
---
Body.
`
}

func validSkillFixture() string {
	return `---
name: example-skill
description: This description is intentionally long enough to satisfy the validator reliably.
---

See references/guide.md for the detailed template.
`
}
