// Package launcher provides multi-runtime agent execution backends.
package launcher

import (
	"context"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Lincyaw/workbuddy/internal/config"
)

// ClaudeRuntime implements Runtime for claude-code (claude -p mode).
type ClaudeRuntime struct{}

// Name returns the runtime identifier.
func (r *ClaudeRuntime) Name() string { return "claude-code" }

// Launch executes an agent using the claude-code runtime.
func (r *ClaudeRuntime) Launch(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (*Result, error) {
	return launchProcess(ctx, "claude-code", agent, task, findClaudeSessionPath)
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

	claudeProjectsDir := filepath.Join(home, ".claude", "projects")
	if _, err := os.Stat(claudeProjectsDir); os.IsNotExist(err) {
		return ""
	}

	entries, err := os.ReadDir(claudeProjectsDir)
	if err != nil {
		return ""
	}

	base := filepath.Base(absRepo)
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() == base {
			return filepath.Join(claudeProjectsDir, entry.Name())
		}
	}

	return ""
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
