package launcher

import (
	"context"

	"github.com/Lincyaw/workbuddy/internal/agent"
	"github.com/Lincyaw/workbuddy/internal/agent/codex"
	"github.com/Lincyaw/workbuddy/internal/config"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
)

// Launcher is kept as a compatibility alias while runtime owns the canonical registry.
type Launcher = runtimepkg.Registry

func NewLauncher() *Launcher {
	l := runtimepkg.NewRegistry()
	l.SetSessionStarter(func(_ context.Context, agent *config.AgentConfig, task *runtimepkg.TaskContext) (runtimepkg.Session, error, bool) {
		if agent != nil && agent.Runner == config.RunnerGitHubActions {
			return newGHASession(agent, task), nil, true
		}
		return nil, nil, false
	})
	RegisterBuiltins(l)
	return l
}

// RegisterBuiltins installs the built-in runtimes into a registry.
func RegisterBuiltins(l *runtimepkg.Registry) {
	if l == nil {
		return
	}
	l.Register(&ClaudeRuntime{}, config.RuntimeClaudeCode, config.RuntimeClaudeShot)
	l.Register(&agentBridgeRuntime{
		runtimeName: config.RuntimeCodex,
		newBackend: func() (agent.Backend, error) {
			return codex.NewBackend(codex.Config{})
		},
	}, config.RuntimeCodex, config.RuntimeCodexServer)
}
