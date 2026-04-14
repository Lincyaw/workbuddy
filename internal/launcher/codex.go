package launcher

import (
	"context"
	"os"
	"path/filepath"

	"github.com/Lincyaw/workbuddy/internal/config"
)

// CodexRuntime implements Runtime for codex (codex exec mode).
type CodexRuntime struct{}

// Name returns the runtime identifier.
func (r *CodexRuntime) Name() string { return "codex" }

// Launch executes an agent using the codex runtime.
func (r *CodexRuntime) Launch(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (*Result, error) {
	return launchProcess(ctx, "codex", agent, task, findCodexLogPath)
}

// findCodexLogPath attempts to locate the codex output log directory.
func findCodexLogPath(repoPath string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

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
