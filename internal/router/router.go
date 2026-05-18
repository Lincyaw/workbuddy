// Package router makes the scheduling decision for an agent dispatch: given
// a workflow state and the set of configured agents, which agent should run,
// and should the task proceed at all (dependency gate)?
//
// It intentionally does NOT load GitHub context, persist the task row, or
// provision worktrees — those responsibilities live in:
//
//   - internal/taskprep   — context loading, task persistence, embedded dispatch
//   - internal/worker     — workspace/worktree provisioning at execute time
//   - internal/dependency — dependency verdict computation + gating helper
//
// See issue #145 finding #8 for the motivation for this split.
package router

import (
	"context"
	"log"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/dependency"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/taskprep"
	"github.com/Lincyaw/workbuddy/internal/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// WorkerTask is re-exported from internal/taskprep so existing consumers
// (cmd/serve.go, cmd/coordinator.go, internal/worker/distributed.go, tests)
// continue to compile against router.WorkerTask. The canonical definition
// lives on the preparer side because materialising a WorkerTask is the
// preparer's responsibility.
type WorkerTask = taskprep.WorkerTask

// IssueDataReader is re-exported from internal/taskprep for the same reason.
type IssueDataReader = taskprep.IssueDataReader

// Decision is the output of the scheduling layer: "dispatch AgentName for
// this issue in this workflow state". It is consumed by a Preparer that
// materialises the task row + GH context.
type Decision = taskprep.Decision

// Preparer is the contract the Router delegates task materialisation to.
// The concrete implementation lives in internal/taskprep.
type Preparer interface {
	Prepare(ctx context.Context, d Decision) error
}

// readerAwarePreparer is an optional extension implemented by preparers that
// accept a late IssueDataReader override (taskprep.Preparer does).
type readerAwarePreparer interface {
	Preparer
	SetIssueDataReader(IssueDataReader)
}

// Router consumes DispatchRequests from the StateMachine, applies
// scheduling policy (agent lookup, dependency gating) and hands the
// resulting Decision to a Preparer.
//
// The registry is kept on the struct for call-site compatibility and for a
// potential future when scheduling actually selects among multiple workers;
// today the preparer persists the task for a worker to claim, whether that
// worker was launched by `serve` or by a standalone `workbuddy worker`.
// EventRecorder is the narrow event-log interface the router uses to publish
// "considered but did not enqueue" decisions (REQ #345). Kept here rather
// than importing the concrete *eventlog.EventLogger so test fakes — and
// any future telemetry sink — can swap in without circular imports.
type EventRecorder interface {
	Log(eventType, repo string, issueNum int, payload interface{})
}

type Router struct {
	agents    map[string]*config.AgentConfig
	workflows map[string]*config.WorkflowConfig
	registry  *registry.Registry
	gateStore dependency.GateStore
	preparer  Preparer
	events    EventRecorder
}

// NewRouter creates a Router wired for the task-queue based worker handoff.
// The call signature matches the pre-split router.NewRouter so existing
// consumers (cmd/serve.go, cmd/repo_runtime.go) don't have to be rewired.
//
// The wsMgr argument is retained for signature compatibility but is unused
// by the router — workspace provisioning lives on the worker side.
func NewRouter(
	agents map[string]*config.AgentConfig,
	reg *registry.Registry,
	st store.Store,
	repo string,
	repoRoot string,
	taskChan chan<- WorkerTask,
	wsMgr any,
	dispatchToEmbedded bool,
) *Router {
	_ = repo // reserved for future multi-repo routing; Decision already carries repo per-dispatch
	_ = wsMgr
	return &Router{
		agents:    agents,
		registry:  reg,
		gateStore: st,
		preparer:  taskprep.NewPreparer(st, repoRoot, taskChan, dispatchToEmbedded),
	}
}

// SetIssueDataReader forwards the reader override to the underlying
// preparer when it supports that hook. Kept for call-site compatibility
// with the pre-split API.
func (r *Router) SetIssueDataReader(reader IssueDataReader) {
	if rs, ok := r.preparer.(readerAwarePreparer); ok {
		rs.SetIssueDataReader(reader)
	}
}

// SetPreparer swaps in an alternative Preparer. Intended for tests and for
// cmd/* wiring that wants to construct a taskprep.Preparer with custom
// options.
func (r *Router) SetPreparer(p Preparer) {
	if p != nil {
		r.preparer = p
	}
}

// SetEventRecorder attaches (or detaches with nil) an event sink for the
// router's "considered but skipped" decisions. The recorder is best-effort
// telemetry: nil is permitted and short-circuits cheaply at the emit site,
// so existing call sites that construct a Router without telemetry are
// unaffected.
func (r *Router) SetEventRecorder(rec EventRecorder) {
	r.events = rec
}

// logEvent records a router-level event when an event recorder is wired.
// nil-safe so existing tests and callers that do not attach telemetry keep
// working.
func (r *Router) logEvent(eventType, repo string, issueNum int, payload interface{}) {
	if r == nil || r.events == nil {
		return
	}
	r.events.Log(eventType, repo, issueNum, payload)
}

// SetWorkflows wires workflow definitions into the router so it can attach
// state metadata to dispatch decisions. The state metadata travels onward to
// the Preparer (and the runtime task context) so the transition footer can
// be synthesized at prompt-render time without reaching back into the
// state-machine. Calling with nil clears the registry.
func (r *Router) SetWorkflows(workflows map[string]*config.WorkflowConfig) {
	r.workflows = workflows
}

// Run consumes dispatch requests until ctx is cancelled.
func (r *Router) Run(ctx context.Context, dispatchCh <-chan statemachine.DispatchRequest) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case req, ok := <-dispatchCh:
			if !ok {
				return nil
			}
			r.handleDispatch(ctx, req)
		}
	}
}

// handleDispatch applies scheduling policy and hands the decision to the
// preparer.
func (r *Router) handleDispatch(ctx context.Context, req statemachine.DispatchRequest) {
	// REQ-138 (#320): reparent under the persisted issue trace_id when
	// the dispatch request carries one. Empty is a no-op.
	ctx = tracing.ContextFromTraceID(ctx, req.RootTraceID)
	role, runtime := r.lookupAgentMeta(req.AgentName)
	ctx, span := tracing.Start(ctx, "router.handleDispatch",
		attribute.String("workbuddy.repo", req.Repo),
		attribute.Int("workbuddy.issue", req.IssueNum),
		attribute.String("workbuddy.agent", req.AgentName),
		attribute.String("workbuddy.workflow", req.Workflow),
		attribute.String("workbuddy.state", req.State),
	)
	tracing.SetIssueAttrs(span, req.Repo, req.IssueNum, 0, role, runtime)
	defer span.End()
	decision, ok := r.decide(req)
	if !ok {
		span.SetAttributes(attribute.Bool("workbuddy.dispatch.skipped", true))
		return
	}
	if err := r.preparer.Prepare(ctx, decision); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		log.Printf("[router] prepare failed for %s#%d: %v", req.Repo, req.IssueNum, err)
	}
}

// lookupAgentMeta returns the (role, runtime) tuple for an agent, or
// ("","") when the agent is not registered. Used to stamp the standard
// span attributes for REQ-138 (#320).
func (r *Router) lookupAgentMeta(name string) (string, string) {
	if r == nil || r.agents == nil {
		return "", ""
	}
	a, ok := r.agents[name]
	if !ok || a == nil {
		return "", ""
	}
	return a.Role, a.Runtime
}

// checkDependencyBlocked queries the recorded dependency verdict and applies
// the same gating policy as dependency.IsBlocked, but also returns the
// observed verdict string so the caller can stamp it on the
// TypeDispatchBlockedByDependency payload (REQ-149 / #345). When the
// gateStore is nil or has no recorded state, returns (false, "", nil).
func (r *Router) checkDependencyBlocked(repo string, issueNum int) (bool, string, error) {
	if r.gateStore == nil {
		return false, "", nil
	}
	depState, err := r.gateStore.QueryIssueDependencyState(repo, issueNum)
	if err != nil {
		return false, "", err
	}
	if depState == nil {
		return false, "", nil
	}
	switch depState.Verdict {
	case store.DependencyVerdictBlocked, store.DependencyVerdictNeedsHuman:
		return true, depState.Verdict, nil
	default:
		return false, depState.Verdict, nil
	}
}

// decide is the pure scheduling core: no side effects other than logging.
// Returns (decision, true) if the dispatch should proceed.
func (r *Router) decide(req statemachine.DispatchRequest) (Decision, bool) {
	agent, ok := r.agents[req.AgentName]
	if !ok {
		log.Printf("[router] agent %q not found, skipping dispatch for %s#%d", req.AgentName, req.Repo, req.IssueNum)
		r.logEvent(eventlog.TypeDispatchSkippedAgentNotFound, req.Repo, req.IssueNum, map[string]any{
			"agent":    req.AgentName,
			"workflow": req.Workflow,
			"state":    req.State,
		})
		return Decision{}, false
	}
	blocked, verdict, err := r.checkDependencyBlocked(req.Repo, req.IssueNum)
	if err != nil {
		log.Printf("[router] failed to query dependency state for %s#%d: %v", req.Repo, req.IssueNum, err)
		// Error branch omits verdict (we never observed one) and adds error
		// per the REQ-149 unified payload schema.
		r.logEvent(eventlog.TypeDispatchBlockedByDependency, req.Repo, req.IssueNum, map[string]any{
			"agent":    req.AgentName,
			"workflow": req.Workflow,
			"state":    req.State,
			"source":   "router",
			"error":    err.Error(),
		})
		return Decision{}, false
	}
	if blocked {
		log.Printf("[router] blocked dispatch for %s#%d due to dependency verdict=%s", req.Repo, req.IssueNum, verdict)
		r.logEvent(eventlog.TypeDispatchBlockedByDependency, req.Repo, req.IssueNum, map[string]any{
			"agent":    req.AgentName,
			"workflow": req.Workflow,
			"state":    req.State,
			"source":   "router",
			"verdict":  verdict,
		})
		return Decision{}, false
	}
	return Decision{
		Repo:           req.Repo,
		IssueNum:       req.IssueNum,
		AgentName:      req.AgentName,
		Agent:          agent,
		Workflow:       req.Workflow,
		State:          req.State,
		SourceState:    req.SourceState,
		RolloutIndex:   req.RolloutIndex,
		RolloutsTotal:  req.RolloutsTotal,
		RolloutGroupID: req.RolloutGroupID,
		StateDef:       r.lookupState(req.Workflow, req.State),
		SourceStateDef: r.lookupState(req.Workflow, req.SourceState),
	}, true
}

// lookupState returns the *config.State for a (workflow, state) pair, or nil
// when the workflow registry is not wired or the state is unknown. nil is
// fine — the preparer treats it as "no footer".
func (r *Router) lookupState(workflowName, stateName string) *config.State {
	if r.workflows == nil {
		return nil
	}
	wf, ok := r.workflows[workflowName]
	if !ok || wf == nil {
		return nil
	}
	state, ok := wf.States[stateName]
	if !ok {
		return nil
	}
	return state
}
