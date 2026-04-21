package launcher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Lincyaw/workbuddy/internal/agent"
	"github.com/Lincyaw/workbuddy/internal/agent/claude"
	"github.com/Lincyaw/workbuddy/internal/agent/codex"
	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

// newBackendFromConfig returns an agent.Backend for the given runtime name,
// or an error when the runtime is unsupported.
func newBackendFromConfig(runtimeName string) (agent.Backend, error) {
	switch runtimeName {
	case config.RuntimeCodex, config.RuntimeCodexServer:
		return codex.NewBackend(codex.Config{})
	case config.RuntimeClaudeCode, config.RuntimeClaudeShot:
		return claude.NewBackend(), nil
	default:
		return nil, fmt.Errorf("agent: unsupported runtime %q", runtimeName)
	}
}

// agentBridgeRuntime implements launcher.Runtime by delegating to agent.Backend.
type agentBridgeRuntime struct {
	backend     agent.Backend
	backendMu   sync.Mutex
	newBackend  func() (agent.Backend, error)
	runtimeName string
}

func (r *agentBridgeRuntime) Name() string { return r.runtimeName }

func (r *agentBridgeRuntime) backendInstance() (agent.Backend, error) {
	r.backendMu.Lock()
	defer r.backendMu.Unlock()
	if r.backend != nil {
		return r.backend, nil
	}
	if r.newBackend == nil {
		return nil, fmt.Errorf("launcher: agent bridge %q missing backend factory", r.runtimeName)
	}
	backend, err := r.newBackend()
	if err != nil {
		return nil, err
	}
	r.backend = backend
	return backend, nil
}

func (r *agentBridgeRuntime) Start(ctx context.Context, agentCfg *config.AgentConfig, task *TaskContext) (Session, error) {
	prompt := resolvePrompt(agentCfg, task)
	spec := agent.Spec{
		Backend:  agentCfg.Runtime,
		Workdir:  task.WorkDir,
		Prompt:   prompt,
		Model:    agentCfg.Policy.Model,
		Sandbox:  agentCfg.Policy.Sandbox,
		Approval: agentCfg.Policy.Approval,
		Env:      envSliceToMap(buildScopedEnv(agentCfg, task)),
		Tags: map[string]string{
			"agent": agentCfg.Name,
			"repo":  task.Repo,
		},
	}

	backend, err := r.backendInstance()
	if err != nil {
		return nil, fmt.Errorf("launcher: agent bridge backend init: %w", err)
	}

	sess, err := backend.NewSession(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("launcher: agent bridge: %w", err)
	}

	var handle bridgeSessionHandle
	if task.SessionHandle() != nil {
		handle = task.SessionHandle()
	}

	return &agentBridgeSession{
		session:  sess,
		handle:   handle,
		agentCfg: agentCfg,
		task:     task,
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

type agentBridgeSession struct {
	session  agent.Session
	handle   bridgeSessionHandle
	agentCfg *config.AgentConfig
	task     *TaskContext
}

func (s *agentBridgeSession) Run(ctx context.Context, events chan<- launcherevents.Event) (*Result, error) {
	var seq uint64
	sessionID := ""
	if s.task != nil {
		sessionID = s.task.Session.ID
	}
	if sessionID == "" && s.session != nil {
		sessionID = s.session.ID()
	}
	if events != nil {
		emitPermissionEvent(events, &seq, sessionID, sessionID, s.agentCfg)
	}

	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for evt := range s.session.Events() {
			raw := evt.Raw
			if len(raw) == 0 {
				raw = evt.Body
			}
			if s.handle != nil && len(raw) > 0 {
				line := append(append([]byte(nil), raw...), '\n')
				_ = s.handle.WriteStdout(line)
			}
			if events == nil {
				continue
			}

			kind := translateAgentEventKind(evt.Kind)
			if kind == "" {
				continue
			}
			body := evt.Body
			if len(body) == 0 {
				body = json.RawMessage("{}")
			}
			if len(raw) == 0 {
				raw = body
			}
			turnID := evt.TurnID
			if turnID == "" {
				turnID = sessionID
			}

			seq++
			translated := launcherevents.Event{
				Kind:      kind,
				Timestamp: time.Now().UTC(),
				SessionID: sessionID,
				TurnID:    turnID,
				Seq:       seq,
				Payload:   body,
				Raw:       raw,
			}
			select {
			case events <- translated:
			case <-ctx.Done():
				return
			}
		}
	}()

	agentResult, err := s.session.Wait(ctx)
	if err != nil && ctx.Err() != nil {
		_ = s.session.Close()
	}
	<-pumpDone

	var meta map[string]string
	if len(agentResult.FilesChanged) > 0 {
		meta = map[string]string{
			"files_changed": strings.Join(agentResult.FilesChanged, ","),
		}
	}
	sessionPath := bridgeSessionPath(s.handle)
	return &Result{
		ExitCode:       agentResult.ExitCode,
		Duration:       agentResult.Duration,
		LastMessage:    agentResult.FinalMsg,
		Meta:           meta,
		SessionPath:    sessionPath,
		RawSessionPath: sessionPath,
		SessionRef: SessionRef{
			ID:   agentResult.SessionRef.ID,
			Kind: agentResult.SessionRef.Kind,
		},
	}, err
}

func (s *agentBridgeSession) SetApprover(Approver) error { return ErrNotSupported }

func (s *agentBridgeSession) Close() error {
	return s.session.Close()
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

func envSliceToMap(entries []string) map[string]string {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 || parts[0] == "" {
			continue
		}
		out[parts[0]] = parts[1]
	}
	return out
}

type bridgeSessionHandle interface {
	WriteStdout([]byte) error
	StdoutPath() string
}

func bridgeSessionPath(handle bridgeSessionHandle) string {
	if handle == nil {
		return ""
	}
	return handle.StdoutPath()
}

func translateAgentEventKind(kind string) launcherevents.EventKind {
	switch kind {
	case "turn.started":
		return launcherevents.KindTurnStarted
	case "turn.completed":
		return launcherevents.KindTurnCompleted
	case "agent.message":
		return launcherevents.KindAgentMessage
	case "tool.call":
		return launcherevents.KindToolCall
	case "tool.result":
		return launcherevents.KindToolResult
	case "error":
		return launcherevents.KindError
	case "reasoning":
		return launcherevents.KindReasoning
	case "command.exec":
		return launcherevents.KindCommandExec
	case "command.output":
		return launcherevents.KindCommandOutput
	case "file.change":
		return launcherevents.KindFileChange
	case "token.usage":
		return launcherevents.KindTokenUsage
	case "task.complete":
		return launcherevents.KindTaskComplete
	case "log":
		return launcherevents.KindLog
	case "internal":
		return ""
	default:
		return launcherevents.KindLog
	}
}
