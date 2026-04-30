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

func TestHooksTestRunsCommandActionWithoutEventlog(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "hooks.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
hooks:
  - name: echo
    events: ["alert"]
    action:
      type: command
      cmd: ["sh", "-c", "printf '%s/%s' \"$WORKBUDDY_EVENT_TYPE\" \"$WORKBUDDY_HOOK_NAME\"; cat"]
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	fixturePath := filepath.Join(dir, "ev.json")
	if err := os.WriteFile(fixturePath, []byte(`{"type":"alert","repo":"x/y","issue_num":7,"payload":{"hello":"world"}}`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	rootCmd.SetArgs([]string{"hooks", "test",
		"--" + flagHooksConfig, cfgPath,
		"--hook", "echo",
		"--event-fixture", fixturePath,
	})
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	rootCmd.SetOut(stdout)
	rootCmd.SetErr(stderr)
	defer rootCmd.SetArgs(nil)

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "exit: 0") {
		t.Fatalf("expected exit: 0 in output, got: %q", out)
	}
	if !strings.Contains(out, "alert/echo") {
		t.Fatalf("expected env vars to surface in stdout, got: %q", out)
	}
	if !strings.Contains(out, `"hello":"world"`) {
		t.Fatalf("expected fixture payload to be piped on stdin and echoed back, got: %q", out)
	}
}

func TestHooksTestUnknownHookErrors(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "hooks.yaml")
	if err := os.WriteFile(cfgPath, []byte(`hooks: [{name: a, events: [alert], action: {type: webhook, url: http://x}}]`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	fxPath := filepath.Join(dir, "ev.json")
	if err := os.WriteFile(fxPath, []byte(`{"type":"alert"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	rootCmd.SetArgs([]string{"hooks", "test",
		"--" + flagHooksConfig, cfgPath,
		"--hook", "does-not-exist",
		"--event-fixture", fxPath,
	})
	rootCmd.SetOut(&bytes.Buffer{})
	rootCmd.SetErr(&bytes.Buffer{})
	defer rootCmd.SetArgs(nil)

	err := rootCmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected hook-not-found error, got %v", err)
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
