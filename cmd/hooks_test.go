package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHooksListEmpty(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.yaml")

	rootCmd.SetArgs([]string{"hooks", "list", "--" + flagHooksConfig, missing})
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rootCmd.SetOut(stdout)
	rootCmd.SetErr(stderr)
	defer rootCmd.SetArgs(nil)

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(stdout.String(), "no hooks configured") {
		t.Fatalf("expected 'no hooks configured', got: %q", stdout.String())
	}
}

func TestHooksListShowsConfiguredHook(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.yaml")
	if err := os.WriteFile(path, []byte(`
hooks:
  - name: dev-test
    events: [alert, dispatch]
    action:
      type: webhook
      url: http://localhost:1234
`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	rootCmd.SetArgs([]string{"hooks", "list", "--" + flagHooksConfig, path})
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rootCmd.SetOut(stdout)
	rootCmd.SetErr(stderr)
	defer rootCmd.SetArgs(nil)

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "dev-test") {
		t.Fatalf("expected hook name in output: %q", out)
	}
	if !strings.Contains(out, "alert,dispatch") {
		t.Fatalf("expected events in output: %q", out)
	}
	if !strings.Contains(out, "webhook") {
		t.Fatalf("expected action type in output: %q", out)
	}
}

func TestResolveHooksConfigPathPrecedence(t *testing.T) {
	t.Setenv("WORKBUDDY_HOOKS_CONFIG", "/tmp/from-env.yaml")
	if got := ResolveHooksConfigPath("/tmp/from-flag.yaml"); got != "/tmp/from-flag.yaml" {
		t.Fatalf("flag should win, got %q", got)
	}
	if got := ResolveHooksConfigPath(""); got != "/tmp/from-env.yaml" {
		t.Fatalf("env should fall back, got %q", got)
	}
}
