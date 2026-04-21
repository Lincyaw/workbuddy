package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

func TestRegistryStartUsesRegisteredRuntime(t *testing.T) {
	t.Parallel()

	called := false
	reg := NewRegistry()
	reg.Register(&testRuntime{
		name: config.RuntimeCodex,
		start: func(_ context.Context, agent *config.AgentConfig, task *TaskContext) (Session, error) {
			called = true
			if agent.Name != "dev-agent" {
				t.Fatalf("agent = %q, want dev-agent", agent.Name)
			}
			if task.Repo != "owner/repo" {
				t.Fatalf("task repo = %q, want owner/repo", task.Repo)
			}
			return &testSession{}, nil
		},
	})

	sess, err := reg.Start(context.Background(), &config.AgentConfig{Name: "dev-agent", Runtime: config.RuntimeCodex}, &TaskContext{Repo: "owner/repo"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !called {
		t.Fatal("registered runtime was not invoked")
	}
	if sess == nil {
		t.Fatal("session = nil, want non-nil")
	}
}

func TestRegistryStartReportsSupportedRuntimes(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(&testRuntime{name: config.RuntimeCodex})
	_, err := reg.Start(context.Background(), &config.AgentConfig{Name: "dev-agent", Runtime: "missing"}, &TaskContext{})
	if err == nil {
		t.Fatal("Start error = nil, want unsupported runtime")
	}
	if !strings.Contains(err.Error(), config.RuntimeCodex) {
		t.Fatalf("error = %q, want supported runtime list", err)
	}
}

func TestRegistryStartUsesSpecialStarter(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	want := errors.New("special")
	reg.SetSessionStarter(func(_ context.Context, _ *config.AgentConfig, _ *TaskContext) (Session, error, bool) {
		return nil, want, true
	})

	_, err := reg.Start(context.Background(), &config.AgentConfig{Name: "dev-agent", Runtime: config.RuntimeCodex}, &TaskContext{})
	if !errors.Is(err, want) {
		t.Fatalf("Start error = %v, want %v", err, want)
	}
}

type testRuntime struct {
	name  string
	start func(context.Context, *config.AgentConfig, *TaskContext) (Session, error)
}

func (t *testRuntime) Name() string { return t.name }

func (t *testRuntime) Start(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (Session, error) {
	if t.start != nil {
		return t.start(ctx, agent, task)
	}
	return &testSession{}, nil
}

func (t *testRuntime) Launch(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (*Result, error) {
	sess, err := t.Start(ctx, agent, task)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()
	return sess.Run(ctx, nil)
}

type testSession struct{}

func (testSession) Run(context.Context, chan<- launcherevents.Event) (*Result, error) {
	return &Result{ExitCode: 0}, nil
}

func (testSession) SetApprover(Approver) error { return nil }
func (testSession) Close() error               { return nil }
