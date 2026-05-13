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
	// GitOps, when non-nil, is invoked by the AgentM bridge after a
	// successful run with a non-empty artifact: the coordinator commits
	// the artifact to a `workbuddy/issue-N` branch, pushes, and opens a
	// PR. v0.6 coordinator-managed dispatch per
	// docs/decisions/2026-05-13-k8s-agentm-otel.md (Block 2). Other
	// runtimes (claude-code, codex) remain self-managed; this hook is
	// only consulted when the underlying session is AgentM.
	GitOps AgentMGitOps
}

// AgentMGitOps is the bridge between the runtime package and
// internal/gitops. Defined locally so runtime stays a leaf-of-leaves;
// production wiring constructs an adapter that satisfies this interface
// around a *gitops.Client.
type AgentMGitOps interface {
	// PublishArtifact commits whatever is staged in req.RepoLocalPath
	// (the AgentM workspace) onto req.Branch, pushes, and opens a PR.
	// Returns the PR URL on success. An ErrNoChangesToPublish return
	// means the agent produced no diff; the caller MUST treat that as
	// a no-op publish, not a failure.
	PublishArtifact(ctx context.Context, req AgentMPublishRequest) (prURL string, err error)
}

// ErrNoChangesToPublish signals that an AgentMGitOps.PublishArtifact call
// found no working-tree changes — the agent declared success but its
// workspace is identical to the base branch. Callers surface this as
// metadata, not as a failure.
var ErrNoChangesToPublish = fmt.Errorf("agent bridge: agentm produced no changes to publish")

// AgentMPublishRequest is the input to AgentMGitOps.PublishArtifact.
type AgentMPublishRequest struct {
	Repo          string
	IssueNumber   int
	IssueTitle    string
	Branch        string
	CommitMessage string
	PRTitle       string
	PRBody        string
	RepoLocalPath string
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
		GitOps:   r.GitOps,
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
	// GitOps, when non-nil and the underlying session is AgentM,
	// publishes the artifact (commit/push/PR) after a successful run.
	GitOps AgentMGitOps
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
			// Coordinator-managed publish path (REQ-142 / #330).
			// Only invoked when the run succeeded; failed runs already
			// surface failure_reason for the reporter to comment on.
			if out.Success && s.GitOps != nil && s.Task != nil {
				prURL, pubMeta, pubErr := s.publishAgentMArtifact(ctx, out)
				for k, v := range pubMeta {
					meta[k] = v
				}
				if pubErr != nil {
					// Publish failure is infra failure: AgentM did its
					// job, but the coordinator couldn't ship the diff.
					meta[MetaInfraFailure] = "true"
					meta[MetaInfraFailureReason] = "agentm publish: " + pubErr.Error()
				} else if prURL != "" {
					meta["pr_url"] = prURL
				}
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

// publishAgentMArtifact is the coordinator-managed commit+push+PR step
// (REQ-142). It runs only when the underlying session is AgentM and the
// bridge runtime has a GitOps adapter configured.
func (s *AgentBridgeSession) publishAgentMArtifact(ctx context.Context, out *agentm.Output) (string, map[string]string, error) {
	if s.Task == nil {
		return "", nil, nil
	}
	repo := s.Task.Repo
	issueNum := s.Task.Issue.Number
	if repo == "" || issueNum <= 0 {
		return "", nil, nil
	}
	workdir := s.Task.WorkDir
	if workdir == "" {
		workdir = s.Task.RepoRoot
	}
	if workdir == "" {
		return "", nil, fmt.Errorf("no workdir on task context")
	}

	branch := fmt.Sprintf("workbuddy/issue-%d", issueNum)
	commitMsg := fmt.Sprintf("workbuddy(agentm): resolve issue #%d", issueNum)
	title := s.Task.Issue.Title
	if title == "" {
		title = fmt.Sprintf("workbuddy: resolve issue #%d", issueNum)
	} else {
		title = fmt.Sprintf("workbuddy: %s", title)
	}
	body := buildAgentMPRBody(repo, issueNum, out)

	req := AgentMPublishRequest{
		Repo:          repo,
		IssueNumber:   issueNum,
		IssueTitle:    s.Task.Issue.Title,
		Branch:        branch,
		CommitMessage: commitMsg,
		PRTitle:       title,
		PRBody:        body,
		RepoLocalPath: workdir,
	}

	prURL, err := s.GitOps.PublishArtifact(ctx, req)
	meta := map[string]string{}
	if err != nil {
		if isNoChangesErr(err) {
			meta["agentm_publish"] = "no_changes"
			return "", meta, nil
		}
		return "", meta, err
	}
	meta["agentm_publish"] = "published"
	meta["agentm_pr_branch"] = branch
	return prURL, meta, nil
}

func buildAgentMPRBody(repo string, issueNum int, out *agentm.Output) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Resolves %s#%d.\n\n", repo, issueNum)
	b.WriteString("Generated by workbuddy AgentM coordinator-managed dispatch.\n")
	if out != nil && out.NextLabel != "" {
		fmt.Fprintf(&b, "\nAgent next_label: `%s`\n", out.NextLabel)
	}
	if out != nil && out.SessionLogPath != "" {
		fmt.Fprintf(&b, "\nSession log: `%s`\n", out.SessionLogPath)
	}
	return b.String()
}

func isNoChangesErr(err error) bool {
	if err == nil {
		return false
	}
	if err == ErrNoChangesToPublish {
		return true
	}
	return strings.Contains(err.Error(), "no changes to commit") ||
		strings.Contains(err.Error(), "no changes to publish")
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
