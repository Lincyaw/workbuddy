package statemachine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Lincyaw/workbuddy/internal/alertbus"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/workflow"
)

// ChangeEvent represents a state change detected by the Poller.
type ChangeEvent struct {
	Type     string // poller.EventLabelAdded, poller.EventLabelRemoved, poller.EventPRCreated, etc.
	Repo     string
	IssueNum int
	Labels   []string // current labels on the issue
	Detail   string   // extra info (e.g. label name, comment body)
	Author   string
}

// DispatchRequest is sent to the Task Router when an agent needs to run.
type DispatchRequest struct {
	Repo      string
	IssueNum  int
	AgentName string
	Workflow  string
	State     string
}

// EventRecorder abstracts event logging so tests can use a fake.
type EventRecorder interface {
	Log(eventType, repo string, issueNum int, payload interface{})
}

// StuckTimeout is the default duration after which an issue is considered stuck
// if a label hasn't changed after an agent completes.
const StuckTimeout = 5 * time.Minute

const defaultJoinStrategy = config.JoinAllPassed

// MaxConsecutiveAgentFailures is the number of back-to-back failed or timed-out
// tasks the same agent may accumulate on one issue before the coordinator
// refuses to dispatch again. It exists to break runaway re-dispatch cycles
// caused by launcher-layer crashes, sustained infra errors, or agents whose
// verdict flips failure-pass-failure on repeat. Humans must intervene to
// clear the condition (fix the worker, change labels, or close the issue).
const MaxConsecutiveAgentFailures = 3

// DoneLabel is the canonical terminal label for completed issues. Dispatch
// for any agent is refused when this label is present, regardless of the
// stale workflow state captured in a retry request.
const DoneLabel = "status:done"

// DefaultIssueClaimLease is the fallback lease duration for per-issue dispatch
// claims (AC-7 of REQ-057). Long enough to accommodate a dev-agent run, short
// enough to recover from a crashed coordinator.
const DefaultIssueClaimLease = 30 * time.Minute

type dispatchGroup struct {
	workflow string
	state    string
	join     string
	agents   map[string]struct{}

	dispatchedAgents map[string]struct{}
	completedTaskIDs map[string]struct{}
	completedAgents  map[string]struct{}
	successAgents    map[string]struct{}
	failedAgents     map[string]struct{}
}

func newDispatchGroup(wf, state, join string, agents []string) *dispatchGroup {
	g := &dispatchGroup{
		workflow:         wf,
		state:            state,
		join:             join,
		agents:           make(map[string]struct{}, len(agents)),
		dispatchedAgents: make(map[string]struct{}, len(agents)),
		completedTaskIDs: make(map[string]struct{}),
		completedAgents:  make(map[string]struct{}),
		successAgents:    make(map[string]struct{}),
		failedAgents:     make(map[string]struct{}),
	}
	for _, a := range agents {
		g.agents[a] = struct{}{}
	}
	return g
}

// StateMachine evaluates Poller events against workflow definitions,
// manages transitions, detects cycles, and dispatches agent tasks.
type StateMachine struct {
	workflows map[string]*config.WorkflowConfig
	store     *store.Store
	dispatch  chan<- DispatchRequest
	eventlog  EventRecorder
	alertBus  *alertbus.Bus

	// processedEvents tracks (repo, issueNum, eventKey) to ensure idempotency.
	processedEvents sync.Map // key: string → struct{}

	// inflightMu protects inflight map.
	inflightMu sync.Mutex
	// inflight tracks active dispatch groups per issue. key: "repo#issueNum"
	inflight map[string]*dispatchGroup

	// stuckTimeout configurable for tests; defaults to StuckTimeout.
	stuckTimeout time.Duration

	// completionTimes tracks when a completed state is eligible for stuck detection.
	// key: "repo#issueNum" → (completionTime, labelsAtCompletion)
	completionMu    sync.Mutex
	completionTimes map[string]completionRecord

	workflowManager *workflow.Manager

	// issueClaim configuration (REQ-057). When claimerID is empty, per-issue
	// claim acquisition is skipped — useful for tests that don't care and for
	// backwards compatibility with the existing NewStateMachine signature.
	// claimTokens tracks the active claim token per issue so MarkAgentCompleted
	// can release the lease it acquired.
	issueClaimerID  string
	issueClaimLease time.Duration
	claimTokensMu   sync.Mutex
	claimTokens     map[string]string // key: "repo#issueNum" → active claim token
}

type completionRecord struct {
	at     time.Time
	labels string // JSON-encoded labels at completion
}

// NewStateMachine creates a StateMachine.
func NewStateMachine(
	workflows map[string]*config.WorkflowConfig,
	st *store.Store,
	dispatch chan<- DispatchRequest,
	eventlog EventRecorder,
	alertBus *alertbus.Bus,
) *StateMachine {
	var workflowManager *workflow.Manager
	if st != nil {
		workflowManager = workflow.NewManager(st.DB())
	}
	return &StateMachine{
		workflows:       workflows,
		store:           st,
		dispatch:        dispatch,
		eventlog:        eventlog,
		alertBus:        alertBus,
		inflight:        make(map[string]*dispatchGroup),
		stuckTimeout:    StuckTimeout,
		completionTimes: make(map[string]completionRecord),
		workflowManager: workflowManager,
		claimTokens:     make(map[string]string),
	}
}

// SetIssueClaim enables per-issue dispatch-claim acquisition (REQ-057). When
// claimerID is empty the feature is disabled (useful for tests). When lease is
// zero or negative, DefaultIssueClaimLease is used.
func (sm *StateMachine) SetIssueClaim(claimerID string, lease time.Duration) {
	sm.issueClaimerID = strings.TrimSpace(claimerID)
	if lease <= 0 {
		lease = DefaultIssueClaimLease
	}
	sm.issueClaimLease = lease
}

// SetStuckTimeout overrides the stuck timeout (useful for tests).
func (sm *StateMachine) SetStuckTimeout(d time.Duration) {
	sm.stuckTimeout = d
}

// ResetDedup clears the processed-events deduplication set.
// Call this at the start of each poll cycle so that events from
// different cycles are not suppressed.
func (sm *StateMachine) ResetDedup() {
	sm.processedEvents.Range(func(key, value interface{}) bool {
		sm.processedEvents.Delete(key)
		return true
	})
}

// HandleEvent processes a single ChangeEvent from the Poller.
func (sm *StateMachine) HandleEvent(ctx context.Context, event ChangeEvent) error {
	// Idempotency: build a unique key for this event.
	eventKey := fmt.Sprintf("%s#%d#%s#%s", event.Repo, event.IssueNum, event.Type, event.Detail)
	if _, loaded := sm.processedEvents.LoadOrStore(eventKey, struct{}{}); loaded {
		return nil // already processed
	}

	// Find matching workflows for this issue.
	matched := sm.findMatchingWorkflows(event)

	if len(matched) == 0 {
		// No match — skip silently.
		return nil
	}
	if len(matched) > 1 {
		// Multiple workflows match — log error, skip.
		names := make([]string, len(matched))
		for i, m := range matched {
			names[i] = m.Name
		}
		payload, _ := json.Marshal(map[string]interface{}{
			"workflows": names,
			"labels":    event.Labels,
		})
		sm.eventlog.Log(eventlog.TypeErrorMultiWorkflow, event.Repo, event.IssueNum, string(payload))
		return fmt.Errorf("statemachine: issue %s#%d matches %d workflows", event.Repo, event.IssueNum, len(matched))
	}

	wf := matched[0]
	return sm.processWorkflowEvent(ctx, wf, event)
}

// findMatchingWorkflows returns workflows whose trigger label is present on the issue.
func (sm *StateMachine) findMatchingWorkflows(event ChangeEvent) []*config.WorkflowConfig {
	var matched []*config.WorkflowConfig
	for _, wf := range sm.workflows {
		triggerLabel := wf.Trigger.IssueLabel
		if triggerLabel == "" {
			continue
		}
		for _, l := range event.Labels {
			if l == triggerLabel {
				matched = append(matched, wf)
				break
			}
		}
	}
	return matched
}

// processWorkflowEvent handles the event for a single matched workflow.
func (sm *StateMachine) processWorkflowEvent(ctx context.Context, wf *config.WorkflowConfig, event ChangeEvent) error {
	// Determine current state from labels.
	currentStateName, currentState := sm.findCurrentState(wf, event.Labels)
	if err := sm.ensureWorkflowInstance(wf.Name, event.Repo, event.IssueNum, currentStateName); err != nil {
		return fmt.Errorf("statemachine: ensure workflow instance: %w", err)
	}
	if currentState == nil {
		// Issue has the workflow trigger but no status label matching any state.
		// Could be initial state or misconfigured. Skip silently.
		return nil
	}

	// State-entry detection: dispatch the state agents if:
	// 1. label_added matches the current state's enter_label (label just changed), OR
	// 2. issue_created and the issue already has a state label with agents (first seen)
	stateEntryDetected := (event.Type == poller.EventLabelAdded && event.Detail == currentState.EnterLabel) ||
		(event.Type == poller.EventIssueCreated)
	if stateEntryDetected && sm.stateHasAgents(currentState) {
		log.Printf("[statemachine] state entry detected: %s#%d entered %q, dispatching agents %q",
			event.Repo, event.IssueNum, currentStateName, sm.stateAgents(currentState))
		sm.eventlog.Log(eventlog.TypeStateEntry, event.Repo, event.IssueNum,
			map[string]interface{}{
				"state":  currentStateName,
				"agents": append([]string(nil), sm.stateAgents(currentState)...),
				"join":   currentState.Join,
			})

		// Clear any stale inflight group: if a label_added event triggers state entry,
		// the previous agent's work is done (it changed the label). The worker goroutine
		// may not have called MarkAgentCompleted yet due to a race condition.
		issueKey := sm.issueKey(event.Repo, event.IssueNum)
		sm.inflightMu.Lock()
		if existing, ok := sm.inflight[issueKey]; ok && existing.state != currentStateName {
			log.Printf("[statemachine] clearing prior inflight group for %s (was state=%q, now=%q)", issueKey, existing.state, currentStateName)
			delete(sm.inflight, issueKey)
		}
		sm.inflightMu.Unlock()

		return sm.dispatchStateAgents(ctx, event.Repo, event.IssueNum, wf.Name, currentStateName, currentState)
	}

	if !stateEntryDetected {
		_, err := sm.evaluateTransitions(ctx, wf, event, currentStateName, currentState)
		return err
	}

	return nil
}

func (sm *StateMachine) publishAlert(eventKind string, severity alertbus.Severity, repo string, issueNum int, agentName string, payload map[string]any) {
	if sm.alertBus == nil {
		return
	}
	sm.alertBus.Publish(alertbus.AlertEvent{
		Kind:      eventKind,
		Severity:  severity,
		Repo:      repo,
		IssueNum:  issueNum,
		AgentName: agentName,
		Timestamp: time.Now().Unix(),
		Payload:   payload,
	})
}

// evaluateTransitions evaluates transitions for the given state and event.
// It returns true if any transition was taken, or an error if transition bookkeeping fails.
func (sm *StateMachine) evaluateTransitions(ctx context.Context, wf *config.WorkflowConfig, event ChangeEvent, currentStateName string, currentState *config.State) (bool, error) {
	// Build evaluation context.
	evalCtx := &EvalContext{
		EventType:     event.Type,
		Labels:        event.Labels,
		LabelAdded:    "",
		LabelRemoved:  "",
		LatestComment: event.Detail,
	}
	switch event.Type {
	case poller.EventLabelAdded:
		evalCtx.LabelAdded = event.Detail
	case poller.EventLabelRemoved:
		evalCtx.LabelRemoved = event.Detail
	}

	// Evaluate transitions.
	for _, tr := range currentState.Transitions {
		if !EvaluateCondition(tr.When, evalCtx) {
			continue
		}

		targetStateName := tr.To
		targetState, ok := wf.States[targetStateName]
		if !ok {
			log.Printf("[statemachine] warning: transition target %q not found in workflow %q", targetStateName, wf.Name)
			continue
		}

		// Check if this is a back-edge (target state was visited before).
		isBackEdge := sm.isBackEdge(event.Repo, event.IssueNum, targetStateName)

		if isBackEdge {
			count, err := sm.store.IncrementTransition(event.Repo, event.IssueNum, currentStateName, targetStateName)
			if err != nil {
				return false, fmt.Errorf("statemachine: increment transition: %w", err)
			}

			maxRetries := wf.MaxRetries
			if maxRetries <= 0 {
				maxRetries = 3 // sensible default
			}

			if count >= maxRetries {
				// Reject back-edge, transition to failed.
				sm.eventlog.Log(eventlog.TypeCycleLimitReached, event.Repo, event.IssueNum,
					map[string]interface{}{"from": currentStateName, "to": targetStateName, "count": count, "max_retries": maxRetries})
				sm.publishAlert(alertbus.KindCycleLimitReached, alertbus.SeverityError, event.Repo, event.IssueNum, "", map[string]any{
					"from":        currentStateName,
					"to":          targetStateName,
					"count":       count,
					"max_retries": maxRetries,
				})

				// Mark as failed — dispatch request not sent.
				// The "failed" state and "needs-human" label would be applied
				// by the system. Since Go code doesn't write labels (agents do),
				// we record the event. In v0.1.0, we still record the intent.
				sm.eventlog.Log(eventlog.TypeTransitionToFailed, event.Repo, event.IssueNum,
					map[string]interface{}{"from": currentStateName, "rejected_to": targetStateName, "needs_human": true})
				sm.publishAlert(alertbus.KindTransitionToFailed, alertbus.SeverityError, event.Repo, event.IssueNum, "", map[string]any{
					"from":        currentStateName,
					"rejected_to": targetStateName,
					"needs_human": true,
				})
				return false, nil
			}
		} else {
			// Not a back-edge, but still record the transition for history.
			_, err := sm.store.IncrementTransition(event.Repo, event.IssueNum, currentStateName, targetStateName)
			if err != nil {
				return false, fmt.Errorf("statemachine: increment transition: %w", err)
			}
		}

		// Persist the workflow state transition.
		if sm.workflowManager != nil {
			if err := sm.workflowManager.Advance(event.Repo, event.IssueNum, wf.Name, currentStateName, targetStateName, currentState.Agent); err != nil {
				return false, fmt.Errorf("statemachine: persist workflow transition: %w", err)
			}
		}

		// Log the transition.
		sm.eventlog.Log(eventlog.TypeTransition, event.Repo, event.IssueNum,
			map[string]string{"from": currentStateName, "to": targetStateName})

		// If the target state has agents, dispatch them.
		if sm.stateHasAgents(targetState) {
			if err := sm.dispatchStateAgents(ctx, event.Repo, event.IssueNum, wf.Name, targetStateName, targetState); err != nil {
				return true, err
			}
		}

		// Only process the first matching transition.
		return true, nil
	}

	return false, nil
}

func (sm *StateMachine) ensureWorkflowInstance(workflowName, repo string, issueNum int, currentState string) error {
	if sm.workflowManager == nil {
		return nil
	}
	return sm.workflowManager.CreateIfMissing(repo, issueNum, workflowName, currentState)
}

// DispatchAgent sends a dispatch request for the given agent, respecting in-flight group locking.
func (sm *StateMachine) DispatchAgent(ctx context.Context, repo string, issueNum int, agentName, workflow, state string) error {
	if blocked, err := sm.isBlockedByDone(repo, issueNum, agentName); err != nil {
		return err
	} else if blocked {
		return nil
	}
	if blocked, err := sm.isBlockedByFailureCap(repo, issueNum, agentName); err != nil {
		return err
	} else if blocked {
		return nil
	}
	if blocked, err := sm.isBlockedByDependency(repo, issueNum, agentName); err != nil {
		return err
	} else if blocked {
		return nil
	}
	if blocked, err := sm.isBlockedByIssueClaim(repo, issueNum, agentName, workflow, state); err != nil {
		return err
	} else if blocked {
		return nil
	}
	return sm.dispatchSingleAgent(ctx, repo, issueNum, agentName, workflow, state)
}

// isBlockedByIssueClaim acquires (or extends) the persistent per-issue claim
// for this coordinator so that concurrent coordinators sharing the same SQLite
// database cannot dispatch overlapping tasks on the same issue. A successful
// self-extension is transparent; a fresh acquisition on an expired prior
// holder logs TypeIssueClaimExpired; contention with another live holder logs
// TypeDispatchSkippedClaim and blocks dispatch.
func (sm *StateMachine) isBlockedByIssueClaim(repo string, issueNum int, agentName, workflow, state string) (bool, error) {
	if sm.store == nil || sm.issueClaimerID == "" {
		return false, nil
	}
	lease := sm.issueClaimLease
	if lease <= 0 {
		lease = DefaultIssueClaimLease
	}
	res, err := sm.store.AcquireIssueClaim(repo, issueNum, sm.issueClaimerID, lease)
	if err != nil {
		if errors.Is(err, store.ErrIssueClaimHeldByOther) {
			other := ""
			if claim, qErr := sm.store.QueryIssueClaim(repo, issueNum); qErr == nil && claim != nil {
				other = claim.WorkerID
			}
			sm.eventlog.Log(eventlog.TypeDispatchSkippedClaim, repo, issueNum, map[string]any{
				"agent":    agentName,
				"workflow": workflow,
				"state":    state,
				"claimer":  sm.issueClaimerID,
				"held_by":  other,
				"reason":   "issue_claim_held_by_other",
			})
			return true, nil
		}
		return false, fmt.Errorf("statemachine: acquire issue claim: %w", err)
	}
	if res.OverwrotePrior {
		sm.eventlog.Log(eventlog.TypeIssueClaimExpired, repo, issueNum, map[string]any{
			"agent":       agentName,
			"workflow":    workflow,
			"state":       state,
			"claimer":     sm.issueClaimerID,
			"prior_owner": res.PriorWorkerID,
		})
	}
	sm.claimTokensMu.Lock()
	sm.claimTokens[sm.issueKey(repo, issueNum)] = res.ClaimToken
	sm.claimTokensMu.Unlock()
	return false, nil
}

// releaseIssueClaim drops the persistent claim for (repo, issueNum) when the
// state machine has finished dispatching work for the current group. No-ops
// when the feature is disabled.
func (sm *StateMachine) releaseIssueClaim(repo string, issueNum int) {
	if sm.store == nil || sm.issueClaimerID == "" {
		return
	}
	key := sm.issueKey(repo, issueNum)
	sm.claimTokensMu.Lock()
	delete(sm.claimTokens, key)
	sm.claimTokensMu.Unlock()
	if _, err := sm.store.ReleaseIssueClaim(repo, issueNum, sm.issueClaimerID); err != nil {
		log.Printf("[statemachine] release issue claim for %s#%d failed: %v", repo, issueNum, err)
	}
}

// isBlockedByDone refuses dispatch when the issue already carries the
// terminal DoneLabel in the cached label snapshot. This guards against stale
// redispatch paths that carry a pre-completion state value.
func (sm *StateMachine) isBlockedByDone(repo string, issueNum int, agentForLog string) (bool, error) {
	if sm.store == nil {
		return false, nil
	}
	ic, err := sm.store.QueryIssueCache(repo, issueNum)
	if err != nil {
		return false, fmt.Errorf("statemachine: query issue cache: %w", err)
	}
	if ic == nil || ic.Labels == "" {
		return false, nil
	}
	var labels []string
	if unmarshalErr := json.Unmarshal([]byte(ic.Labels), &labels); unmarshalErr != nil {
		// Fall back to a quoted-substring scan of the raw cache entry so a
		// corrupted labels row still blocks dispatch if it mentions the done
		// label. Log the corruption separately so operators can fix the cache.
		sm.eventlog.Log(eventlog.TypeError, repo, issueNum, map[string]any{
			"source": "issue_cache_labels_unmarshal",
			"error":  unmarshalErr.Error(),
		})
		if strings.Contains(ic.Labels, `"`+DoneLabel+`"`) {
			sm.eventlog.Log(eventlog.TypeDispatchBlockedByDone, repo, issueNum, map[string]string{
				"agent":    agentForLog,
				"label":    DoneLabel,
				"fallback": "substring_scan_after_unmarshal_error",
			})
			sm.publishAlert(alertbus.KindDispatchBlocked, alertbus.SeverityInfo, repo, issueNum, "", map[string]any{
				"reason": "status_done",
				"agent":  agentForLog,
			})
			return true, nil
		}
		return false, nil
	}
	for _, l := range labels {
		if l == DoneLabel {
			sm.eventlog.Log(eventlog.TypeDispatchBlockedByDone, repo, issueNum, map[string]string{
				"agent": agentForLog,
				"label": DoneLabel,
			})
			sm.publishAlert(alertbus.KindDispatchBlocked, alertbus.SeverityInfo, repo, issueNum, "", map[string]any{
				"reason": "status_done",
				"agent":  agentForLog,
			})
			return true, nil
		}
	}
	return false, nil
}

// isBlockedByFailureCap refuses dispatch when this agent has accumulated
// MaxConsecutiveAgentFailures back-to-back failed or timed-out tasks on
// the issue since its last successful run.
func (sm *StateMachine) isBlockedByFailureCap(repo string, issueNum int, agentName string) (bool, error) {
	if sm.store == nil {
		return false, nil
	}
	count, err := sm.store.CountConsecutiveAgentFailures(repo, issueNum, agentName)
	if err != nil {
		return false, fmt.Errorf("statemachine: count consecutive failures: %w", err)
	}
	if count < MaxConsecutiveAgentFailures {
		return false, nil
	}
	sm.eventlog.Log(eventlog.TypeDispatchBlockedByFailureCap, repo, issueNum, map[string]any{
		"agent":             agentName,
		"consecutive_fails": count,
		"cap":               MaxConsecutiveAgentFailures,
	})
	sm.publishAlert(alertbus.KindDispatchBlocked, alertbus.SeverityError, repo, issueNum, "", map[string]any{
		"reason":            "failure_cap",
		"agent":             agentName,
		"consecutive_fails": count,
		"cap":               MaxConsecutiveAgentFailures,
	})
	return true, nil
}

// isBlockedByDependency checks if the issue is blocked by a dependency.
// Returns true if blocked (and logs the event), false if ready.
func (sm *StateMachine) isBlockedByDependency(repo string, issueNum int, agentForLog string) (bool, error) {
	depState, err := sm.store.QueryIssueDependencyState(repo, issueNum)
	if err != nil {
		return false, fmt.Errorf("statemachine: query dependency state: %w", err)
	}
	if depState != nil && (depState.Verdict == store.DependencyVerdictBlocked || depState.Verdict == store.DependencyVerdictNeedsHuman) {
		sm.eventlog.Log(eventlog.TypeDispatchBlockedByDependency, repo, issueNum, map[string]string{
			"verdict": depState.Verdict,
			"agent":   agentForLog,
		})
		return true, nil
	}
	return false, nil
}

// dispatchSingleAgent sends a dispatch request for one agent.
// Caller must have already checked dependency state.
func (sm *StateMachine) dispatchSingleAgent(ctx context.Context, repo string, issueNum int, agentName, workflow, state string) error {
	issueKey := sm.issueKey(repo, issueNum)

	sm.inflightMu.Lock()
	if existing, ok := sm.inflight[issueKey]; ok {
		if existing.workflow != workflow || existing.state != state {
			sm.inflightMu.Unlock()
			sm.eventlog.Log(eventlog.TypeDispatchSkippedInflight, repo, issueNum,
				map[string]string{"agent": agentName, "reason": "different state/workflow already running", "state": state, "workflow": workflow})
			return nil
		}
		// Same workflow+state: block if this agent was already dispatched.
		if _, dispatched := existing.dispatchedAgents[agentName]; dispatched {
			sm.inflightMu.Unlock()
			sm.eventlog.Log(eventlog.TypeDispatchSkippedInflight, repo, issueNum,
				map[string]string{"agent": agentName, "reason": "agent already dispatched", "state": state, "workflow": workflow})
			return nil
		}
		existing.dispatchedAgents[agentName] = struct{}{}
	} else {
		// Create a single-agent inflight group to prevent duplicate dispatches.
		sm.inflight[issueKey] = newDispatchGroup(workflow, state, defaultJoinStrategy, []string{agentName})
	}
	sm.inflightMu.Unlock()

	sm.eventlog.Log(eventlog.TypeDispatch, repo, issueNum, map[string]any{
		"agent_name": agentName,
		"workflow":   workflow,
		"state":      state,
	})

	req := DispatchRequest{
		Repo:      repo,
		IssueNum:  issueNum,
		AgentName: agentName,
		Workflow:  workflow,
		State:     state,
	}
	select {
	case sm.dispatch <- req:
	case <-ctx.Done():
		// Context cancelled before dispatch sent. Remove this agent from
		// the group. If no agents were successfully dispatched, clean up
		// the entire group.
		released := false
		sm.inflightMu.Lock()
		if g, ok := sm.inflight[issueKey]; ok {
			delete(g.dispatchedAgents, agentName)
			if len(g.dispatchedAgents) == 0 {
				delete(sm.inflight, issueKey)
				released = true
			}
		}
		sm.inflightMu.Unlock()
		if released {
			// Nothing else is pending for this issue — release the lease so
			// another coordinator/cycle can pick it up.
			sm.releaseIssueClaim(repo, issueNum)
		}
		return ctx.Err()
	}
	return nil
}

func (sm *StateMachine) dispatchStateAgents(ctx context.Context, repo string, issueNum int, wfName, state string, stateDef *config.State) error {
	agents := sm.stateAgents(stateDef)
	if len(agents) == 0 {
		return nil
	}

	// Check dependency once for all agents in this state.
	if blocked, err := sm.isBlockedByDependency(repo, issueNum, agents[0]); err != nil {
		return err
	} else if blocked {
		return nil
	}

	// Acquire the per-issue claim once for the whole group so concurrent
	// coordinators sharing the same SQLite database cannot both dispatch a
	// workflow state onto the same issue. The claim is released together
	// with the dispatch group in MarkAgentCompleted.
	if blocked, err := sm.isBlockedByIssueClaim(repo, issueNum, agents[0], wfName, state); err != nil {
		return err
	} else if blocked {
		return nil
	}

	issueKey := sm.issueKey(repo, issueNum)
	join := stateDef.Join // already normalized by config loader
	if join == "" {
		join = defaultJoinStrategy
	}

	group := newDispatchGroup(wfName, state, join, agents)

	sm.inflightMu.Lock()
	if existing, ok := sm.inflight[issueKey]; ok {
		reason := "same state group already inflight"
		if existing.workflow != wfName || existing.state != state {
			reason = "different state/workflow already running"
		}
		sm.inflightMu.Unlock()
		sm.eventlog.Log(eventlog.TypeDispatchSkippedInflight, repo, issueNum,
			map[string]string{"state": state, "reason": reason, "workflow": wfName})
		return nil
	}
	sm.inflight[issueKey] = group
	sm.inflightMu.Unlock()

	for _, agent := range agents {
		if err := sm.dispatchSingleAgent(ctx, repo, issueNum, agent, wfName, state); err != nil {
			// Don't remove the inflight group — agents already dispatched before
			// this failure are still running and need tracking. Log the error
			// and let those agents complete normally.
			log.Printf("[statemachine] partial dispatch failure for %s (agent %s): %v", issueKey, agent, err)
			return err
		}
	}
	return nil
}

// findCurrentState returns the state name and State whose enter_label matches
// one of the issue's current labels.
func (sm *StateMachine) findCurrentState(wf *config.WorkflowConfig, labels []string) (string, *config.State) {
	labelSet := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		labelSet[l] = struct{}{}
	}
	for name, state := range wf.States {
		if state.EnterLabel == "" {
			continue
		}
		if _, ok := labelSet[state.EnterLabel]; ok {
			return name, state
		}
	}
	return "", nil
}

// isBackEdge checks if the target state was previously visited by this issue.
// We check transition_counts: if any transition TO this state exists, it's a back-edge.
func (sm *StateMachine) isBackEdge(repo string, issueNum int, targetState string) bool {
	counts, err := sm.store.QueryTransitionCounts(repo, issueNum)
	if err != nil {
		log.Printf("[statemachine] error querying transition counts: %v", err)
		return false
	}
	for _, tc := range counts {
		if tc.ToState == targetState && tc.Count > 0 {
			return true
		}
	}
	return false
}

// MarkAgentCompleted should be called when an agent execution finishes.
// It clears inflight group state and records completion time for stuck detection
// when the group can no longer progress.
func (sm *StateMachine) MarkAgentCompleted(repo string, issueNum int, taskID, agentName string, exitCode int, currentLabels []string) {
	issueKey := sm.issueKey(repo, issueNum)
	agentName = sm.canonicalAgentName(taskID, agentName)

	sm.inflightMu.Lock()
	group := sm.inflight[issueKey]
	if group == nil {
		sm.inflightMu.Unlock()
		sm.releaseIssueClaim(repo, issueNum)
		sm.recordStuckCandidate(issueKey, currentLabels)
		sm.logCompletionEvent(repo, issueNum, taskID, agentName, exitCode)
		return
	}

	taskKey := strings.TrimSpace(taskID)
	if taskKey != "" {
		if _, seen := group.completedTaskIDs[taskKey]; seen {
			sm.inflightMu.Unlock()
			sm.logCompletionEvent(repo, issueNum, taskID, agentName, exitCode)
			return
		}
		group.completedTaskIDs[taskKey] = struct{}{}
	}

	// Verify agent belongs to this dispatch group.
	if _, belongs := group.agents[agentName]; !belongs {
		sm.inflightMu.Unlock()
		log.Printf("[statemachine] warning: agent %q completed for %s but is not in inflight group, skipping update", agentName, issueKey)
		sm.logCompletionEvent(repo, issueNum, taskID, agentName, exitCode)
		return
	}

	agentCompletedAlready := false
	if _, seen := group.completedAgents[agentName]; seen {
		agentCompletedAlready = true
	}

	if !agentCompletedAlready {
		group.completedAgents[agentName] = struct{}{}
		if exitCode == 0 {
			group.successAgents[agentName] = struct{}{}
		} else {
			group.failedAgents[agentName] = struct{}{}
		}
	}

	// group.join is guaranteed normalized by dispatchStateAgents / newDispatchGroup.
	join := group.join

	shouldAdvance := false
	passed := false
	switch join {
	case config.JoinAnyPassed:
		if !agentCompletedAlready && exitCode == 0 {
			if len(group.successAgents) > 0 {
				shouldAdvance = true
				passed = true
			}
		}
		if !shouldAdvance && len(group.completedAgents) >= len(group.agents) {
			shouldAdvance = true
			passed = false
		}
	case config.JoinAllPassed:
		if !agentCompletedAlready && exitCode != 0 {
			shouldAdvance = true
			passed = false
		}
		if len(group.completedAgents) >= len(group.agents) {
			shouldAdvance = true
			if len(group.failedAgents) == 0 {
				passed = true
			} else {
				passed = false
			}
		}
	default:
		if len(group.completedAgents) >= len(group.agents) {
			shouldAdvance = true
			passed = len(group.failedAgents) == 0
		}
	}

	if shouldAdvance {
		delete(sm.inflight, issueKey)
	}
	sm.inflightMu.Unlock()

	if shouldAdvance {
		sm.releaseIssueClaim(repo, issueNum)
	}

	sm.logCompletionEvent(repo, issueNum, taskID, agentName, exitCode)

	if shouldAdvance {
		if passed {
			// Evaluate the next transition only when this group is complete.
			if transitioned := sm.evaluateCompletionTransitions(context.Background(), repo, issueNum, group.workflow, group.state, currentLabels); !transitioned {
				sm.recordStuckCandidate(issueKey, currentLabels)
			}
		} else {
			sm.eventlog.Log(eventlog.TypeTransitionToFailed, repo, issueNum,
				map[string]any{"state": group.state, "issue": issueNum, "join": join})
			sm.publishAlert(alertbus.KindTransitionToFailed, alertbus.SeverityError, repo, issueNum, "", map[string]any{
				"state": group.state,
				"join":  join,
			})
			sm.recordStuckCandidate(issueKey, currentLabels)
		}
	}
}

// evaluateCompletionTransitions replays transition evaluation using current labels
// and completion event semantics.
func (sm *StateMachine) evaluateCompletionTransitions(ctx context.Context, repo string, issueNum int, workflowName, sourceState string, currentLabels []string) bool {
	event := ChangeEvent{Type: poller.EventLabelAdded, Repo: repo, IssueNum: issueNum, Labels: currentLabels}
	matched := sm.findMatchingWorkflows(event)
	if len(matched) != 1 {
		return false
	}
	if matched[0].Name != workflowName {
		return false
	}
	wf := matched[0]
	currentState := wf.States[sourceState]
	if currentState == nil {
		return false
	}

	for _, label := range currentLabels {
		event.Detail = label
		wasTransitioned, err := sm.evaluateTransitions(ctx, wf, event, sourceState, currentState)
		if err != nil {
			log.Printf("[statemachine] completion transition failed: %v", err)
			return false
		}
		if wasTransitioned {
			return true
		}
	}
	return false
}

func (sm *StateMachine) logCompletionEvent(repo string, issueNum int, taskID, agentName string, exitCode int) {
	sm.eventlog.Log(eventlog.TypeCompleted, repo, issueNum, map[string]any{
		"task_id":    taskID,
		"agent_name": agentName,
		"exit_code":  exitCode,
	})
}

func (sm *StateMachine) canonicalAgentName(taskID, fallback string) string {
	if sm.store == nil || strings.TrimSpace(taskID) == "" {
		return fallback
	}
	task, err := sm.store.GetTask(taskID)
	if err != nil || task == nil || strings.TrimSpace(task.AgentName) == "" {
		return fallback
	}
	return task.AgentName
}

func (sm *StateMachine) recordStuckCandidate(issueKey string, labels []string) {
	labelsJSON, _ := json.Marshal(labels)
	sm.completionMu.Lock()
	sm.completionTimes[issueKey] = completionRecord{
		at:     time.Now(),
		labels: string(labelsJSON),
	}
	sm.completionMu.Unlock()
}

// CheckStuck examines issues whose agents completed but labels haven't changed.
// Should be called periodically (e.g. each poll cycle).
func (sm *StateMachine) CheckStuck(repo string, issueNum int, currentLabels []string) {
	issueKey := sm.issueKey(repo, issueNum)

	sm.completionMu.Lock()
	rec, exists := sm.completionTimes[issueKey]
	sm.completionMu.Unlock()

	if !exists {
		return
	}

	if time.Since(rec.at) < sm.stuckTimeout {
		return
	}

	// Check if labels changed since completion.
	currentJSON, _ := json.Marshal(currentLabels)
	if string(currentJSON) == rec.labels {
		// Labels unchanged after timeout — stuck!
		sm.eventlog.Log(eventlog.TypeStuckDetected, repo, issueNum,
			map[string]interface{}{"since": rec.at.Format(time.RFC3339), "labels": json.RawMessage(rec.labels)})
		sm.publishAlert(alertbus.KindStuckDetected, alertbus.SeverityWarn, repo, issueNum, "", map[string]any{
			"since":  rec.at.Format(time.RFC3339),
			"labels": rec.labels,
		})

		// Clear the record so we don't keep firing.
		sm.completionMu.Lock()
		delete(sm.completionTimes, issueKey)
		sm.completionMu.Unlock()
	} else {
		// Labels changed — not stuck, clear record.
		sm.completionMu.Lock()
		delete(sm.completionTimes, issueKey)
		sm.completionMu.Unlock()
	}
}

// CheckAllStuck evaluates all open cached issues for the configured stuck timeout.
func (sm *StateMachine) CheckAllStuck(repo string) {
	if sm.store == nil {
		return
	}
	openIssues, err := sm.store.ListIssueCaches(repo)
	if err != nil {
		return
	}
	for _, cached := range openIssues {
		if cached.State != "open" {
			continue
		}
		var labels []string
		if err := json.Unmarshal([]byte(cached.Labels), &labels); err != nil {
			continue
		}
		sm.CheckStuck(cached.Repo, cached.IssueNum, labels)
	}
}

// IsInflight returns true if an agent state group is currently running for the given issue.
func (sm *StateMachine) IsInflight(repo string, issueNum int) bool {
	issueKey := sm.issueKey(repo, issueNum)
	sm.inflightMu.Lock()
	defer sm.inflightMu.Unlock()
	_, ok := sm.inflight[issueKey]
	return ok
}

func (sm *StateMachine) stateAgents(state *config.State) []string {
	if len(state.Agents) > 0 {
		return state.Agents
	}
	if state.Agent == "" {
		return nil
	}
	return []string{state.Agent}
}

func (sm *StateMachine) stateHasAgents(state *config.State) bool {
	return len(sm.stateAgents(state)) > 0
}

func (sm *StateMachine) issueKey(repo string, issueNum int) string {
	return fmt.Sprintf("%s#%d", repo, issueNum)
}
