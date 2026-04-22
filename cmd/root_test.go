package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecuteUsageOutput(t *testing.T) {
	t.Run("runtime errors do not print usage", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))

		err, stdout, stderr := executeRootForTest(t, "deploy", "stop", "--name", "nonexistent")
		if err == nil {
			t.Fatal("expected deploy stop to fail for a missing deployment")
		}
		if stdout != "" {
			t.Fatalf("expected no stdout, got %q", stdout)
		}
		if strings.Contains(stderr, "Usage:") {
			t.Fatalf("stderr unexpectedly included usage: %q", stderr)
		}
	})

	t.Run("unknown flags still print usage", func(t *testing.T) {
		err, stdout, stderr := executeRootForTest(t, "deploy", "stop", "--bad-flag")
		if err == nil {
			t.Fatal("expected deploy stop with an unknown flag to fail")
		}
		if stdout != "" {
			t.Fatalf("expected no stdout, got %q", stdout)
		}
		if !strings.Contains(stderr, "Error: unknown flag: --bad-flag") {
			t.Fatalf("stderr missing flag error: %q", stderr)
		}
		if !strings.Contains(stderr, "Usage:\n  workbuddy deploy stop [flags]") {
			t.Fatalf("stderr missing usage for syntax error: %q", stderr)
		}
	})

	t.Run("missing required flags still print usage", func(t *testing.T) {
		err, _, stderr := executeRootForTest(t, "worker", "--role", "dev")
		if err == nil {
			t.Fatal("expected worker to fail when --coordinator is omitted")
		}
		if !strings.Contains(stderr, `Error: required flag(s) "coordinator" not set`) {
			t.Fatalf("stderr missing required-flag error: %q", stderr)
		}
		if !strings.Contains(stderr, "Usage:\n  workbuddy worker [flags]") {
			t.Fatalf("stderr missing usage for required-flag error: %q", stderr)
		}
	})

	t.Run("unknown subcommands still print usage", func(t *testing.T) {
		err, _, stderr := executeRootForTest(t, "frob")
		if err == nil {
			t.Fatal("expected frob to fail")
		}
		if !strings.Contains(stderr, `Error: unknown command "frob" for "workbuddy"`) {
			t.Fatalf("stderr missing unknown-command error: %q", stderr)
		}
		if !strings.Contains(stderr, "workbuddy [command]") {
			t.Fatalf("stderr missing usage for unknown subcommand: %q", stderr)
		}
	})
}

func executeRootForTest(t *testing.T, args ...string) (error, string, string) {
	t.Helper()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetOut(os.Stdout)
		rootCmd.SetErr(os.Stderr)
		rootCmd.SetArgs(nil)
	})

	err := Execute()
	return err, stdout.String(), stderr.String()
}
