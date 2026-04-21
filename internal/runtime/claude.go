package runtime

import (
	"context"
	"os"
	"path/filepath"

	"github.com/Lincyaw/workbuddy/internal/config"
)

type ClaudeRuntime struct{}

func (r *ClaudeRuntime) Name() string { return config.RuntimeClaudeShot }

func (r *ClaudeRuntime) Start(_ context.Context, agent *config.AgentConfig, task *TaskContext) (Session, error) {
	return NewProcessSession(r.Name(), agent, task, FindClaudeSessionPath), nil
}

func (r *ClaudeRuntime) Launch(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (*Result, error) {
	sess, err := r.Start(ctx, agent, task)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()
	return sess.Run(ctx, nil)
}

func FindClaudeSessionPath(repoPath string) string {
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
