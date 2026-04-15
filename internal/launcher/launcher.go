package launcher

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

// Launcher dispatches agent execution to the appropriate Runtime implementation.
type Launcher struct {
	runtimes map[string]Runtime
}

func NewLauncher() *Launcher {
	l := &Launcher{runtimes: make(map[string]Runtime)}
	l.Register(&ClaudeRuntime{}, config.RuntimeClaudeCode, config.RuntimeClaudeShot)
	l.Register(&CodexRuntime{}, config.RuntimeCodex, config.RuntimeCodexExec)
	return l
}

func (l *Launcher) Register(rt Runtime, aliases ...string) {
	l.runtimes[rt.Name()] = rt
	for _, alias := range aliases {
		l.runtimes[alias] = rt
	}
}

func (l *Launcher) Start(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (Session, error) {
	runtimeName := agent.Runtime
	if runtimeName == "" {
		runtimeName = config.RuntimeClaudeCode
	}
	rt, ok := l.runtimes[runtimeName]
	if !ok {
		return nil, fmt.Errorf("launcher: unsupported runtime: %s, supported: %s", runtimeName, l.supportedRuntimes())
	}
	return rt.Start(ctx, agent, task)
}

func (l *Launcher) Launch(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (*Result, error) {
	sess, err := l.Start(ctx, agent, task)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()

	ch := make(chan launcherevents.Event, 32)
	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()
	result, runErr := sess.Run(ctx, ch)
	close(ch)
	<-done
	return result, runErr
}

func (l *Launcher) supportedRuntimes() string {
	seen := map[string]bool{}
	var names []string
	for name := range l.runtimes {
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
