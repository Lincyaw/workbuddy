package cmd

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	"github.com/spf13/cobra"
)

func TestParseRunFlags(t *testing.T) {
	cmd := &cobra.Command{Use: "run"}
	cmd.Flags().String("runtime", config.RuntimeCodexExec, "")
	cmd.Flags().StringP("prompt", "p", "", "")
	cmd.Flags().String("prompt-file", "", "")
	cmd.Flags().String("workdir", ".", "")
	cmd.Flags().String("sandbox", "danger-full-access", "")
	cmd.Flags().String("approval", "never", "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().Duration("timeout", 30*time.Minute, "")
	if err := cmd.Flags().Set("runtime", "codex"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("prompt", "hello"); err != nil {
		t.Fatal(err)
	}
	opts, err := parseRunFlags(cmd)
	if err != nil {
		t.Fatalf("parseRunFlags: %v", err)
	}
	if opts.runtime != "codex" || opts.prompt != "hello" {
		t.Fatalf("unexpected opts: %+v", opts)
	}
}

func TestRunRuntimeWithOpts_UsesLauncher(t *testing.T) {
	mockRT := &mockRuntime{name: config.RuntimeClaudeShot, resultFn: func(ctx context.Context, agent *config.AgentConfig, task *launcher.TaskContext) (*launcher.Result, error) {
		return &launcher.Result{ExitCode: 0, LastMessage: "review complete", SessionPath: filepath.Join(task.WorkDir, "artifact.jsonl")}, nil
	}}
	lnch := launcher.NewLauncher()
	lnch.Register(mockRT, config.RuntimeClaudeCode, config.RuntimeClaudeShot)

	var out bytes.Buffer
	var errOut bytes.Buffer
	err := runRuntimeWithOpts(context.Background(), &runOpts{
		runtime:  config.RuntimeClaudeCode,
		prompt:   "review this",
		workdir:  t.TempDir(),
		sandbox:  "danger-full-access",
		approval: "never",
		timeout:  time.Minute,
	}, lnch, &out, &errOut)
	if err != nil {
		t.Fatalf("runRuntimeWithOpts: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "review complete" {
		t.Fatalf("stdout = %q", got)
	}
	if !strings.Contains(errOut.String(), "artifact=") {
		t.Fatalf("stderr missing artifact info: %q", errOut.String())
	}
	if mockRT.CallCount() != 1 {
		t.Fatalf("call count = %d", mockRT.CallCount())
	}
}

func TestRunRuntimeWithOpts_FailsOnNonZeroExit(t *testing.T) {
	mockRT := &mockRuntime{name: config.RuntimeCodexExec, resultFn: func(ctx context.Context, agent *config.AgentConfig, task *launcher.TaskContext) (*launcher.Result, error) {
		return &launcher.Result{ExitCode: 23, LastMessage: "review failed", SessionPath: filepath.Join(task.WorkDir, "artifact.jsonl")}, nil
	}}
	lnch := launcher.NewLauncher()
	lnch.Register(mockRT, config.RuntimeCodex, config.RuntimeCodexExec)

	var out bytes.Buffer
	var errOut bytes.Buffer
	err := runRuntimeWithOpts(context.Background(), &runOpts{
		runtime:  config.RuntimeCodexExec,
		prompt:   "review this",
		workdir:  t.TempDir(),
		sandbox:  "danger-full-access",
		approval: "never",
		timeout:  time.Minute,
	}, lnch, &out, &errOut)
	if err == nil {
		t.Fatal("expected non-zero exit error")
	}
	if !strings.Contains(err.Error(), "runtime exited with code 23") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "review failed" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestRunRuntime_CodexPromptE2E(t *testing.T) {
	if os.Getenv("CODEX_E2E") != "1" {
		t.Skip("set CODEX_E2E=1 to run codex CLI review e2e")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not installed")
	}
	repo := t.TempDir()
	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v failed: %v\n%s", name, args, err, string(out))
		}
	}
	run("git", "init")
	run("git", "config", "user.email", "test@example.com")
	run("git", "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", "hello.txt")
	run("git", "commit", "-m", "init")
	if err := os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	lnch := launcher.NewLauncher()
	var out bytes.Buffer
	var errOut bytes.Buffer
	err := runRuntimeWithOpts(context.Background(), &runOpts{
		runtime:  config.RuntimeCodexExec,
		prompt:   "Review the current repository changes in this working directory. Start your reply with REVIEW_OK and then provide a concise review.",
		workdir:  repo,
		sandbox:  "danger-full-access",
		approval: "never",
		timeout:  2 * time.Minute,
	}, lnch, &out, &errOut)
	if err != nil {
		t.Fatalf("runRuntimeWithOpts: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got == "" || !strings.Contains(got, "REVIEW_OK") {
		t.Fatalf("unexpected stdout: %q", got)
	}
	if !strings.Contains(errOut.String(), "artifact=") {
		t.Fatalf("stderr missing artifact info: %q", errOut.String())
	}
}
