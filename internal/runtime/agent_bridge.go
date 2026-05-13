package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Lincyaw/workbuddy/internal/agent"
	"github.com/Lincyaw/workbuddy/internal/agent/agentm"
	"github.com/Lincyaw/workbuddy/internal/agent/claude"
	"github.com/Lincyaw/workbuddy/internal/agent/codex"
	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

func NewBackendFromConfig(runtimeName string) (agent.Backend, error) {
	switch runtimeName {
	case config.RuntimeCodex, config.RuntimeCodexServer:
		return codex.NewBackend(codex.Config{})
	case config.RuntimeClaudeCode, config.RuntimeClaudeShot:
		return claude.NewBackend(), nil
	case config.RuntimeAgentM:
		return agentm.NewBackend(), nil
	default:
		return nil, fmt.Errorf("agent: unsupported runtime %q", runtimeName)
	}
}

type AgentBridgeRuntime struct {
	Backend     agent.Backend
	BackendMu   sync.Mutex
	NewBackend  func() (agent.Backend, error)
	RuntimeName string
}

func NewAgentBridgeRuntime(runtimeName string, factory func() (agent.Backend, error)) *AgentBridgeRuntime {
	return &AgentBridgeRuntime{RuntimeName: runtimeName, NewBackend: factory}
}

func (r *AgentBridgeRuntime) Name() string { return r.RuntimeName }

// Shutdown stops the lazily-cached agent backend (and, for codex, its shared
// app-server child process). Safe to call multiple times and from the
// worker/coordinator shutdown path regardless of whether any session was
// ever created.
func (r *AgentBridgeRuntime) Shutdown(ctx context.Context) error {
	r.BackendMu.Lock()
	backend := r.Backend
	r.Backend = nil
	r.BackendMu.Unlock()
	if backend == nil {
		return nil
	}
	return backend.Shutdown(ctx)
}

func (r *AgentBridgeRuntime) backendInstance() (agent.Backend, error) {
	r.BackendMu.Lock()
	defer r.BackendMu.Unlock()
	if r.Backend != nil {
		return r.Backend, nil
	}
	if r.NewBackend == nil {
		return nil, fmt.Errorf("runtime: agent bridge %q missing backend factory", r.RuntimeName)
	}
	backend, err := r.NewBackend()
	if err != nil {
		return nil, err
	}
	r.Backend = backend
	return backend, nil
}

func (r *AgentBridgeRuntime) Start(ctx context.Context, agentCfg *config.AgentConfig, task *TaskContext) (Session, error) {
	prompt := resolvePrompt(agentCfg, task)
	spec := agent.Spec{
		Backend:  agentCfg.Runtime,
		Workdir:  task.WorkDir,
		Prompt:   prompt,
		Args:     rolloutInvocationArgs(task),
		Model:    agentCfg.Policy.Model,
		Sandbox:  agentCfg.Policy.Sandbox,
		Approval: agentCfg.Policy.Approval,
		Env:      injectAgentMEnv(agentCfg, injectTraceContext(ctx, envSliceToMap(BuildScopedEnv(agentCfg, task)), task)),
		Tags: map[string]string{
			"agent": agentCfg.Name,
			"repo":  task.Repo,
		},
	}

	backend, err := r.backendInstance()
	if err != nil {
		return nil, fmt.Errorf("runtime: agent bridge backend init: %w", err)
	}

	sess, err := backend.NewSession(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("runtime: agent bridge: %w", err)
	}

	var handle BridgeSessionHandle
	if task.SessionHandle() != nil {
		handle = task.SessionHandle()
	}

	return &AgentBridgeSession{
		Session:  sess,
		Handle:   handle,
		AgentCfg: agentCfg,
		Task:     task,
	}, nil
}

func (r *AgentBridgeRuntime) Launch(ctx context.Context, agentCfg *config.AgentConfig, task *TaskContext) (*Result, error) {
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

type BridgeSessionHandle interface {
	WriteStdout([]byte) error
	StdoutPath() string
}

type AgentBridgeSession struct {
	Session  agent.Session
	Handle   BridgeSessionHandle
	AgentCfg *config.AgentConfig
	Task     *TaskContext
}

func (s *AgentBridgeSession) Run(ctx context.Context, events chan<- launcherevents.Event) (*Result, error) {
	var seq uint64
	sessionID := ""
	if s.Task != nil {
		sessionID = s.Task.Session.ID
	}
	if sessionID == "" && s.Session != nil {
		sessionID = s.Session.ID()
	}
	if events != nil {
		EmitPermissionEvent(events, &seq, sessionID, sessionID, s.AgentCfg, emitRuntimeEvent)
	}

	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for evt := range s.Session.Events() {
			raw := evt.Raw
			if len(raw) == 0 {
				raw = evt.Body
			}
			if s.Handle != nil && len(raw) > 0 {
				line := append(append([]byte(nil), raw...), '\n')
				_ = s.Handle.WriteStdout(line)
			}
			if events == nil {
				continue
			}

			kind := TranslateAgentEventKind(evt.Kind)
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

	agentResult, err := s.Session.Wait(ctx)
	if err != nil && ctx.Err() != nil {
		_ = s.Session.Close()
	}
	<-pumpDone

	meta := map[string]string{}
	if len(agentResult.FilesChanged) > 0 {
		meta["files_changed"] = strings.Join(agentResult.FilesChanged, ",")
	}
	sessionPath := bridgeSessionPath(s.Handle)

	// AgentM exposes a structured RESULT: contract. If the underlying
	// session is an AgentM session, surface next_label/failure_reason on
	// the Result so reporter/audit can render it and route the state
	// machine. A malformed/missing RESULT is an infra failure.
	if extractor, ok := s.Session.(interface {
		Output() (*agentm.Output, error)
		SessionLogPath() string
	}); ok {
		if logPath := extractor.SessionLogPath(); logPath != "" {
			sessionPath = logPath
		}
		out, perr := extractor.Output()
		switch {
		case perr != nil:
			// Build a Result we can mark as infra failure; the bridge
			// returns the wait error so the worker treats this as failed.
			meta[MetaInfraFailure] = "true"
			meta[MetaInfraFailureReason] = perr.Error()
		case out != nil:
			meta["agentm_next_label"] = out.NextLabel
			if out.ArtifactPath != "" {
				meta["agentm_artifact_path"] = out.ArtifactPath
			}
			if !out.Success {
				meta["agentm_failure_reason"] = out.FailureReason
			}
		}
	}

	if len(meta) == 0 {
		meta = nil
	}
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

func (s *AgentBridgeSession) SetApprover(Approver) error { return ErrNotSupported }

func (s *AgentBridgeSession) Close() error {
	return s.Session.Close()
}

func resolvePrompt(agentCfg *config.AgentConfig, task *TaskContext) string {
	if p := ResolvePromptBody(agentCfg, task); p != "" {
		rendered, err := RenderAgentPrompt(p, task)
		if err == nil {
			return rendered
		}
		// Fall back to the body without the footer if the combined render
		// fails (parse error in the agent body itself).
		return p
	}
	if cmd := strings.TrimSpace(agentCfg.Command); cmd != "" {
		rendered, err := RenderCommandRaw(cmd, task)
		if err == nil {
			return rendered
		}
		return cmd
	}
	return ""
}

// injectTraceContext adds W3C TraceContext (TRACEPARENT, TRACESTATE) and
// workbuddy run/issue identifiers to the agent env so OTel-aware runtimes
// (today: AgentM) can continue the parent span. Idempotent: if env already
// contains TRACEPARENT, it is preserved.
func injectTraceContext(ctx context.Context, env map[string]string, task *TaskContext) map[string]string {
	if env == nil {
		env = map[string]string{}
	}
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	for k, v := range carrier {
		// MapCarrier keys are lowercase; uppercase them for env-var
		// convention (TRACEPARENT, TRACESTATE, BAGGAGE).
		upper := strings.ToUpper(k)
		if _, exists := env[upper]; !exists {
			env[upper] = v
		}
	}
	if task != nil {
		if _, ok := env["WORKBUDDY_RUN_ID"]; !ok && task.Session.ID != "" {
			env["WORKBUDDY_RUN_ID"] = task.Session.ID
		}
	}
	return env
}

// EnvDevContainerImage is the env var workbuddy injects into the AgentM
// subprocess to tell it which dev container image to dispatch into.
// AgentM owns the actual agent-env Gateway call; workbuddy just forwards
// the agent-config field. See docs/planned/agentm-runtime.md and
// docs/decisions/2026-05-13-k8s-agentm-otel.md (Block 2).
const EnvDevContainerImage = "WORKBUDDY_DEV_CONTAINER_IMAGE"

// injectAgentMEnv adds AgentM-specific env vars derived from the agent
// config. Today that's just dev_container_image → WORKBUDDY_DEV_CONTAINER_IMAGE,
// injected only when runtime=agentm; other runtimes ignore the field
// (and config validation already warned about it).
func injectAgentMEnv(agentCfg *config.AgentConfig, env map[string]string) map[string]string {
	if agentCfg == nil || agentCfg.Runtime != config.RuntimeAgentM {
		return env
	}
	image := strings.TrimSpace(agentCfg.DevContainerImage)
	if image == "" {
		return env
	}
	if env == nil {
		env = map[string]string{}
	}
	if _, exists := env[EnvDevContainerImage]; !exists {
		env[EnvDevContainerImage] = image
	}
	return env
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

func bridgeSessionPath(handle BridgeSessionHandle) string {
	if handle == nil {
		return ""
	}
	return handle.StdoutPath()
}

func TranslateAgentEventKind(kind string) launcherevents.EventKind {
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

func emitRuntimeEvent(ch chan<- launcherevents.Event, seq *uint64, sessionID, turnID string, kind launcherevents.EventKind, payload any, raw []byte) {
	if ch == nil {
		return
	}
	*seq = *seq + 1
	payloadJSON, err := launcherevents.EncodePayload(payload)
	if err != nil {
		payloadJSON = []byte(`{"message":"event payload encode failed"}`)
	}
	var rawMsg []byte
	if len(raw) > 0 {
		rawMsg = append(rawMsg, raw...)
	}
	ch <- launcherevents.Event{Kind: kind, Timestamp: time.Now().UTC(), SessionID: sessionID, TurnID: turnID, Seq: *seq, Payload: payloadJSON, Raw: rawMsg}
}
