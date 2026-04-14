package launcher

import (
	"context"
	"fmt"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/config"
)

// Launcher dispatches agent execution to the appropriate Runtime implementation.
type Launcher struct {
	runtimes map[string]Runtime
}

// NewLauncher creates a Launcher with built-in runtimes registered.
func NewLauncher() *Launcher {
	l := &Launcher{
		runtimes: make(map[string]Runtime),
	}
	l.Register(&ClaudeRuntime{})
	l.Register(&CodexRuntime{})
	return l
}

// Register adds a Runtime implementation to the launcher.
func (l *Launcher) Register(rt Runtime) {
	l.runtimes[rt.Name()] = rt
}

// Launch selects the appropriate runtime and executes the agent.
func (l *Launcher) Launch(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (*Result, error) {
	runtimeName := agent.Runtime
	if runtimeName == "" {
		runtimeName = "claude-code" // default runtime
	}

	rt, ok := l.runtimes[runtimeName]
	if !ok {
		supported := l.supportedRuntimes()
		return nil, fmt.Errorf("launcher: unsupported runtime: %s, supported: %s", runtimeName, supported)
	}

	return rt.Launch(ctx, agent, task)
}

// supportedRuntimes returns a comma-separated list of registered runtime names.
func (l *Launcher) supportedRuntimes() string {
	names := make([]string, 0, len(l.runtimes))
	for name := range l.runtimes {
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}
