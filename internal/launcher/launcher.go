package launcher

import (
	"context"

	"github.com/Lincyaw/workbuddy/internal/agent"
	"github.com/Lincyaw/workbuddy/internal/agent/codex"
	"github.com/Lincyaw/workbuddy/internal/config"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	supclient "github.com/Lincyaw/workbuddy/internal/supervisor/client"
)

// Launcher is kept as a compatibility alias while runtime owns the canonical registry.
type Launcher = runtimepkg.Registry

// NewLauncher builds a Registry pre-populated with the workbuddy built-in
// runtimes, wired to the supplied supervisor client. The hook is fired
// immediately after each successful StartAgent (taskID, agentID); workers use
// it to persist task_queue.supervisor_agent_id so a worker restart can adopt
// the running subprocess instead of orphaning it. Either argument may be nil
// only in unit tests that exercise stubbed runtimes.
func NewLauncher(client *supclient.Client, hook runtimepkg.AgentStartedHook) *Launcher {
	l := runtimepkg.NewRegistry()
	l.SetSupervisorClient(client)
	l.SetAgentStartedHook(hook)
	l.SetSessionStarter(func(_ context.Context, agent *config.AgentConfig, task *runtimepkg.TaskContext) (runtimepkg.Session, error, bool) {
		if agent != nil && agent.Runner == config.RunnerGitHubActions {
			return runtimepkg.NewGHASession(agent, task), nil, true
		}
		return nil, nil, false
	})
	RegisterBuiltins(l)
	return l
}

// RegisterBuiltins installs the built-in runtimes into a registry. The
// supervisor client and agent-started hook are pulled off the registry, so
// callers should call SetSupervisorClient / SetAgentStartedHook first.
func RegisterBuiltins(l *runtimepkg.Registry) {
	if l == nil {
		return
	}
	l.Register(&runtimepkg.ClaudeRuntime{}, config.RuntimeClaudeCode, config.RuntimeClaudeShot)
	l.Register(newAgentBridgeRuntime(config.RuntimeCodex, func() (agent.Backend, error) {
		return codex.NewBackend(codex.Config{})
	}), config.RuntimeCodex, config.RuntimeCodexServer)
}
