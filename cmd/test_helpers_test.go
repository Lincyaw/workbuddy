package cmd

import (
	"context"
	"sync"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

type mockRuntime struct {
	name     string
	mu       sync.Mutex
	calls    int
	countFor string
	resultFn func(ctx context.Context, agent *config.AgentConfig, task *launcher.TaskContext) (*launcher.Result, error)
}

func (m *mockRuntime) Name() string { return m.name }

type mockSession struct {
	rt    *mockRuntime
	agent *config.AgentConfig
	task  *launcher.TaskContext
}

func (s *mockSession) Run(ctx context.Context, _ chan<- launcherevents.Event) (*launcher.Result, error) {
	return s.rt.Launch(ctx, s.agent, s.task)
}

func (s *mockSession) SetApprover(launcher.Approver) error { return launcher.ErrNotSupported }
func (s *mockSession) Close() error                        { return nil }

func (m *mockRuntime) Start(ctx context.Context, agent *config.AgentConfig, task *launcher.TaskContext) (launcher.Session, error) {
	return &mockSession{rt: m, agent: agent, task: task}, nil
}

func (m *mockRuntime) Launch(ctx context.Context, agent *config.AgentConfig, task *launcher.TaskContext) (*launcher.Result, error) {
	m.mu.Lock()
	if m.countFor == "" || (agent != nil && agent.Name == m.countFor) {
		m.calls++
	}
	fn := m.resultFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, agent, task)
	}
	return &launcher.Result{ExitCode: 0, Stdout: "mock output"}, nil
}

func (m *mockRuntime) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

type mockCommentWriter struct {
	mu       sync.Mutex
	comments []string
}

func (m *mockCommentWriter) WriteComment(_ string, _ int, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.comments = append(m.comments, body)
	return nil
}

func (m *mockCommentWriter) Comments() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.comments...)
}
