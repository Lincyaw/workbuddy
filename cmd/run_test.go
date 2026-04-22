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
)

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
	mockRT := &mockRuntime{name: config.RuntimeCodex, resultFn: func(ctx context.Context, agent *config.AgentConfig, task *launcher.TaskContext) (*launcher.Result, error) {
		return &launcher.Result{ExitCode: 23, LastMessage: "review failed", SessionPath: filepath.Join(task.WorkDir, "artifact.jsonl")}, nil
	}}
	lnch := launcher.NewLauncher()
	lnch.Register(mockRT, config.RuntimeCodex, config.RuntimeCodex)

	var out bytes.Buffer
	var errOut bytes.Buffer
	err := runRuntimeWithOpts(context.Background(), &runOpts{
		runtime:  config.RuntimeCodex,
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

func TestResolveRunPromptMissingFileShowsSuggestedFix(t *testing.T) {
	missingPath := filepath.Join(t.TempDir(), "missing-prompt.txt")

	_, err := resolveRunPrompt(&runOpts{promptFile: missingPath})
	if err == nil {
		t.Fatal("expected missing prompt file to fail")
	}
	if got, want := err.Error(), `run: prompt file "`+missingPath+`" was not found. Create the file or pass the prompt directly with --prompt.`; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	if strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("error leaked raw syscall text: %v", err)
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
		runtime:  config.RuntimeCodex,
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
