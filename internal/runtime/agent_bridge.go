package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// AgentBridgeRuntime is the shared host-exec core that bridges a workbuddy
// dispatch onto an agent.Backend subprocess: it owns the lazily-cached
// backend, the spec assembly, the event pump, and the base Result assembly.
// codex (and claude-shot) use it directly as self-managed host-exec peers.
// The agentm runtime (see agentm_runtime.go) composes this core and layers
// its sandbox-specific behavior — structured RESULT extraction,
// coordinator-side label writes, and the control loop — on top, so no
// `if runtime == agentm` branch lives in this file.
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

// Capabilities reports the host-exec contract shared by the codex/claude
// bridge runtimes: the agent runs as a host subprocess (not sandboxed),
// self-manages its labels via `gh issue edit`, and needs host gh/git
// credentials. The agentm runtime overrides this (see AgentMRuntime).
func (r *AgentBridgeRuntime) Capabilities() Capabilities {
	return Capabilities{Sandboxed: false, ManagesOwnLabels: true, NeedsHostGHCreds: true}
}

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
	spec := r.baseSpec(ctx, agentCfg, task, prompt, "")
	return r.newBridgeSession(ctx, agentCfg, task, spec)
}

// baseSpec assembles the agent.Spec fields shared by every bridge runtime.
// resumeSessionID, when non-empty, asks the backend to resume instead of
// starting a fresh session.
func (r *AgentBridgeRuntime) baseSpec(ctx context.Context, agentCfg *config.AgentConfig, task *TaskContext, prompt, resumeSessionID string) agent.Spec {
	return agent.Spec{
		Backend:         agentCfg.Runtime,
		Workdir:         task.WorkDir,
		Prompt:          prompt,
		Args:            rolloutInvocationArgs(task),
		Model:           agentCfg.Policy.Model,
		Sandbox:         agentCfg.Policy.Sandbox,
		Approval:        agentCfg.Policy.Approval,
		Env:             injectTraceContext(ctx, envSliceToMap(BuildScopedEnv(agentCfg, task)), task),
		ResumeSessionID: resumeSessionID,
		Tags: map[string]string{
			"agent": agentCfg.Name,
			"repo":  task.Repo,
		},
	}
}

// newBridgeSession boots the backend session for spec and wraps it in a
// generic AgentBridgeSession.
func (r *AgentBridgeRuntime) newBridgeSession(ctx context.Context, agentCfg *config.AgentConfig, task *TaskContext, spec agent.Spec) (*AgentBridgeSession, error) {
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
	return launchViaStart(ctx, r, agentCfg, task)
}

// launchViaStart is the generic one-shot Launch: Start a session, drain its
// events, and return the Result. Shared by the bridge core and the agentm
// runtime's direct-launch path.
func launchViaStart(ctx context.Context, rt Runtime, agentCfg *config.AgentConfig, task *TaskContext) (*Result, error) {
	sess, err := rt.Start(ctx, agentCfg, task)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()
	return drainSession(ctx, sess)
}

// drainSession runs a session with a discard event sink and returns its
// Result.
func drainSession(ctx context.Context, sess Session) (*Result, error) {
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

// AgentBridgeSession is the generic bridge session: it pumps backend events
// into the launcher event stream (and the durable stdout handle) and
// assembles the base Result. It carries no agentm-specific behavior; the
// agentm runtime wraps it (see AgentMSession).
type AgentBridgeSession struct {
	Session  agent.Session
	Handle   BridgeSessionHandle
	AgentCfg *config.AgentConfig
	Task     *TaskContext
}

func (s *AgentBridgeSession) Run(ctx context.Context, events chan<- launcherevents.Event) (*Result, error) {
	result, _, err := s.runCore(ctx, events)
	return result, err
}

// runCore pumps the backend event stream and assembles the base Result. It
// returns the raw agent.Result alongside so composing sessions (agentm) can
// layer additional Meta without re-running the pump. This is the shared
// machinery the ADR (§1b) requires both runtimes to compose rather than
// duplicate.
func (s *AgentBridgeSession) runCore(ctx context.Context, events chan<- launcherevents.Event) (*Result, agent.Result, error) {
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

	result := &Result{
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
	}
	return result, agentResult, err
}

func (s *AgentBridgeSession) SetApprover(Approver) error { return ErrNotSupported }

func (s *AgentBridgeSession) Close() error {
	return s.Session.Close()
}

// resolvePrompt renders the agent prompt for self-managed host-exec runtimes
// (claude / codex): the transition footer carrying the `gh issue edit`
// routing instructions is appended so the agent can self-route. The agentm
// runtime suppresses that footer (see resolveAgentMPrompt).
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

// copyAgentMSessionLog copies the agentm-native session log out of the
// backend's per-session temp dir (which the backend deletes on Close) into
// the durable worker session dir, alongside stdout. It returns the path to
// the copy. An empty handle, an empty source path, or an unreadable source
// yields ("", err) and the caller keeps the original path.
func copyAgentMSessionLog(handle BridgeSessionHandle, srcPath string) (string, error) {
	if handle == nil || srcPath == "" {
		return "", nil
	}
	stdoutPath := handle.StdoutPath()
	if stdoutPath == "" {
		return "", nil
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(filepath.Dir(stdoutPath), "agentm-session.jsonl")
	if dst == srcPath {
		return dst, nil
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return "", err
	}
	return dst, nil
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
