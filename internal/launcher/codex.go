package launcher

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
)

// CodexRuntime implements Runtime for codex (codex exec mode).
type CodexRuntime struct{}

// Name returns the runtime identifier.
func (r *CodexRuntime) Name() string { return "codex" }

// Launch executes an agent using the codex runtime.
func (r *CodexRuntime) Launch(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (*Result, error) {
	rendered, err := renderCommand(agent.Command, task)
	if err != nil {
		return nil, err
	}

	timeout := agent.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute // default timeout for codex
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, "sh", "-c", rendered)
	cmd.Dir = task.Repo
	cmd.Env = append(os.Environ(), buildEnvVars(task)...)
	// Use process group so we can kill all child processes on timeout/cancel.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Kill the entire process group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	duration := time.Since(start)

	exitCode := 0
	if runErr != nil {
		// Check context errors first — when deadline/cancel kills the process,
		// cmd.Run() returns an ExitError (signal: killed), but the root cause is the context.
		if execCtx.Err() == context.DeadlineExceeded {
			return &Result{
				ExitCode: -1,
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
				Duration: duration,
				Meta:     nil,
			}, fmt.Errorf("launcher: codex: timeout after %s: %w", timeout, execCtx.Err())
		} else if ctx.Err() != nil {
			return &Result{
				ExitCode: -1,
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
				Duration: duration,
				Meta:     nil,
			}, fmt.Errorf("launcher: codex: cancelled: %w", ctx.Err())
		} else if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("launcher: codex: exec: %w", runErr)
		}
	}

	meta := parseMeta(stdout.String())
	sessionPath := findCodexLogPath(task.Repo)

	return &Result{
		ExitCode:    exitCode,
		Stdout:      stdout.String(),
		Stderr:      stderr.String(),
		Duration:    duration,
		Meta:        meta,
		SessionPath: sessionPath,
	}, nil
}

// findCodexLogPath attempts to locate the codex output log directory.
func findCodexLogPath(repoPath string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	// Codex stores logs under ~/.codex/ or the repo's .codex/ directory
	candidates := []string{
		filepath.Join(repoPath, ".codex", "logs"),
		filepath.Join(home, ".codex", "logs"),
	}

	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}

	return ""
}
