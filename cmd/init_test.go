package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/config"
)

func TestRunInitWithOpts_CreatesFunctionalServeScaffold(t *testing.T) {
	root := t.TempDir()
	var out bytes.Buffer

	err := runInitWithOpts(context.Background(), root, &initOpts{repo: "octo/workbuddy"}, &out)
	if err != nil {
		t.Fatalf("runInitWithOpts: %v", err)
	}

	cfgDir := filepath.Join(root, ".github", "workbuddy")
	cfg, warnings, err := config.LoadConfig(cfgDir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
	if cfg.Global.Repo != "octo/workbuddy" {
		t.Fatalf("repo = %q", cfg.Global.Repo)
	}
	if _, ok := cfg.Agents["dev-agent"]; !ok {
		t.Fatalf("dev-agent missing from config")
	}
	if _, ok := cfg.Agents["review-agent"]; !ok {
		t.Fatalf("review-agent missing from config")
	}
	if _, ok := cfg.Workflows["default"]; !ok {
		t.Fatalf("default workflow missing from config")
	}

	for _, rel := range []string{
		".workbuddy/logs",
		".workbuddy/sessions",
		".workbuddy/worktrees",
		".workbuddy/.gitignore",
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Fatalf("missing %s: %v", rel, err)
		}
	}
	if got := out.String(); !strings.Contains(got, "wrote .github/workbuddy/config.yaml") {
		t.Fatalf("stdout missing scaffold summary: %q", got)
	}
}

func TestRunInitWithOpts_FailsOnConflictWithoutForce(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, ".github", "workbuddy", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("repo: keep/me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runInitWithOpts(context.Background(), root, &initOpts{repo: "octo/workbuddy"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), "config.yaml") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "repo: keep/me\n" {
		t.Fatalf("config.yaml unexpectedly changed: %q", string(data))
	}
}

func TestRunInitWithOpts_ForceOverwritesExistingFiles(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, ".github", "workbuddy", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("repo: keep/me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runInitWithOpts(context.Background(), root, &initOpts{repo: "octo/workbuddy", force: true}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("runInitWithOpts: %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "repo: octo/workbuddy") {
		t.Fatalf("config.yaml not overwritten: %q", string(data))
	}
}

func TestParseGitRemoteURL(t *testing.T) {
	tests := map[string]string{
		"git@github.com:Lincyaw/workbuddy.git":       "Lincyaw/workbuddy",
		"https://github.com/Lincyaw/workbuddy.git":   "Lincyaw/workbuddy",
		"ssh://git@github.com/Lincyaw/workbuddy.git": "Lincyaw/workbuddy",
		"http://github.com/Lincyaw/workbuddy.git":    "Lincyaw/workbuddy",
		"not-a-remote": "",
		"https://github.com/Lincyaw/workbuddy/extra": "",
	}
	for remote, want := range tests {
		if got := parseGitRemoteURL(remote); got != want {
			t.Fatalf("parseGitRemoteURL(%q) = %q, want %q", remote, got, want)
		}
	}
}
