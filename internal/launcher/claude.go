// Package launcher provides multi-runtime agent execution backends.
package launcher

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
)

// ClaudeRuntime implements Runtime for claude-code (claude -p mode).
type ClaudeRuntime struct{}

// Name returns the runtime identifier.
func (r *ClaudeRuntime) Name() string { return "claude-code" }

// Launch executes an agent using the claude-code runtime.
func (r *ClaudeRuntime) Launch(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (*Result, error) {
	rendered, err := renderCommand(agent.Command, task)
	if err != nil {
		return nil, err
	}

	timeout := agent.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute // default timeout for claude
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
			}, fmt.Errorf("launcher: claude-code: timeout after %s: %w", timeout, execCtx.Err())
		} else if ctx.Err() != nil {
			return &Result{
				ExitCode: -1,
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
				Duration: duration,
				Meta:     nil,
			}, fmt.Errorf("launcher: claude-code: cancelled: %w", ctx.Err())
		} else if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("launcher: claude-code: exec: %w", runErr)
		}
	}

	meta := parseMeta(stdout.String())
	sessionPath := findClaudeSessionPath(task.Repo)

	return &Result{
		ExitCode:    exitCode,
		Stdout:      stdout.String(),
		Stderr:      stderr.String(),
		Duration:    duration,
		Meta:        meta,
		SessionPath: sessionPath,
	}, nil
}

// findClaudeSessionPath attempts to locate the claude session directory.
func findClaudeSessionPath(repoPath string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return ""
	}

	// Claude stores sessions under ~/.claude/projects/<sanitized-path>/
	claudeProjectsDir := filepath.Join(home, ".claude", "projects")
	if _, err := os.Stat(claudeProjectsDir); os.IsNotExist(err) {
		return ""
	}

	// Try to find the project directory by matching the repo path
	entries, err := os.ReadDir(claudeProjectsDir)
	if err != nil {
		return ""
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Claude uses a sanitized version of the path as directory name
		dirPath := filepath.Join(claudeProjectsDir, entry.Name())
		// Simple heuristic: check if directory name contains part of repo path
		if containsPathSegment(entry.Name(), absRepo) {
			return dirPath
		}
	}

	return ""
}

// containsPathSegment checks if the sanitized directory name relates to the repo path.
func containsPathSegment(dirName, repoPath string) bool {
	base := filepath.Base(repoPath)
	return len(base) > 0 && len(dirName) > 0 && dirName == base ||
		len(dirName) > 0 && len(repoPath) > 0 && dirName == filepath.Base(repoPath)
}

// buildEnvVars creates the WORKBUDDY_* environment variables for agent execution.
func buildEnvVars(task *TaskContext) []string {
	return []string{
		"WORKBUDDY_ISSUE_NUMBER=" + strconv.Itoa(task.Issue.Number),
		"WORKBUDDY_ISSUE_TITLE=" + task.Issue.Title,
		"WORKBUDDY_ISSUE_BODY=" + task.Issue.Body,
		"WORKBUDDY_REPO=" + task.Repo,
		"WORKBUDDY_SESSION_ID=" + task.Session.ID,
	}
}
