package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
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
	"github.com/Lincyaw/workbuddy/internal/control"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"

	"github.com/Lincyaw/workbuddy/internal/tracing"
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
	// LabelWriter, when non-nil, is invoked by the AgentM bridge after a
	// successful run AND a successful GitOps publish to apply the
	// `next_label` value the agent emitted on its structured Result.
	// Coordinator-managed label writes are the v0.6 closing of the
	// AgentM state-machine loop (REQ-146, #332). Self-managed runtimes
	// (claude-code, codex) keep flipping labels themselves via
	// `gh issue edit` from inside the agent subprocess and MUST NOT
	// have this hook wired — the per-runtime gate lives in Run() below.
	LabelWriter AgentMLabelWriter
	// PRMerger, when non-nil, is invoked after the merge-agent applies
	// the merged label. The coordinator squash-merges the PR and deletes
	// the branch. Only fires for AgentM runs whose applied label matches
	// MergedLabel.
	PRMerger AgentMPRMerger
	// ControlObserver, when non-nil, enables the closed-loop control
	// system for AgentM dispatches. After each agent run, the observer
	// checks world state (branch/PR/label/comment) against the role's
	// objective and resumes the agent if post-conditions are not met.
	ControlObserver control.Observer
	// ControlMaxRounds caps the number of observe-resume iterations.
	// Defaults to 5 when zero.
	ControlMaxRounds int
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

// AgentMLabelWriter is the bridge between the runtime package and
// internal/labelwriter. Defined locally so runtime stays a leaf-of-leaves;
// production wiring constructs an adapter that satisfies this interface
// around a *labelwriter.Writer. v0.6 sanctioned exception to the
// "agents own label writes" rule, per docs/decisions/
// 2026-05-13-k8s-agentm-otel.md (Block 2 § Two execution modes) — only
// AgentM runs use this path.
type AgentMLabelWriter interface {
	// ApplyNextLabel adds `label` to issue #issueNum in `repo`. An empty
	// label MUST be a no-op (no error). The implementation is expected
	// to consult the repo registration for host_kind so GitHub and
	// Gitea backends route to the right wire protocol.
	ApplyNextLabel(ctx context.Context, repo string, issueNum int, label string) error
}

// AgentMPRMerger is the bridge between the runtime package and
// internal/gitops for the merge-agent post-label step. When the
// merge-agent returns next_label matching MergedLabel, the coordinator
// squash-merges the PR and deletes the branch.
type AgentMPRMerger interface {
	MergePR(ctx context.Context, repo, branch string) error
}

// MergedLabel is the label value that triggers coordinator-side PR merge
// after the merge-agent approves. Must match the workflow's merged state
// enter_label.
const MergedLabel = "status:merged"

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
	return r.startWithResume(ctx, agentCfg, task, "")
}

// startWithResume is the internal Start implementation. When resumeSessionID
// is non-empty, it is set on the agent.Spec so the agentm backend emits
// --resume <id> instead of starting a fresh session.
func (r *AgentBridgeRuntime) startWithResume(ctx context.Context, agentCfg *config.AgentConfig, task *TaskContext, resumeSessionID string) (Session, error) {
	prompt := resolvePrompt(agentCfg, task)
	// For AgentM dispatches, derive a deterministic TRACEPARENT from the
	// repo+issue so all sessions for the same issue share a trace ID.
	traceCtx := ctx
	if agentCfg.Runtime == config.RuntimeAgentM && task != nil && task.Repo != "" && task.Issue.Number > 0 {
		traceCtx = issueTraceContext(task.Repo, task.Issue.Number)
	}
	spec := agent.Spec{
		Backend:         agentCfg.Runtime,
		Workdir:         task.WorkDir,
		Prompt:          prompt,
		Args:            rolloutInvocationArgs(task),
		Model:           agentCfg.Policy.Model,
		Sandbox:         agentCfg.Policy.Sandbox,
		Approval:        agentCfg.Policy.Approval,
		Env:             injectAgentMEnv(agentCfg, injectTraceContext(traceCtx, envSliceToMap(BuildScopedEnv(agentCfg, task)), task), task),
		ResumeSessionID: resumeSessionID,
		Tags: map[string]string{
			"agent": agentCfg.Name,
			"repo":  task.Repo,
		},
	}
	if agentCfg.Runtime == config.RuntimeAgentM {
		spec.Scenario = agentCfg.Scenario
		spec.Extensions = agentMExtensions(agentCfg)
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
		Session:     sess,
		Handle:      handle,
		AgentCfg:    agentCfg,
		Task:        task,
		GitOps:      r.GitOps,
		LabelWriter: r.LabelWriter,
		PRMerger:    r.PRMerger,
	}, nil
}

func (r *AgentBridgeRuntime) Launch(ctx context.Context, agentCfg *config.AgentConfig, task *TaskContext) (*Result, error) {
	if agentCfg.Runtime == config.RuntimeAgentM && task != nil && task.Repo != "" && task.Issue.Number > 0 && r.ControlObserver != nil {
		return r.launchWithControlLoop(ctx, agentCfg, task)
	}
	return r.launchDirect(ctx, agentCfg, task)
}

func (r *AgentBridgeRuntime) launchDirect(ctx context.Context, agentCfg *config.AgentConfig, task *TaskContext) (*Result, error) {
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

func (r *AgentBridgeRuntime) launchWithControlLoop(ctx context.Context, agentCfg *config.AgentConfig, task *TaskContext) (*Result, error) {
	branch := fmt.Sprintf("workbuddy/issue-%d", task.Issue.Number)

	obj := r.controlObjective(agentCfg)

	maxRounds := r.ControlMaxRounds
	if maxRounds <= 0 {
		maxRounds = 5
	}

	cfg := &control.LoopConfig{
		Observer:   r.ControlObserver,
		Controller: &control.Controller{MaxRounds: maxRounds},
		Repo:       task.Repo,
		IssueNum:   task.Issue.Number,
		Branch:     branch,
		Objective:  obj,
	}

	var lastResult *Result
	runAgent := func(ctx context.Context, prompt string, resumeSessionID string) (string, error) {
		// For resume rounds, override the prompt with the feedback message.
		taskCopy := *task
		agentCfgCopy := *agentCfg
		if resumeSessionID != "" && prompt != "" {
			agentCfgCopy.Prompt = prompt
		}

		sess, err := r.startWithResume(ctx, &agentCfgCopy, &taskCopy, resumeSessionID)
		if err != nil {
			return "", err
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
		lastResult = result

		sessionID := ""
		if result != nil && result.SessionRef.ID != "" {
			sessionID = result.SessionRef.ID
		}
		return sessionID, runErr
	}

	loopResult, err := control.Run(ctx, cfg, runAgent)
	if err != nil && lastResult != nil {
		return lastResult, err
	}
	if err != nil {
		return nil, err
	}
	if lastResult == nil {
		lastResult = &Result{}
	}
	if lastResult.Meta == nil {
		lastResult.Meta = map[string]string{}
	}
	lastResult.Meta["control_rounds"] = fmt.Sprintf("%d", loopResult.Rounds)
	lastResult.Meta["control_action"] = controlActionString(loopResult.Action)
	if loopResult.Message != "" {
		lastResult.Meta["control_message"] = loopResult.Message
	}
	return lastResult, nil
}

// controlObjective returns the right objective for the agent's role.
func (r *AgentBridgeRuntime) controlObjective(agentCfg *config.AgentConfig) *control.Objective {
	switch agentCfg.Role {
	case "review":
		return control.ReviewObjective()
	case "merge":
		return control.MergeObjective()
	default:
		return control.DevObjective()
	}
}

func controlActionString(a control.Action) string {
	switch a {
	case control.ActionComplete:
		return "complete"
	case control.ActionBlock:
		return "block"
	case control.ActionResume:
		return "resume"
	default:
		return "unknown"
	}
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
	// LabelWriter, when non-nil and the underlying session is AgentM,
	// applies the agent-suggested next_label AFTER GitOps publish
	// succeeds. Strict sequence: if the PR cannot be opened we MUST NOT
	// advance the state machine (REQ-146 / #332).
	LabelWriter AgentMLabelWriter
	// PRMerger, when non-nil and the applied label is MergedLabel,
	// squash-merges the PR after the merge-agent approves.
	PRMerger AgentMPRMerger
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
		// The agentm session log lives inside the backend's per-session temp
		// dir, which the backend removes on Close() (called by the worker
		// after this run). Copy it into the durable worker session dir before
		// that happens so the native conversation log survives for audit, and
		// point SessionPath at the copy rather than the soon-to-be-deleted
		// temp file. If there's no handle (e.g. the one-shot Launch path) or
		// the copy fails, fall back to the temp path — correctness of the run
		// does not depend on capturing the log.
		if logPath := extractor.SessionLogPath(); logPath != "" {
			sessionPath = logPath
			if durable, err := copyAgentMSessionLog(s.Handle, logPath); err == nil && durable != "" {
				sessionPath = durable
			}
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
			if out.NextLabel != "" {
				if _, labelErr := s.applyAgentMNextLabel(ctx, out.NextLabel); labelErr != nil {
					meta["agentm_label_error"] = labelErr.Error()
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

// applyAgentMNextLabel invokes the coordinator-managed label writer for
// AgentM runs. The wrapping span carries wb.next_label.applied so the
// dashboard can attribute state-machine advances to AgentM runs distinctly
// from agent-driven `gh issue edit` calls. Returns the applied label on
// success so the caller can stamp Result.Meta for downstream observability.
func (s *AgentBridgeSession) applyAgentMNextLabel(ctx context.Context, label string) (string, error) {
	if s.LabelWriter == nil || s.Task == nil {
		return "", nil
	}
	label = strings.TrimSpace(label)
	if label == "" {
		return "", nil
	}
	repo := s.Task.Repo
	issueNum := s.Task.Issue.Number
	if repo == "" || issueNum <= 0 {
		return "", nil
	}
	ctx, span := tracing.Start(ctx, "runtime.agentm.apply_next_label",
		attribute.String("workbuddy.repo", repo),
		attribute.Int("workbuddy.issue.number", issueNum),
		attribute.String("wb.next_label.candidate", label),
	)
	defer span.End()
	if err := s.LabelWriter.ApplyNextLabel(ctx, repo, issueNum, label); err != nil {
		span.SetAttributes(attribute.String("wb.next_label.error", err.Error()))
		return "", err
	}
	span.SetAttributes(attribute.String("wb.next_label.applied", label))
	return label, nil
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
	// AgentM runs inside a sandboxed pod and cannot (and must not) run
	// `gh issue edit` to flip labels — the coordinator-managed LabelWriter
	// owns that transition. So suppress the transition footer for agentm and
	// render the body alone. claude/codex keep the footer (they self-route).
	if agentCfg.Runtime == config.RuntimeAgentM {
		if p := ResolvePromptBody(agentCfg, task); p != "" {
			rendered, err := RenderCommandRaw(p, task)
			if err == nil {
				return rendered
			}
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

// agentMExtensions converts the agent-config extension list into agent.Spec
// extensions for the AgentM CLI `-e` flags. The system prompt, if any, is
// just an ordinary extension entry in agentCfg.Extensions — no special-casing.
func agentMExtensions(agentCfg *config.AgentConfig) []agent.SpecExtension {
	if len(agentCfg.Extensions) == 0 {
		return nil
	}
	out := make([]agent.SpecExtension, 0, len(agentCfg.Extensions))
	for _, ext := range agentCfg.Extensions {
		if strings.TrimSpace(ext.Module) == "" {
			continue
		}
		out = append(out, agent.SpecExtension{Module: ext.Module, Config: ext.Config})
	}
	return out
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
const EnvDevContainerImage = "AGENTM_AGENT_ENV_IMAGE"

// EnvAgentMObservabilityDir is the env var workbuddy injects into the
// AgentM subprocess so it writes observability JSONL files to a
// PVC-backed directory instead of the ephemeral worktree.
const EnvAgentMObservabilityDir = "AGENTM_OBSERVABILITY_DIR"

// DefaultDataDir is the PVC mount path used by the Helm chart
// (persistence.mountPath in values.yaml).
const DefaultDataDir = "/var/lib/workbuddy"

// EnvAgentMSkillsDir is the env var workbuddy injects to tell AgentM's
// agent-env to upload skill files from this PVC-backed directory into the
// sandbox at .agentm/skills/ so skill_loader can discover them.
const EnvAgentMSkillsDir = "AGENTM_SKILLS_DIR"

// EnvWorkbuddyRepo is the GitHub repo (OWNER/NAME) for the sandbox to clone.
const EnvWorkbuddyRepo = "WORKBUDDY_REPO"

// EnvWorkbuddyIssueNum is the issue number the agent is working on.
const EnvWorkbuddyIssueNum = "WORKBUDDY_ISSUE_NUM"

// injectAgentMEnv adds AgentM-specific env vars derived from the agent
// config and task context. The sandbox uses these to clone the repo and
// checkout the correct branch autonomously.
func injectAgentMEnv(agentCfg *config.AgentConfig, env map[string]string, task *TaskContext) map[string]string {
	if agentCfg == nil || agentCfg.Runtime != config.RuntimeAgentM {
		return env
	}
	if env == nil {
		env = map[string]string{}
	}
	if image := strings.TrimSpace(agentCfg.DevContainerImage); image != "" {
		if _, exists := env[EnvDevContainerImage]; !exists {
			env[EnvDevContainerImage] = image
		}
	}
	if task != nil && task.Repo != "" && task.Issue.Number > 0 {
		if _, exists := env[EnvAgentMObservabilityDir]; !exists {
			slug := repoToSlug(task.Repo)
			env[EnvAgentMObservabilityDir] = filepath.Join(
				DefaultDataDir, "traces", slug,
				fmt.Sprintf("issue-%d", task.Issue.Number),
			)
		}
		// Repo and issue for sandbox git clone + branch checkout.
		if _, exists := env[EnvWorkbuddyRepo]; !exists {
			env[EnvWorkbuddyRepo] = task.Repo
		}
		if _, exists := env[EnvWorkbuddyIssueNum]; !exists {
			env[EnvWorkbuddyIssueNum] = fmt.Sprintf("%d", task.Issue.Number)
		}
	}
	// Skills directory on PVC — uploaded to sandbox by operations_agent_env.
	if _, exists := env[EnvAgentMSkillsDir]; !exists {
		skillsDir := filepath.Join(DefaultDataDir, "skills")
		env[EnvAgentMSkillsDir] = skillsDir
	}
	return env
}

// repoToSlug converts a repo name like "LGU-SE-Internal/opentelemetry-demo"
// to "LGU-SE-Internal-opentelemetry-demo" for use in filesystem paths.
func repoToSlug(repo string) string {
	return strings.ReplaceAll(repo, "/", "-")
}

// issueTraceContext returns a context carrying a deterministic OTel trace ID
// derived from repo+issueNum so that all AgentM dispatches for the same
// issue share a single trace. The span ID is random per dispatch so each
// run is distinguishable within the trace.
func issueTraceContext(repo string, issueNum int) context.Context {
	h := sha256.New()
	h.Write([]byte(repo))
	_ = binary.Write(h, binary.BigEndian, int64(issueNum))
	sum := h.Sum(nil)

	traceID := hex.EncodeToString(sum[:16])
	spanID := make([]byte, 8)
	_, _ = rand.Read(spanID)
	tp := fmt.Sprintf("00-%s-%s-01", traceID, hex.EncodeToString(spanID))

	// Build a context that the OTel propagator will extract TRACEPARENT from.
	// We inject via MapCarrier so injectTraceContext picks it up the same way
	// a real parent span would propagate.
	carrier := propagation.MapCarrier{"traceparent": tp}
	return otel.GetTextMapPropagator().Extract(context.Background(), carrier)
}

// issueTraceparent returns a W3C traceparent header value with a
// deterministic trace ID derived from repo+issue and a random span ID.
// Exported for testing.
func issueTraceparent(repo string, issueNum int) string {
	h := sha256.New()
	h.Write([]byte(repo))
	_ = binary.Write(h, binary.BigEndian, int64(issueNum))
	sum := h.Sum(nil)

	traceID := hex.EncodeToString(sum[:16])
	spanID := make([]byte, 8)
	_, _ = rand.Read(spanID)
	return fmt.Sprintf("00-%s-%s-01", traceID, hex.EncodeToString(spanID))
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
