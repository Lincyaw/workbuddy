package launcher

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
)

// sessionFinder locates session/log artifacts for a given runtime after execution.
type sessionFinder func(repoPath string) string

// launchProcess is the shared execution logic for all runtimes.
// It renders the command template, builds the subprocess, runs it, and
// returns a Result. The runtimeName is used in error messages, and
// findSession locates runtime-specific session artifacts.
func launchProcess(
	ctx context.Context,
	runtimeName string,
	agent *config.AgentConfig,
	task *TaskContext,
	findSession sessionFinder,
) (*Result, error) {
	rendered, err := renderCommand(agent.Command, task)
	if err != nil {
		return nil, err
	}

	timeout := agent.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// If the command is "claude -p '...'" or similar, extract the prompt and
	// pass it via stdin to avoid shell quoting issues with issue bodies.
	var cmd *exec.Cmd
	if prompt, ok := extractPrompt(rendered); ok {
		cmd = exec.CommandContext(execCtx, "claude", "-p")
		cmd.Stdin = strings.NewReader(prompt)
	} else {
		cmd = exec.CommandContext(execCtx, "sh", "-c", rendered)
	}
	if task.WorkDir != "" {
		cmd.Dir = task.WorkDir
	}
	cmd.Env = append(os.Environ(), buildEnvVars(task)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
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
		if execCtx.Err() == context.DeadlineExceeded {
			return &Result{
				ExitCode: -1,
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
				Duration: duration,
			}, fmt.Errorf("launcher: %s: timeout after %s: %w", runtimeName, timeout, execCtx.Err())
		} else if ctx.Err() != nil {
			return &Result{
				ExitCode: -1,
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
				Duration: duration,
			}, fmt.Errorf("launcher: %s: cancelled: %w", runtimeName, ctx.Err())
		} else if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("launcher: %s: exec: %w", runtimeName, runErr)
		}
	}

	meta := parseMeta(stdout.String())
	var sessionPath string
	if findSession != nil {
		sessionPath = findSession(task.Repo)
	}

	return &Result{
		ExitCode:    exitCode,
		Stdout:      stdout.String(),
		Stderr:      stderr.String(),
		Duration:    duration,
		Meta:        meta,
		SessionPath: sessionPath,
	}, nil
}
