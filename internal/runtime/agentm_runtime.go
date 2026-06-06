package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/agent"
	"github.com/Lincyaw/workbuddy/internal/agent/agentm"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/control"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"

	"github.com/Lincyaw/workbuddy/internal/tracing"
)

// AgentMLabelWriter is the bridge between the runtime package and
// internal/labelwriter. Defined locally so runtime stays a leaf-of-leaves;
// production wiring constructs an adapter that satisfies this interface
// around a *labelwriter.Writer. AgentM is sandboxed and cannot edit labels
// itself (Capabilities().ManagesOwnLabels == false), so the coordinator
// owns the state-machine transition via this writer. See docs/decisions/
// 2026-06-06-runtime-strategy-and-convergence.md (§3) and
// 2026-05-13-k8s-agentm-otel.md (Block 2 § Two execution modes).
type AgentMLabelWriter interface {
	// ApplyNextLabel adds `label` to issue #issueNum in `repo`. An empty
	// label MUST be a no-op (no error). The implementation is expected
	// to consult the repo registration for host_kind so GitHub and
	// Gitea backends route to the right wire protocol.
	ApplyNextLabel(ctx context.Context, repo string, issueNum int, label string) error
}

// AgentMRuntime is the sandboxed (agent-env) runtime strategy. It composes
// the shared AgentBridgeRuntime host-exec core and layers the agentm-specific
// behavior on top: deterministic per-issue trace context, scenario/extension
// spec fields, footer-suppressed prompt rendering, structured RESULT
// extraction, coordinator-side label writes, and the closed-loop control
// loop. None of this is expressed as a runtime-name branch — the agentm
// behavior lives entirely in this type (ADR §1b).
type AgentMRuntime struct {
	*AgentBridgeRuntime
	// LabelWriter, when non-nil, applies the agent-suggested next_label
	// after a run. AgentM is sandboxed (it cannot run `gh issue edit`), so
	// this Go-side write is the only state-machine transition (REQ-146 /
	// #332). It is wired by the launcher off Capabilities().ManagesOwnLabels.
	LabelWriter AgentMLabelWriter
	// ControlObserver, when non-nil, enables the closed-loop control system:
	// after each agent run the observer checks world state against the
	// role's objective and resumes the agent if post-conditions are not met.
	ControlObserver control.Observer
	// ControlMaxRounds caps the number of observe-resume iterations.
	// Defaults to 5 when zero.
	ControlMaxRounds int
}

// NewAgentMRuntime builds the sandboxed agentm runtime strategy over the
// shared host-exec core.
func NewAgentMRuntime(factory func() (agent.Backend, error)) *AgentMRuntime {
	return &AgentMRuntime{AgentBridgeRuntime: NewAgentBridgeRuntime(config.RuntimeAgentM, factory)}
}

// Capabilities reports the sandboxed agentm contract: execution is isolated
// via agent-env, the agent cannot self-manage labels (the coordinator's
// LabelWriter owns transitions), and host gh/git credentials are not needed
// in-process. The !ManagesOwnLabels flag is what drives the launcher to wire
// the LabelWriter onto this runtime.
func (r *AgentMRuntime) Capabilities() Capabilities {
	return Capabilities{Sandboxed: true, ManagesOwnLabels: false, NeedsHostGHCreds: false}
}

// SetLabelWriter wires the coordinator-managed label writer onto this
// runtime. It is invoked by the capability-driven Registry.SetLabelWriter
// for every runtime whose Capabilities().ManagesOwnLabels is false.
func (r *AgentMRuntime) SetLabelWriter(lw AgentMLabelWriter) {
	r.LabelWriter = lw
}

func (r *AgentMRuntime) Start(ctx context.Context, agentCfg *config.AgentConfig, task *TaskContext) (Session, error) {
	return r.startWithResume(ctx, agentCfg, task, "")
}

// startWithResume boots an agentm session. When resumeSessionID is non-empty,
// the agentm backend emits --resume <id> instead of starting fresh.
func (r *AgentMRuntime) startWithResume(ctx context.Context, agentCfg *config.AgentConfig, task *TaskContext, resumeSessionID string) (Session, error) {
	prompt := resolveAgentMPrompt(agentCfg, task)
	// Derive a deterministic TRACEPARENT from repo+issue so all sessions for
	// the same issue share a trace ID.
	traceCtx := ctx
	if task != nil && task.Repo != "" && task.Issue.Number > 0 {
		traceCtx = issueTraceContext(task.Repo, task.Issue.Number)
	}

	spec := r.baseSpec(traceCtx, agentCfg, task, prompt, resumeSessionID)
	spec.Env = injectAgentMEnv(agentCfg, spec.Env, task)
	spec.Scenario = agentCfg.Scenario
	spec.Extensions = agentMExtensions(agentCfg)

	base, err := r.newBridgeSession(ctx, agentCfg, task, spec)
	if err != nil {
		return nil, err
	}
	return &AgentMSession{AgentBridgeSession: base, LabelWriter: r.LabelWriter}, nil
}

func (r *AgentMRuntime) Launch(ctx context.Context, agentCfg *config.AgentConfig, task *TaskContext) (*Result, error) {
	if task != nil && task.Repo != "" && task.Issue.Number > 0 && r.ControlObserver != nil {
		return r.launchWithControlLoop(ctx, agentCfg, task)
	}
	return launchViaStart(ctx, r, agentCfg, task)
}

func (r *AgentMRuntime) launchWithControlLoop(ctx context.Context, agentCfg *config.AgentConfig, task *TaskContext) (*Result, error) {
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

		result, runErr := drainSession(ctx, sess)
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
func (r *AgentMRuntime) controlObjective(agentCfg *config.AgentConfig) *control.Objective {
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

// AgentMSession wraps the generic bridge session and layers the agentm
// structured-RESULT contract: it extracts next_label/failure_reason, copies
// the native session log into the durable session dir, and applies the
// coordinator-side label transition (gated on staleness).
type AgentMSession struct {
	*AgentBridgeSession
	// LabelWriter, when non-nil, applies the agent-suggested next_label
	// after the run. AgentM is autonomous (it pushes its own branch / PR),
	// so this label write is the only Go-side state-machine transition
	// (REQ-146 / #332).
	LabelWriter AgentMLabelWriter
}

func (s *AgentMSession) Run(ctx context.Context, events chan<- launcherevents.Event) (*Result, error) {
	result, _, err := s.runCore(ctx, events)

	// AgentM exposes a structured RESULT: contract. Surface
	// next_label/failure_reason on the Result so reporter/audit can render
	// it and route the state machine. A malformed/missing RESULT is an
	// infra failure.
	extractor, ok := s.Session.(interface {
		Output() (*agentm.Output, error)
		SessionLogPath() string
	})
	if !ok {
		return result, err
	}

	meta := result.Meta
	if meta == nil {
		meta = map[string]string{}
	}

	// The agentm session log lives inside the backend's per-session temp
	// dir, which the backend removes on Close() (called by the worker after
	// this run). Copy it into the durable worker session dir before that
	// happens so the native conversation log survives for audit, and point
	// SessionPath at the copy rather than the soon-to-be-deleted temp file.
	// If there's no handle (e.g. the one-shot Launch path) or the copy
	// fails, fall back to the temp path — correctness of the run does not
	// depend on capturing the log.
	if logPath := extractor.SessionLogPath(); logPath != "" {
		result.SessionPath = logPath
		result.RawSessionPath = logPath
		if durable, cerr := copyAgentMSessionLog(s.Handle, logPath); cerr == nil && durable != "" {
			result.SessionPath = durable
			result.RawSessionPath = durable
		}
	}

	out, perr := extractor.Output()
	switch {
	case perr != nil:
		// Mark as infra failure; the bridge returns the wait error so the
		// worker treats this as failed.
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
			if s.isStaleAgent(ctx) {
				meta["agentm_label_skipped"] = "stale: current issue state no longer matches agent trigger"
			} else if applied, labelErr := s.applyAgentMNextLabel(ctx, out.NextLabel); labelErr != nil {
				meta["agentm_label_error"] = labelErr.Error()
			} else if applied != "" {
				meta["agentm_label_applied"] = applied
			}
		}
	}

	if len(meta) == 0 {
		meta = nil
	}
	result.Meta = meta
	return result, err
}

// applyAgentMNextLabel invokes the coordinator-managed label writer for
// AgentM runs. The wrapping span carries wb.next_label.applied so the
// dashboard can attribute state-machine advances to AgentM runs distinctly
// from agent-driven `gh issue edit` calls. Returns the applied label on
// success so the caller can stamp Result.Meta for downstream observability.
func (s *AgentMSession) applyAgentMNextLabel(ctx context.Context, label string) (string, error) {
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

// isStaleAgent checks whether the issue still carries the status label that
// triggered this agent. If the state has already moved on (another agent's
// label write landed first), applying this agent's next_label would revert
// the state machine — a silent corruption. Best-effort: gh CLI failure is
// treated as "not stale" so the label write proceeds rather than silently
// dropping results.
func (s *AgentMSession) isStaleAgent(ctx context.Context) bool {
	if s.AgentCfg == nil || s.Task == nil || len(s.AgentCfg.Triggers) == 0 {
		return false
	}
	triggerState := s.AgentCfg.Triggers[0].State
	if triggerState == "" {
		return false
	}
	expectedLabel := "status:" + strings.ReplaceAll(triggerState, "_", "-")

	bin, err := exec.LookPath("gh")
	if err != nil {
		return false
	}
	out, err := exec.CommandContext(ctx, bin,
		"issue", "view", fmt.Sprintf("%d", s.Task.Issue.Number),
		"--repo", s.Task.Repo,
		"--json", "labels", "--jq", ".labels[].name",
	).CombinedOutput()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == expectedLabel {
			return false
		}
	}
	log.Printf("[runtime] stale agent %s for %s#%d: expected label %q not found, skipping label transition",
		s.AgentCfg.Name, s.Task.Repo, s.Task.Issue.Number, expectedLabel)
	return true
}

// resolveAgentMPrompt renders the agent prompt for the sandboxed agentm
// runtime. AgentM runs inside a pod and cannot (and must not) run
// `gh issue edit` to flip labels — the coordinator-managed LabelWriter owns
// that transition. So the transition footer is suppressed and the body is
// rendered alone (claude/codex keep the footer via resolvePrompt).
func resolveAgentMPrompt(agentCfg *config.AgentConfig, task *TaskContext) string {
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
