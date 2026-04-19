package launcher

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/agent"
	"github.com/Lincyaw/workbuddy/internal/agent/claude"
	"github.com/Lincyaw/workbuddy/internal/agent/codex"
	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

// newBackendFromConfig returns an agent.Backend for the given runtime name,
// or nil if the caller should fall through to the existing launcher runtime.
func newBackendFromConfig(runtimeName string) (agent.Backend, error) {
	switch runtimeName {
	case config.RuntimeCodex, config.RuntimeCodexExec:
		if os.Getenv("WORKBUDDY_CODEX_BACKEND") == "mcp-server" {
			return codex.NewBackend(codex.Config{})
		}
		return nil, nil // fall through to existing launcher
	case config.RuntimeClaudeCode, config.RuntimeClaudeShot:
		return claude.NewBackend(), nil
	default:
		return nil, fmt.Errorf("agent: unsupported runtime %q", runtimeName)
	}
}

// agentBridgeRuntime implements launcher.Runtime by delegating to agent.Backend.
type agentBridgeRuntime struct {
	backend     agent.Backend
	runtimeName string
}

func (r *agentBridgeRuntime) Name() string { return r.runtimeName }

func (r *agentBridgeRuntime) Start(ctx context.Context, agentCfg *config.AgentConfig, task *TaskContext) (Session, error) {
	prompt := resolvePrompt(agentCfg, task)
	spec := agent.Spec{
		Backend: agentCfg.Runtime,
		Workdir: task.WorkDir,
		Prompt:  prompt,
		Model:   agentCfg.Policy.Model,
		Sandbox: agentCfg.Policy.Sandbox,
		Env:     make(map[string]string),
		Tags: map[string]string{
			"agent": agentCfg.Name,
			"repo":  task.Repo,
		},
	}

	sess, err := r.backend.NewSession(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("launcher: agent bridge: %w", err)
	}

	var handle agent.SessionHandle
	if task.SessionHandle() != nil {
		handle = task.SessionHandle()
	}

	return &agentBridgeSession{
		bridge: &agent.BridgeSession{
			Sess:   sess,
			Handle: handle,
		},
	}, nil
}

func (r *agentBridgeRuntime) Launch(ctx context.Context, agentCfg *config.AgentConfig, task *TaskContext) (*Result, error) {
	sess, err := r.Start(ctx, agentCfg, task)
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

// agentBridgeSession wraps agent.BridgeSession to implement launcher.Session.
type agentBridgeSession struct {
	bridge *agent.BridgeSession
}

func (s *agentBridgeSession) Run(ctx context.Context, events chan<- launcherevents.Event) (*Result, error) {
	br, err := s.bridge.Run(ctx, events)
	if br == nil {
		return nil, err
	}
	return &Result{
		ExitCode:    br.ExitCode,
		Duration:    br.Duration,
		LastMessage: br.LastMessage,
		Meta:        br.Meta,
	}, err
}

func (s *agentBridgeSession) SetApprover(Approver) error { return ErrNotSupported }

func (s *agentBridgeSession) Close() error {
	return s.bridge.Close()
}

// resolvePrompt extracts the prompt text from the agent config.
func resolvePrompt(agentCfg *config.AgentConfig, task *TaskContext) string {
	if p := strings.TrimSpace(agentCfg.Prompt); p != "" {
		rendered, err := renderCommandRaw(p, task)
		if err == nil {
			return rendered
		}
		return p
	}
	if cmd := strings.TrimSpace(agentCfg.Command); cmd != "" {
		rendered, err := renderCommandRaw(cmd, task)
		if err == nil {
			return rendered
		}
		return cmd
	}
	return ""
}
