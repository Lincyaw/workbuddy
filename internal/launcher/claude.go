// Package launcher provides multi-runtime agent execution backends.
package launcher

import (
	"context"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Lincyaw/workbuddy/internal/config"
)

type ClaudeRuntime struct{}

func (r *ClaudeRuntime) Name() string { return config.RuntimeClaudeShot }

func (r *ClaudeRuntime) Start(_ context.Context, agent *config.AgentConfig, task *TaskContext) (Session, error) {
	return newProcessSession(r.Name(), agent, task, findClaudeSessionPath), nil
}

func (r *ClaudeRuntime) Launch(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (*Result, error) {
	sess, err := r.Start(ctx, agent, task)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()
	return sess.Run(ctx, nil)
}

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

func buildEnvVars(task *TaskContext) []string {
	return []string{
		"WORKBUDDY_ISSUE_NUMBER=" + strconv.Itoa(task.Issue.Number),
		"WORKBUDDY_ISSUE_TITLE=" + task.Issue.Title,
		"WORKBUDDY_ISSUE_BODY=" + task.Issue.Body,
		"WORKBUDDY_REPO=" + task.Repo,
		"WORKBUDDY_SESSION_ID=" + task.Session.ID,
	}
}
