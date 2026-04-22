package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

// Registry dispatches agent execution to the appropriate Runtime implementation.
type Registry struct {
	runtimes map[string]Runtime
	manager  *SessionManager
	starter  func(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (Session, error, bool)
}

func NewRegistry() *Registry {
	return &Registry{runtimes: make(map[string]Runtime)}
}

func (l *Registry) Register(rt Runtime, aliases ...string) {
	l.runtimes[rt.Name()] = rt
	for _, alias := range aliases {
		l.runtimes[alias] = rt
	}
}

func (l *Registry) SetSessionManager(manager *SessionManager) {
	l.manager = manager
}

func (l *Registry) SetSessionStarter(starter func(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (Session, error, bool)) {
	l.starter = starter
}

func (l *Registry) Start(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (Session, error) {
	if l.starter != nil {
		if session, err, ok := l.starter(ctx, agent, task); ok {
			return session, err
		}
	}
	runtimeName := agent.Runtime
	if runtimeName == "" {
		runtimeName = config.RuntimeClaudeCode
	}
	if l.manager != nil && task != nil && task.SessionHandle() == nil {
		handle, err := l.manager.Create(SessionCreateInput{
			SessionID: task.Session.ID,
			TaskID:    task.Session.TaskID,
			Repo:      task.Repo,
			IssueNum:  task.Issue.Number,
			AgentName: agent.Name,
			Runtime:   runtimeName,
			WorkerID:  task.Session.WorkerID,
			Attempt:   task.Session.Attempt,
		})
		if err != nil {
			return nil, err
		}
		task.SetSessionHandle(handle)
	}
	rt, ok := l.runtimes[runtimeName]
	if !ok {
		return nil, fmt.Errorf("runtime: unsupported runtime: %s, supported: %s", runtimeName, l.supportedRuntimes())
	}
	return rt.Start(ctx, agent, task)
}

func (l *Registry) Launch(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (*Result, error) {
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

// Shutdown tears down any long-lived resources held by registered runtimes
// (for example, shared agent-backend processes such as the codex
// `app-server`). Runtimes that don't hold such state are no-ops.
func (l *Registry) Shutdown(ctx context.Context) error {
	var firstErr error
	seen := map[Runtime]struct{}{}
	for _, rt := range l.runtimes {
		if _, ok := seen[rt]; ok {
			continue
		}
		seen[rt] = struct{}{}
		closer, ok := rt.(interface {
			Shutdown(ctx context.Context) error
		})
		if !ok {
			continue
		}
		if err := closer.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (l *Registry) supportedRuntimes() string {
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
