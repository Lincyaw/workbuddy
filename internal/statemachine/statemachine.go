package statemachine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Lincyaw/workbuddy/internal/alertbus"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/dependency"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/poller"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
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
	Repo           string
	IssueNum       int
	AgentName      string
	Workflow       string
	State          string
	SourceState    string
	RolloutIndex   int
	RolloutsTotal  int
	RolloutGroupID string
}

// EventRecorder abstracts event logging so tests can use a fake.
type EventRecorder interface {
	Log(eventType, repo string, issueNum int, payload interface{})
}

// StuckTimeout is the default duration after which an issue is considered stuck
// if a label hasn't changed after an agent completes.
const StuckTimeout = 5 * time.Minute

var defaultJoinConfig = config.JoinConfig{Strategy: config.JoinAllPassed}

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

// DefaultMaxReviewCycles is the orchestrator-level cap on dev↔review
// round-trips (developing→reviewing→developing) used when a workflow does
// not specify max_review_cycles in its frontmatter. See REQ-085.
const DefaultMaxReviewCycles = 3
const DefaultMaxSynthCycles = 2

// State names recognized by the dev↔review cycle counter. These match the
// canonical default workflow shipped in `.github/workbuddy/workflows/default.md`.
const (
	stateNameDeveloping = "developing"
	stateNameReviewing  = "reviewing"
	stateNameBlocked    = "blocked"
)

const (
	inflightTaskStatusGone        = "gone"
	inflightLeaseExpiryMultiplier = 5
	defaultTaskLease              = 30 * time.Second
)

// CycleCapReporter is the optional callback used by the StateMachine to post
// a needs-human comment with the rejection-trail digest when an issue trips
// the dev↔review cycle cap. The interface is satisfied by *reporter.Reporter
// in production wiring; tests pass a fake.
type CycleCapReporter interface {
	ReportDevReviewCycleCap(ctx context.Context, repo string, issueNum int, info CycleCapInfo) error
}

// SynthesisNeedsHumanReporter posts a coordinator-side needs-human comment
// when synthesize mode produced malformed or missing structured output.
type SynthesisNeedsHumanReporter interface {
	ReportSynthesisNeedsHuman(ctx context.Context, repo string, issueNum int, reason string) error
}

// CycleCapInfo carries the data the Reporter needs to assemble the
// needs-human comment posted on cap-hit. The rejection-trail digest is
// expected to be assembled by Coordinator Go code (no agent re-invocation).
type CycleCapInfo struct {
	WorkflowName    string
	CycleCount      int
	MaxReviewCycles int
	HitAt           time.Time
}

type dispatchGroup struct {
	workflow string
	state    string
	mode     string
	join     config.JoinConfig
	slots    map[string]string // dispatch slot key -> agent name

	dispatchedSlots  map[string]struct{}
	completedTaskIDs map[string]struct{}
	completedSlots   map[string]struct{}
	successSlots     map[string]struct{}
	failedSlots      map[string]struct{}
}

func newDispatchGroup(wf, state, mode string, join config.JoinConfig, slots map[string]string) *dispatchGroup {
	g := &dispatchGroup{
		workflow:         wf,
		state:            state,
		mode:             mode,
		join:             join,
		slots:            make(map[string]string, len(slots)),
		dispatchedSlots:  make(map[string]struct{}, len(slots)),
		completedTaskIDs: make(map[string]struct{}),
		completedSlots:   make(map[string]struct{}, len(slots)),
		successSlots:     make(map[string]struct{}, len(slots)),
		failedSlots:      make(map[string]struct{}, len(slots)),
	}
	for key, agentName := range slots {
		g.slots[key] = agentName
	}
	return g
}

// InflightGroupSnapshot is the operator-facing identity of one tracked
// in-memory dispatch group.
type InflightGroupSnapshot struct {
	Workflow string            `json:"workflow"`
	State    string            `json:"state"`
	Join     string            `json:"join"`
	Agents   []string          `json:"agents"`
	Slots    map[string]string `json:"slots"`
}

func snapshotDispatchGroup(group *dispatchGroup) *InflightGroupSnapshot {
	if group == nil {
		return nil
	}
	agents := make([]string, 0, len(group.slots))
	seen := make(map[string]struct{}, len(group.slots))
	slots := make(map[string]string, len(group.slots))
	for slotKey, agentName := range group.slots {
		slots[slotKey] = agentName
		if _, ok := seen[agentName]; ok {
			continue
		}
		seen[agentName] = struct{}{}
		agents = append(agents, agentName)
	}
	sort.Strings(agents)
	return &InflightGroupSnapshot{
		Workflow: group.workflow,
		State:    group.state,
		Join:     group.join.Strategy,
		Agents:   agents,
		Slots:    slots,
	}
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

	// capReporter posts the needs-human comment when an issue trips the
	// dev↔review cycle cap. Optional; when nil, cap-hit still records an
	// event + alert but no GitHub comment is written.
	capReporter CycleCapReporter
	// synthNeedsHumanReporter posts the coordinator-side needs-human comment
	// when synthesize mode returns malformed or missing structured output.
	synthNeedsHumanReporter SynthesisNeedsHumanReporter

	// issueClaim configuration (REQ-057). When claimerID is empty, per-issue
	// claim acquisition is skipped — useful for tests that don't care and for
	// backwards compatibility with the existing NewStateMachine signature.
	// claimTokens tracks the active claim token per issue so MarkAgentCompleted
	// can release the lease it acquired.
	issueClaimerID  string
	issueClaimLease time.Duration
	claimTokensMu   sync.Mutex
	claimTokens     map[string]string // key: "repo#issueNum" → active claim token
	claimWarnedMu   sync.Mutex
	claimWarned     map[string]struct{} // per-poll-cycle warning dedup
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
		workflowManager = workflow.NewManager(st)
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
		claimWarned:     make(map[string]struct{}),
	}
}

// SetCycleCapReporter installs the callback used to post a needs-human
// comment when an issue trips the dev↔review cycle cap. Pass nil to disable
// the comment side-effect (event + alert still fire).
func (sm *StateMachine) SetCycleCapReporter(r CycleCapReporter) {
	sm.capReporter = r
}

// SetSynthesisNeedsHumanReporter installs the callback used to post a
// needs-human comment when synthesize mode returns malformed output. Pass nil
// to disable the comment side-effect (event + blocked transition still fire).
func (sm *StateMachine) SetSynthesisNeedsHumanReporter(r SynthesisNeedsHumanReporter) {
	sm.synthNeedsHumanReporter = r
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
	sm.claimWarnedMu.Lock()
	sm.claimWarned = make(map[string]struct{})
	sm.claimWarnedMu.Unlock()
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
		// REQ #255 scenario A: no workflow trigger label matched. If the
		// issue carries a status:* label, the user has configured "this
		// issue should be in a state" but the trigger label is missing,
		// so the state machine would silently skip it. Surface the
		// condition as an INFO event + persistent hazard so it shows up
		// in `workbuddy status` and `workbuddy diagnose`.
		if event.IssueNum > 0 && event.Type != poller.EventPollCycleDone {
			sm.detectNoWorkflowMatchHazard(event)
		}
		return nil
	}
	// A workflow trigger label matched — clear any prior scenario A
	// hazard for this issue so cleared conditions don't linger.
	if event.IssueNum > 0 {
		sm.clearHazardIfKind(event.Repo, event.IssueNum, store.HazardKindNoWorkflowMatch)
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
	priorStateName := sm.queryPriorWorkflowState(wf.Name, event.Repo, event.IssueNum)
	if currentState == nil {
		// REQ #255 scenario B: workflow trigger label is present but no
		// state label matched. If the issue body declares a depends_on
		// block, the state machine cannot enter the issue and the
		// dependency gate cannot release downstream work. Surface as a
		// persistent hazard so the user sees they need to add a status:*
		// label (or status:blocked) for the gate to evaluate.
		if event.IssueNum > 0 {
			sm.detectDependencyUnenteredHazard(wf, event)
		}
		return nil
	}
	// State label is present — clear any prior scenario B hazard so the
	// cleared condition doesn't linger in `workbuddy status`.
	if event.IssueNum > 0 {
		sm.clearHazardIfKind(event.Repo, event.IssueNum, store.HazardKindAwaitingStatusLabel)
	}

	// State-entry detection: dispatch the state agents if:
	// 1. label_added matches the current state's enter_label (label just changed), OR
	// 2. issue_created and the issue already has a state label with agents (first seen)
	stateEntryDetected := (event.Type == poller.EventLabelAdded && event.Detail == currentState.EnterLabel) ||
		(event.Type == poller.EventIssueCreated)
	if stateEntryDetected && sm.stateHasAgents(currentState) {
		log.Printf("[statemachine] state entry detected: %s#%d entered %q, candidate agents=%q",
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

		// REQ-085: orchestrator-level dev↔review cycle counter and cap.
		// Production state changes flow through the state-entry branch (agents
		// flip labels atomically), so this is where we count round-trips.
		blocked, err := sm.applyDevReviewCycleCap(ctx, wf, event.Repo, event.IssueNum, currentStateName)
		if err != nil {
			return err
		}
		if blocked {
			return nil
		}

		// Persist the new current state so future cycles can compare against it.
		sm.recordStateEntry(wf.Name, event.Repo, event.IssueNum, currentStateName, currentState)

		// Touch first_dispatch_at so the long-flight stuck detector knows when
		// the issue first received an agent dispatch.
		if sm.store != nil {
			if err := sm.store.TouchIssueFirstDispatch(event.Repo, event.IssueNum); err != nil {
				log.Printf("[statemachine] touch first dispatch for %s#%d: %v", event.Repo, event.IssueNum, err)
			}
		}

		return sm.dispatchStateAgents(ctx, event.Repo, event.IssueNum, wf, priorStateName, currentStateName, currentState, event.Labels)
	}

	if stateEntryDetected {
		// Stateless state-entry (e.g. status:blocked, status:done): no agent
		// dispatch, but still advance the persisted workflow_instance so
		// subsequent state-aware checks (in particular the dev↔review cycle
		// counter's blocked→developing reset) see the correct prior state.
		sm.recordStateEntry(wf.Name, event.Repo, event.IssueNum, currentStateName, currentState)
		return nil
	}

	_, err := sm.evaluateTransitions(ctx, wf, event, currentStateName, currentState)
	return err
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
//
// Transitions are now a `label → target_state` map. We only fire on label_added
// events, picking the target via direct map lookup keyed on the added label.
func (sm *StateMachine) evaluateTransitions(ctx context.Context, wf *config.WorkflowConfig, event ChangeEvent, currentStateName string, currentState *config.State) (bool, error) {
	if event.Type != poller.EventLabelAdded {
		return false, nil
	}
	addedLabel := event.Detail
	if addedLabel == "" {
		return false, nil
	}

	targetStateName, ok := currentState.Transitions[addedLabel]
	if !ok {
		return false, nil
	}
	if !transitionAllowedForState(currentStateName, currentState, addedLabel, targetStateName, event.Labels) {
		return false, nil
	}

	targetState, ok := wf.States[targetStateName]
	if !ok {
		log.Printf("[statemachine] warning: transition target %q not found in workflow %q", targetStateName, wf.Name)
		return false, nil
	}

	// Check if this is a back-edge (target state was visited before).
	if sm.isBackEdge(event.Repo, event.IssueNum, targetStateName) {
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

			// Mark as failed — dispatch request not sent. Go code doesn't
			// write labels (agents do); we record the intent.
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
		if _, err := sm.store.IncrementTransition(event.Repo, event.IssueNum, currentStateName, targetStateName); err != nil {
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
		if err := sm.dispatchStateAgents(ctx, event.Repo, event.IssueNum, wf, currentStateName, targetStateName, targetState, event.Labels); err != nil {
			return true, err
		}
	}

	return true, nil
}

// applyDevReviewCycleCap detects a developing→reviewing→developing round-trip
// and enforces the workflow's max_review_cycles cap (REQ-085). Returns
// (blocked=true) when the cap is hit and dispatch should NOT proceed; the
// caller is responsible for halting state-entry dispatch in that case.
//
// Cycle detection rule: a "round-trip" is recorded each time the issue
// re-enters the developing state after the workflow_instance's persisted
// current_state was reviewing. The very first entry into developing is
// not counted (no prior reviewing state exists).
//
// Option A reset (REQ-085 maintainer override): when the prior workflow
// state is "blocked" — i.e. a human has flipped status:blocked →
// status:developing to give the issue another shot — the dev↔review cycle
// counter is reset so the cap re-engages from scratch. The blocked→developing
// label transition itself does not count as a round-trip.
func (sm *StateMachine) applyDevReviewCycleCap(ctx context.Context, wf *config.WorkflowConfig, repo string, issueNum int, currentStateName string) (bool, error) {
	if currentStateName != stateNameDeveloping {
		return false, nil
	}
	if sm.store == nil || sm.workflowManager == nil {
		return false, nil
	}
	priorState := sm.queryPriorWorkflowState(wf.Name, repo, issueNum)

	// Option A counter reset: a human-driven blocked→developing transition
	// clears the cycle counter and the cap-hit marker so future round-trips
	// start fresh. We rely on the workflow_instance state advancing through
	// "blocked" via processWorkflowEvent's stateless state-entry branch.
	if priorState == stateNameBlocked {
		if err := sm.store.ResetIssueCycleState(repo, issueNum); err != nil {
			log.Printf("[statemachine] reset issue cycle state on blocked→developing for %s#%d: %v", repo, issueNum, err)
			return false, fmt.Errorf("statemachine: reset issue cycle state: %w", err)
		}
		sm.eventlog.Log(eventlog.TypeDevReviewCycleCountReset, repo, issueNum, map[string]any{
			"workflow":    wf.Name,
			"prior_state": priorState,
			"reason":      "blocked_to_developing",
		})
		return false, nil
	}

	if priorState != stateNameReviewing {
		return false, nil
	}

	reviewSource := sm.latestTransitionIntoState(wf.Name, repo, issueNum, stateNameReviewing)
	if reviewSource == "synthesizing" {
		return sm.applySynthCycleCap(ctx, wf, repo, issueNum)
	}

	cycleCount, err := sm.store.IncrementDevReviewCycleCount(repo, issueNum)
	if err != nil {
		return false, fmt.Errorf("statemachine: increment dev_review_cycle_count: %w", err)
	}

	maxCycles := wf.MaxReviewCycles
	if maxCycles <= 0 {
		maxCycles = DefaultMaxReviewCycles
	}

	payload := map[string]any{
		"workflow":          wf.Name,
		"cycle_count":       cycleCount,
		"max_review_cycles": maxCycles,
	}
	sm.eventlog.Log(eventlog.TypeDevReviewCycleCount, repo, issueNum, payload)

	if cycleCount >= maxCycles {
		if err := sm.store.MarkIssueCycleCapHit(repo, issueNum); err != nil {
			log.Printf("[statemachine] mark issue cycle cap hit for %s#%d: %v", repo, issueNum, err)
		}
		sm.eventlog.Log(eventlog.TypeDevReviewCycleCapReached, repo, issueNum, payload)
		sm.publishAlert(alertbus.KindDevReviewCycleCapReached, alertbus.SeverityError, repo, issueNum, "", payload)
		if sm.capReporter != nil {
			info := CycleCapInfo{
				WorkflowName:    wf.Name,
				CycleCount:      cycleCount,
				MaxReviewCycles: maxCycles,
				HitAt:           time.Now().UTC(),
			}
			if err := sm.capReporter.ReportDevReviewCycleCap(ctx, repo, issueNum, info); err != nil {
				log.Printf("[statemachine] report cycle cap for %s#%d: %v", repo, issueNum, err)
			}
		}
		return true, nil
	}

	if cycleCount == maxCycles-1 {
		warnPayload := map[string]any{
			"workflow":          wf.Name,
			"cycle_count":       cycleCount,
			"max_review_cycles": maxCycles,
			"remaining":         maxCycles - cycleCount,
		}
		sm.eventlog.Log(eventlog.TypeDevReviewCycleApproaching, repo, issueNum, warnPayload)
		sm.publishAlert(alertbus.KindDevReviewCycleApproaching, alertbus.SeverityWarn, repo, issueNum, "", warnPayload)
	}
	return false, nil
}

func (sm *StateMachine) applySynthCycleCap(ctx context.Context, wf *config.WorkflowConfig, repo string, issueNum int) (bool, error) {
	cycleCount, err := sm.store.IncrementSynthCycleCount(repo, issueNum)
	if err != nil {
		return false, fmt.Errorf("statemachine: increment synth_cycle_count: %w", err)
	}
	maxCycles := DefaultMaxSynthCycles
	payload := map[string]any{
		"workflow":          wf.Name,
		"synth_cycle_count": cycleCount,
		"max_synth_cycles":  maxCycles,
	}
	if cycleCount >= maxCycles {
		if err := sm.store.MarkIssueSynthCycleCapHit(repo, issueNum); err != nil {
			log.Printf("[statemachine] mark synth cycle cap hit for %s#%d: %v", repo, issueNum, err)
		}
		sm.eventlog.Log(eventlog.TypeDevReviewCycleCapReached, repo, issueNum, payload)
		if sm.capReporter != nil {
			info := CycleCapInfo{
				WorkflowName:    wf.Name,
				CycleCount:      cycleCount,
				MaxReviewCycles: maxCycles,
				HitAt:           time.Now().UTC(),
			}
			if err := sm.capReporter.ReportDevReviewCycleCap(ctx, repo, issueNum, info); err != nil {
				log.Printf("[statemachine] report synth cycle cap for %s#%d: %v", repo, issueNum, err)
			}
		}
		return true, nil
	}
	return false, nil
}

// recordStateEntry advances the persisted workflow_instance current_state
// when a state-entry causes a real transition. Idempotent for self-entries.
func (sm *StateMachine) recordStateEntry(workflowName, repo string, issueNum int, currentStateName string, currentState *config.State) {
	if sm.workflowManager == nil {
		return
	}
	prior := sm.queryPriorWorkflowState(workflowName, repo, issueNum)
	if prior == "" || prior == currentStateName {
		return
	}
	triggerAgent := ""
	if currentState != nil {
		triggerAgent = currentState.Agent
	}
	if err := sm.workflowManager.Advance(repo, issueNum, workflowName, prior, currentStateName, triggerAgent); err != nil {
		log.Printf("[statemachine] advance workflow instance %s#%d %s→%s: %v", repo, issueNum, prior, currentStateName, err)
		return
	}
	if _, err := sm.store.IncrementTransition(repo, issueNum, prior, currentStateName); err != nil {
		log.Printf("[statemachine] increment transition %s#%d %s→%s: %v", repo, issueNum, prior, currentStateName, err)
	}
}

// queryPriorWorkflowState returns the persisted current_state for the issue
// before the present state-entry. Empty string if the workflow instance does
// not exist or its persisted state cannot be read.
func (sm *StateMachine) queryPriorWorkflowState(workflowName, repo string, issueNum int) string {
	if sm.workflowManager == nil {
		return ""
	}
	instances, err := sm.workflowManager.QueryByRepoIssue(repo, issueNum)
	if err != nil {
		log.Printf("[statemachine] query workflow instances %s#%d: %v", repo, issueNum, err)
		return ""
	}
	for _, inst := range instances {
		if inst.WorkflowName == workflowName {
			return inst.CurrentState
		}
	}
	return ""
}

func (sm *StateMachine) latestTransitionIntoState(workflowName, repo string, issueNum int, toState string) string {
	if sm.workflowManager == nil {
		return ""
	}
	instances, err := sm.workflowManager.QueryByRepoIssue(repo, issueNum)
	if err != nil {
		return ""
	}
	for _, inst := range instances {
		if inst.WorkflowName != workflowName {
			continue
		}
		for i := len(inst.History) - 1; i >= 0; i-- {
			if inst.History[i].To == toState {
				return inst.History[i].From
			}
		}
	}
	return ""
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
	sm.reconcileStaleInflight(repo, issueNum)
	if blocked, err := sm.isBlockedByIssueClaim(repo, issueNum, agentName, workflow, state); err != nil {
		return err
	} else if blocked {
		return nil
	}
	return sm.dispatchSingleAgent(ctx, DispatchRequest{Repo: repo, IssueNum: issueNum, AgentName: agentName, Workflow: workflow, State: state})
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
			warnKey := sm.issueKey(repo, issueNum)
			sm.claimWarnedMu.Lock()
			_, alreadyWarned := sm.claimWarned[warnKey]
			if !alreadyWarned {
				sm.claimWarned[warnKey] = struct{}{}
			}
			sm.claimWarnedMu.Unlock()
			if !alreadyWarned {
				log.Printf("[statemachine] WARN dispatch blocked by issue claim for %s#%d claimer=%s held_by=%s workflow=%s state=%s", repo, issueNum, sm.issueClaimerID, other, workflow, state)
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
	token := sm.claimTokens[key]
	delete(sm.claimTokens, key)
	sm.claimTokensMu.Unlock()
	if token == "" {
		return
	}
	if _, err := sm.store.ReleaseIssueClaim(repo, issueNum, sm.issueClaimerID, token); err != nil {
		log.Printf("[statemachine] release issue claim for %s#%d failed: %v", repo, issueNum, err)
	}
}

// ReleaseAllIssueClaims drops every currently tracked claim for this state
// machine instance. Used during coordinator shutdown so a clean restart does
// not inherit stale coordinator-owned claims.
func (sm *StateMachine) ReleaseAllIssueClaims() {
	if sm.store == nil || sm.issueClaimerID == "" {
		return
	}
	sm.claimTokensMu.Lock()
	tracked := make(map[string]string, len(sm.claimTokens))
	for key, token := range sm.claimTokens {
		tracked[key] = token
	}
	sm.claimTokens = make(map[string]string)
	sm.claimTokensMu.Unlock()

	for key, token := range tracked {
		if token == "" {
			continue
		}
		repo, issueNum, ok := parseIssueKey(key)
		if !ok {
			log.Printf("[statemachine] release all issue claims: invalid issue key %q", key)
			continue
		}
		if _, err := sm.store.ReleaseIssueClaim(repo, issueNum, sm.issueClaimerID, token); err != nil {
			log.Printf("[statemachine] release issue claim for %s#%d during shutdown failed: %v", repo, issueNum, err)
		}
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
		log.Printf("[statemachine] dispatch blocked by dependency: %s#%d agent=%s verdict=%s",
			repo, issueNum, agentForLog, depState.Verdict)
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
func rolloutSlotKey(index int) string {
	return fmt.Sprintf("rollout:%d", index)
}

func dispatchSlotKey(req DispatchRequest) string {
	if req.RolloutIndex > 0 {
		return rolloutSlotKey(req.RolloutIndex)
	}
	return req.AgentName
}

func (sm *StateMachine) dispatchSingleAgent(ctx context.Context, req DispatchRequest) error {
	repo := req.Repo
	issueNum := req.IssueNum
	agentName := req.AgentName
	workflow := req.Workflow
	state := req.State
	issueKey := sm.issueKey(repo, issueNum)
	slotKey := dispatchSlotKey(req)

	sm.inflightMu.Lock()
	if existing, ok := sm.inflight[issueKey]; ok {
		if existing.workflow != workflow || existing.state != state {
			sm.inflightMu.Unlock()
			taskStatus, _ := sm.inspectInflightTaskStatus(repo, issueNum, existing)
			sm.eventlog.Log(eventlog.TypeDispatchSkippedInflight, repo, issueNum,
				map[string]string{"agent": agentName, "reason": "different state/workflow already running", "state": state, "task_status": taskStatus, "workflow": workflow})
			return nil
		}
		// Same workflow+state: block if this slot was already dispatched.
		if _, dispatched := existing.dispatchedSlots[slotKey]; dispatched {
			sm.inflightMu.Unlock()
			taskStatus, _ := sm.inspectInflightTaskStatus(repo, issueNum, existing)
			sm.eventlog.Log(eventlog.TypeDispatchSkippedInflight, repo, issueNum,
				map[string]string{"agent": agentName, "reason": "agent already dispatched", "state": state, "task_status": taskStatus, "workflow": workflow})
			return nil
		}
		existing.dispatchedSlots[slotKey] = struct{}{}
	} else {
		sm.inflight[issueKey] = newDispatchGroup(workflow, state, config.StateModeReview, defaultJoinConfig, map[string]string{slotKey: agentName})
		sm.inflight[issueKey].dispatchedSlots[slotKey] = struct{}{}
	}
	sm.inflightMu.Unlock()

	sm.eventlog.Log(eventlog.TypeDispatch, repo, issueNum, map[string]any{
		"agent_name":     agentName,
		"workflow":       workflow,
		"state":          state,
		"rollout_index":  req.RolloutIndex,
		"rollouts_total": req.RolloutsTotal,
		"group_id":       req.RolloutGroupID,
	})
	if req.RolloutIndex > 0 && sm.eventlog != nil {
		sm.eventlog.Log(eventlog.TypeRolloutDispatched, repo, issueNum, map[string]any{
			"agent_name":     agentName,
			"workflow":       workflow,
			"state":          state,
			"rollout_index":  req.RolloutIndex,
			"rollouts_total": req.RolloutsTotal,
			"group_id":       req.RolloutGroupID,
		})
	}
	select {
	case sm.dispatch <- req:
		log.Printf("[statemachine] dispatched %s for %s#%d state=%s", agentName, repo, issueNum, state)
	case <-ctx.Done():
		// Context cancelled before dispatch sent. Remove this agent from
		// the group. If no agents were successfully dispatched, clean up
		// the entire group.
		released := false
		sm.inflightMu.Lock()
		if g, ok := sm.inflight[issueKey]; ok {
			delete(g.dispatchedSlots, slotKey)
			if len(g.dispatchedSlots) == 0 {
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

func (sm *StateMachine) dispatchStateAgents(ctx context.Context, repo string, issueNum int, wf *config.WorkflowConfig, sourceState, state string, stateDef *config.State, labels []string) error {
	agents := sm.stateAgents(stateDef)
	if len(agents) == 0 {
		return nil
	}
	wfName := wf.Name

	// Check dependency once for all agents in this state.
	if blocked, err := sm.isBlockedByDependency(repo, issueNum, agents[0]); err != nil {
		return err
	} else if blocked {
		return nil
	}
	sm.reconcileStaleInflight(repo, issueNum)

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
	rollouts, err := resolveStateRollouts(stateDef, labels)
	if err != nil {
		return err
	}
	join := stateDef.Join // already normalized by config loader
	if strings.TrimSpace(join.Strategy) == "" {
		join = defaultJoinConfig
	}
	slots := make(map[string]string)
	requests := make([]DispatchRequest, 0)
	if rollouts > 1 {
		groupID := uuid.NewString()
		for idx := 1; idx <= rollouts; idx++ {
			req := DispatchRequest{
				Repo:           repo,
				IssueNum:       issueNum,
				AgentName:      agents[0],
				Workflow:       wfName,
				State:          state,
				SourceState:    sourceState,
				RolloutIndex:   idx,
				RolloutsTotal:  rollouts,
				RolloutGroupID: groupID,
			}
			slots[dispatchSlotKey(req)] = req.AgentName
			requests = append(requests, req)
		}
	} else if stateMode(stateDef) != config.StateModeSynth {
		inherited, err := sm.inheritedRolloutRequests(repo, issueNum, wf, sourceState, state, agents[0])
		if err != nil {
			return err
		}
		if len(inherited) > 0 {
			for _, req := range inherited {
				slots[dispatchSlotKey(req)] = req.AgentName
				requests = append(requests, req)
			}
		} else {
			for _, agent := range agents {
				req := DispatchRequest{
					Repo:        repo,
					IssueNum:    issueNum,
					AgentName:   agent,
					Workflow:    wfName,
					State:       state,
					SourceState: sourceState,
				}
				slots[dispatchSlotKey(req)] = req.AgentName
				requests = append(requests, req)
			}
		}
	} else {
		for _, agent := range agents {
			req := DispatchRequest{
				Repo:        repo,
				IssueNum:    issueNum,
				AgentName:   agent,
				Workflow:    wfName,
				State:       state,
				SourceState: sourceState,
			}
			slots[dispatchSlotKey(req)] = req.AgentName
			requests = append(requests, req)
		}
	}

	group := newDispatchGroup(wfName, state, stateMode(stateDef), join, slots)

	sm.inflightMu.Lock()
	if existing, ok := sm.inflight[issueKey]; ok {
		reason := "same state group already inflight"
		if existing.workflow != wfName || existing.state != state {
			reason = "different state/workflow already running"
		}
		sm.inflightMu.Unlock()
		taskStatus, _ := sm.inspectInflightTaskStatus(repo, issueNum, existing)
		sm.eventlog.Log(eventlog.TypeDispatchSkippedInflight, repo, issueNum,
			map[string]string{"state": state, "reason": reason, "task_status": taskStatus, "workflow": wfName})
		return nil
	}
	sm.inflight[issueKey] = group
	sm.inflightMu.Unlock()

	for _, req := range requests {
		if err := sm.dispatchSingleAgent(ctx, req); err != nil {
			// Don't remove the inflight group — agents already dispatched before
			// this failure are still running and need tracking. Log the error
			// and let those agents complete normally.
			log.Printf("[statemachine] partial dispatch failure for %s (agent %s): %v", issueKey, req.AgentName, err)
			return err
		}
	}
	return nil
}

func (sm *StateMachine) inheritedRolloutRequests(repo string, issueNum int, wf *config.WorkflowConfig, sourceState, targetState, agentName string) ([]DispatchRequest, error) {
	if sm.store == nil || wf == nil || sourceState == "" || sourceState == targetState {
		return nil, nil
	}
	sourceDef := wf.States[sourceState]
	if sourceDef == nil || strings.TrimSpace(sourceDef.Join.Strategy) != config.JoinRollouts {
		return nil, nil
	}
	summary, err := sm.store.LatestRolloutGroupSummaryForIssueState(repo, issueNum, wf.Name, sourceState)
	if err != nil {
		return nil, fmt.Errorf("statemachine: summarize inherited rollout group: %w", err)
	}
	if summary == nil || summary.SuccessCount == 0 {
		return nil, nil
	}
	requests := make([]DispatchRequest, 0, summary.SuccessCount)
	for _, task := range summary.Tasks {
		if task.Status != store.TaskStatusCompleted || task.RolloutIndex <= 0 {
			continue
		}
		requests = append(requests, DispatchRequest{
			Repo:           repo,
			IssueNum:       issueNum,
			AgentName:      agentName,
			Workflow:       wf.Name,
			State:          targetState,
			SourceState:    sourceState,
			RolloutIndex:   task.RolloutIndex,
			RolloutsTotal:  task.RolloutsTotal,
			RolloutGroupID: task.RolloutGroupID,
		})
	}
	sort.Slice(requests, func(i, j int) bool {
		return requests[i].RolloutIndex < requests[j].RolloutIndex
	})
	return requests, nil
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
	sm.MarkAgentCompletedWithDecision(repo, issueNum, taskID, agentName, exitCode, currentLabels, nil)
}

func (sm *StateMachine) MarkAgentCompletedWithDecision(repo string, issueNum int, taskID, agentName string, exitCode int, currentLabels []string, decision *runtimepkg.SynthesisDecision) {
	issueKey := sm.issueKey(repo, issueNum)
	agentName = sm.canonicalAgentName(taskID, agentName)
	slotKey := agentName
	rolloutGroupID := ""
	rolloutIndex := 0
	rolloutsTotal := 1
	if sm.store != nil && strings.TrimSpace(taskID) != "" {
		if task, err := sm.store.GetTask(taskID); err == nil && task != nil {
			rolloutGroupID = task.RolloutGroupID
			rolloutIndex = task.RolloutIndex
			rolloutsTotal = task.RolloutsTotal
			if rolloutIndex > 0 {
				slotKey = rolloutSlotKey(rolloutIndex)
			}
		}
	}

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

	// Verify the completed slot belongs to this dispatch group.
	if _, belongs := group.slots[slotKey]; !belongs {
		sm.inflightMu.Unlock()
		log.Printf("[statemachine] warning: slot %q completed for %s but is not in inflight group, skipping update", slotKey, issueKey)
		sm.logCompletionEvent(repo, issueNum, taskID, agentName, exitCode)
		return
	}

	slotCompletedAlready := false
	if _, seen := group.completedSlots[slotKey]; seen {
		slotCompletedAlready = true
	}

	if !slotCompletedAlready {
		group.completedSlots[slotKey] = struct{}{}
		if exitCode == 0 {
			group.successSlots[slotKey] = struct{}{}
		} else {
			group.failedSlots[slotKey] = struct{}{}
		}
	}

	if rolloutIndex > 0 && sm.eventlog != nil {
		status := store.TaskStatusFailed
		if exitCode == 0 {
			status = store.TaskStatusCompleted
		}
		sm.eventlog.Log(eventlog.TypeRolloutCompleted, repo, issueNum, map[string]any{
			"agent_name":     agentName,
			"workflow":       group.workflow,
			"state":          group.state,
			"rollout_index":  rolloutIndex,
			"rollouts_total": rolloutsTotal,
			"group_id":       rolloutGroupID,
			"status":         status,
		})
	}

	shouldAdvance := false
	passed := false
	join := group.join.Strategy
	switch join {
	case config.JoinRollouts:
		if rolloutGroupID == "" {
			if len(group.completedSlots) >= len(group.slots) {
				shouldAdvance = true
				passed = len(group.failedSlots) == 0
			}
			break
		}
		summary, err := sm.store.SummarizeRolloutGroup(rolloutGroupID)
		if err != nil {
			log.Printf("[statemachine] rollout summary failed for %s: %v", rolloutGroupID, err)
			break
		}
		if summary != nil {
			minSuccesses := group.join.MinSuccesses
			if minSuccesses <= 0 {
				minSuccesses = summary.RolloutsTotal
			}
			if summary.TerminalCount >= summary.RolloutsTotal {
				shouldAdvance = true
				passed = summary.SuccessCount >= minSuccesses
			}
		}
	case config.JoinAnyPassed:
		if !slotCompletedAlready && exitCode == 0 {
			if len(group.successSlots) > 0 {
				shouldAdvance = true
				passed = true
			}
		}
		if !shouldAdvance && len(group.completedSlots) >= len(group.slots) {
			shouldAdvance = true
			passed = false
		}
	case config.JoinAllPassed:
		if !slotCompletedAlready && exitCode != 0 {
			shouldAdvance = true
			passed = false
		}
		if len(group.completedSlots) >= len(group.slots) {
			shouldAdvance = true
			if len(group.failedSlots) == 0 {
				passed = true
			} else {
				passed = false
			}
		}
	default:
		if len(group.completedSlots) >= len(group.slots) {
			shouldAdvance = true
			passed = len(group.failedSlots) == 0
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
	sm.logSynthesisDecision(repo, issueNum, group, exitCode, decision)

	if shouldAdvance {
		if stateModeFromGroup(group) == config.StateModeSynth && shouldEscalateSynthDecision(exitCode, decision) {
			if decision == nil && sm.synthNeedsHumanReporter != nil {
				if err := sm.synthNeedsHumanReporter.ReportSynthesisNeedsHuman(context.Background(), repo, issueNum, "malformed_or_missing_synthesis_output"); err != nil {
					log.Printf("[statemachine] synth needs-human comment failed for %s#%d: %v", repo, issueNum, err)
				}
			}
			if err := sm.advanceRolloutFailureToBlocked(repo, issueNum, group.workflow, group.state); err != nil {
				log.Printf("[statemachine] synth escalation transition failed for %s#%d: %v", repo, issueNum, err)
			}
			sm.recordStuckCandidate(issueKey, currentLabels)
			return
		}
		if passed {
			// Evaluate the next transition only when this group is complete.
			if transitioned := sm.evaluateCompletionTransitions(context.Background(), repo, issueNum, group.workflow, group.state, currentLabels); !transitioned {
				if stateModeFromGroup(group) == config.StateModeSynth {
					if err := sm.advanceRolloutFailureToBlocked(repo, issueNum, group.workflow, group.state); err != nil {
						log.Printf("[statemachine] synth malformed transition failed for %s#%d: %v", repo, issueNum, err)
					}
					return
				}
				sm.recordStuckCandidate(issueKey, currentLabels)
			}
		} else {
			sm.eventlog.Log(eventlog.TypeTransitionToFailed, repo, issueNum,
				map[string]any{"state": group.state, "issue": issueNum, "join": join, "needs_human": join == config.JoinRollouts})
			if join == config.JoinRollouts {
				log.Printf("[statemachine] rollout join failed for %s#%d state=%s group=%s", repo, issueNum, group.state, rolloutGroupID)
				if err := sm.advanceRolloutFailureToBlocked(repo, issueNum, group.workflow, group.state); err != nil {
					log.Printf("[statemachine] rollout join needs-human transition failed for %s#%d: %v", repo, issueNum, err)
				}
			}
			sm.publishAlert(alertbus.KindTransitionToFailed, alertbus.SeverityError, repo, issueNum, "", map[string]any{
				"state":       group.state,
				"join":        join,
				"needs_human": join == config.JoinRollouts,
			})
			sm.recordStuckCandidate(issueKey, currentLabels)
		}
	}
}

func (sm *StateMachine) logSynthesisDecision(repo string, issueNum int, group *dispatchGroup, exitCode int, decision *runtimepkg.SynthesisDecision) {
	if group == nil || stateModeFromGroup(group) != config.StateModeSynth || sm.eventlog == nil {
		return
	}
	if decision == nil {
		sm.eventlog.Log(eventlog.TypeSynthesisDecision, repo, issueNum, map[string]any{
			"outcome":      "escalate",
			"rejected_prs": []int{},
			"reason":       "malformed_or_missing_synthesis_output",
		})
		return
	}
	payload := map[string]any{
		"outcome":      decision.Outcome,
		"rejected_prs": decision.RejectedPRs,
		"reason":       decision.Reason,
	}
	switch decision.Outcome {
	case "pick":
		payload["chosen_pr"] = decision.ChosenPR
	case "cherry-pick":
		payload["synth_pr"] = decision.SynthPR
	}
	sm.eventlog.Log(eventlog.TypeSynthesisDecision, repo, issueNum, payload)
}

func shouldEscalateSynthDecision(exitCode int, decision *runtimepkg.SynthesisDecision) bool {
	if decision == nil {
		return true
	}
	return decision.Outcome == "escalate"
}

func stateModeFromGroup(group *dispatchGroup) string {
	if group == nil || strings.TrimSpace(group.mode) == "" {
		return config.StateModeReview
	}
	return group.mode
}

func (sm *StateMachine) advanceRolloutFailureToBlocked(repo string, issueNum int, workflowName, sourceState string) error {
	if sm.workflowManager == nil {
		return nil
	}
	wf, ok := sm.workflows[workflowName]
	if !ok || wf == nil {
		return nil
	}
	blockedState, ok := wf.States[stateNameBlocked]
	if !ok || blockedState == nil {
		return nil
	}
	if err := sm.workflowManager.Advance(repo, issueNum, workflowName, sourceState, stateNameBlocked, ""); err != nil {
		return fmt.Errorf("advance workflow state to blocked: %w", err)
	}
	if _, err := sm.store.IncrementTransition(repo, issueNum, sourceState, stateNameBlocked); err != nil {
		return fmt.Errorf("increment blocked transition: %w", err)
	}
	sm.eventlog.Log(eventlog.TypeTransition, repo, issueNum, map[string]string{"from": sourceState, "to": stateNameBlocked})
	return nil
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

// ClearInflight removes the in-memory inflight entry for one issue and returns
// the prior group identity. This is the manual escape hatch behind the
// coordinator admin API; ordinary recovery should come from Dispatch's
// task_queue-backed self-heal path.
func (sm *StateMachine) ClearInflight(repo string, issueNum int) (*InflightGroupSnapshot, bool) {
	issueKey := sm.issueKey(repo, issueNum)
	sm.inflightMu.Lock()
	group := sm.inflight[issueKey]
	if group == nil {
		sm.inflightMu.Unlock()
		return nil, false
	}
	snapshot := snapshotDispatchGroup(group)
	delete(sm.inflight, issueKey)
	sm.inflightMu.Unlock()
	sm.releaseIssueClaim(repo, issueNum)
	return snapshot, true
}

func (sm *StateMachine) reconcileStaleInflight(repo string, issueNum int) {
	issueKey := sm.issueKey(repo, issueNum)
	sm.inflightMu.Lock()
	group := sm.inflight[issueKey]
	sm.inflightMu.Unlock()
	if group == nil {
		return
	}
	taskStatus, stale := sm.inspectInflightTaskStatus(repo, issueNum, group)
	if !stale {
		return
	}
	if !sm.clearInflightIfMatch(issueKey, group) {
		return
	}
	sm.releaseIssueClaim(repo, issueNum)
	log.Printf("[statemachine] reclaimed stale inflight group for %s task_status=%s", issueKey, taskStatus)
}

func (sm *StateMachine) clearInflightIfMatch(issueKey string, target *dispatchGroup) bool {
	sm.inflightMu.Lock()
	defer sm.inflightMu.Unlock()
	current := sm.inflight[issueKey]
	if current == nil || current != target {
		return false
	}
	delete(sm.inflight, issueKey)
	return true
}

func (sm *StateMachine) inspectInflightTaskStatus(repo string, issueNum int, group *dispatchGroup) (string, bool) {
	if sm.store == nil || group == nil {
		return "", false
	}
	tasks, err := sm.store.ListIssueTasks(repo, issueNum)
	if err != nil {
		log.Printf("[statemachine] inspect inflight task status for %s#%d: %v", repo, issueNum, err)
		return "", false
	}
	slotKeys := make([]string, 0, len(group.slots))
	for slotKey := range group.slots {
		slotKeys = append(slotKeys, slotKey)
	}
	sort.Strings(slotKeys)

	statusBySlot := make(map[string]string, len(slotKeys))
	allReclaimable := len(slotKeys) > 0
	for _, slotKey := range slotKeys {
		task, ok := findInflightSlotTask(tasks, group, slotKey)
		if !ok {
			statusBySlot[slotKey] = inflightTaskStatusGone
			continue
		}
		statusBySlot[slotKey] = task.Status
		if inflightTaskKeepsGroupLive(task) {
			allReclaimable = false
		}
	}
	return formatInflightTaskStatus(slotKeys, statusBySlot), allReclaimable
}

func findInflightSlotTask(tasks []store.TaskRecord, group *dispatchGroup, slotKey string) (*store.TaskRecord, bool) {
	wantAgent := group.slots[slotKey]
	wantRollout := 0
	if strings.HasPrefix(slotKey, "rollout:") {
		idx, err := strconv.Atoi(strings.TrimPrefix(slotKey, "rollout:"))
		if err != nil {
			return nil, false
		}
		wantRollout = idx
	}

	var best *store.TaskRecord
	for i := range tasks {
		task := &tasks[i]
		if task.Workflow != group.workflow || task.State != group.state {
			continue
		}
		if wantRollout > 0 {
			if task.RolloutIndex != wantRollout {
				continue
			}
		} else {
			if task.RolloutIndex != 0 || task.AgentName != wantAgent {
				continue
			}
		}
		if best == nil || taskRecency(task).After(taskRecency(best)) {
			best = task
		}
	}
	if best == nil {
		return nil, false
	}
	return best, true
}

func taskRecency(task *store.TaskRecord) time.Time {
	if task == nil {
		return time.Time{}
	}
	if !task.UpdatedAt.IsZero() {
		return task.UpdatedAt
	}
	return task.CreatedAt
}

func inflightTaskKeepsGroupLive(task *store.TaskRecord) bool {
	if task == nil {
		return false
	}
	switch task.Status {
	case store.TaskStatusPending:
		return true
	case store.TaskStatusRunning:
		if task.LeaseExpiresAt.IsZero() {
			return true
		}
		lease := inferredTaskLease(task)
		return time.Now().UTC().Before(task.LeaseExpiresAt.Add(inflightLeaseExpiryMultiplier * lease))
	case store.TaskStatusCompleted:
		return true
	default:
		return false
	}
}

func inferredTaskLease(task *store.TaskRecord) time.Duration {
	if task == nil || task.LeaseExpiresAt.IsZero() {
		return defaultTaskLease
	}
	var leaseStart time.Time
	switch {
	case !task.HeartbeatAt.IsZero():
		leaseStart = task.HeartbeatAt
	case !task.AckedAt.IsZero():
		leaseStart = task.AckedAt
	}
	if leaseStart.IsZero() {
		return defaultTaskLease
	}
	lease := task.LeaseExpiresAt.Sub(leaseStart)
	if lease <= 0 {
		return defaultTaskLease
	}
	return lease
}

func formatInflightTaskStatus(slotKeys []string, statusBySlot map[string]string) string {
	if len(slotKeys) == 0 {
		return inflightTaskStatusGone
	}
	if len(slotKeys) == 1 {
		if status := strings.TrimSpace(statusBySlot[slotKeys[0]]); status != "" {
			return status
		}
		return inflightTaskStatusGone
	}
	parts := make([]string, 0, len(slotKeys))
	for _, slotKey := range slotKeys {
		status := strings.TrimSpace(statusBySlot[slotKey])
		if status == "" {
			status = inflightTaskStatusGone
		}
		parts = append(parts, fmt.Sprintf("%s=%s", slotKey, status))
	}
	return strings.Join(parts, ",")
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

func stateMode(state *config.State) string {
	if state == nil || strings.TrimSpace(state.Mode) == "" {
		return config.StateModeReview
	}
	return state.Mode
}

func transitionAllowedForState(currentStateName string, currentState *config.State, label, targetState string, labels []string) bool {
	if currentStateName != stateNameDeveloping || currentState == nil {
		return true
	}
	hasSynthTarget := false
	for _, candidate := range currentState.Transitions {
		if candidate == "synthesizing" {
			hasSynthTarget = true
			break
		}
	}
	if !hasSynthTarget {
		return true
	}
	rollouts, err := resolveStateRollouts(currentState, labels)
	if err != nil {
		return false
	}
	if targetState == "synthesizing" {
		return rollouts > 1
	}
	if targetState == stateNameReviewing && label == "status:reviewing" {
		return rollouts <= 1
	}
	return true
}

func resolveStateRollouts(state *config.State, labels []string) (int, error) {
	defaultRollouts := 1
	if state != nil && state.Rollouts > 0 {
		defaultRollouts = state.Rollouts
	}
	override := 0
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if !strings.HasPrefix(label, "rollouts:") {
			continue
		}
		switch label {
		case "rollouts:2", "rollouts:3", "rollouts:4", "rollouts:5":
			n, _ := strconv.Atoi(strings.TrimPrefix(label, "rollouts:"))
			if override != 0 && override != n {
				return 0, fmt.Errorf("statemachine: conflicting rollout labels: %q", label)
			}
			override = n
		default:
			return 0, fmt.Errorf("statemachine: invalid rollout label %q (expected literal rollouts:2..rollouts:5)", label)
		}
	}
	if override > 0 {
		return override, nil
	}
	return defaultRollouts, nil
}

func (sm *StateMachine) issueKey(repo string, issueNum int) string {
	return fmt.Sprintf("%s#%d", repo, issueNum)
}

// detectNoWorkflowMatchHazard records the scenario A hazard (issue has a
// status:* label but no workflow trigger label matched) idempotently per
// labels fingerprint. Caller must already have established len(matched) == 0.
func (sm *StateMachine) detectNoWorkflowMatchHazard(event ChangeEvent) {
	if sm.store == nil {
		return
	}
	if !hasStatusLabel(event.Labels) {
		// No status label → user hasn't asked the issue to enter the
		// pipeline yet. Not a hazard.
		return
	}
	if !sm.hasAnyWorkflow() {
		// No workflows configured at all (e.g. tests, fresh deploy
		// without configs). Treating this as a hazard would just be
		// noise.
		return
	}
	fingerprint := labelsFingerprint(event.Labels)
	hazard := store.PipelineHazard{
		Repo:        event.Repo,
		IssueNum:    event.IssueNum,
		Kind:        store.HazardKindNoWorkflowMatch,
		Fingerprint: fingerprint,
	}
	changed, err := sm.store.UpsertIssuePipelineHazard(hazard)
	if err != nil {
		log.Printf("[statemachine] upsert no-workflow-match hazard for %s#%d: %v", event.Repo, event.IssueNum, err)
		return
	}
	if !changed {
		return
	}
	triggers := sm.workflowTriggerLabels()
	sm.eventlog.Log(eventlog.TypeIssueNoWorkflowMatch, event.Repo, event.IssueNum, map[string]any{
		"labels":            append([]string(nil), event.Labels...),
		"workflow_triggers": triggers,
		"hint":              "add one of the workflow trigger labels (e.g. workbuddy) to opt this issue into the pipeline",
	})
}

// detectDependencyUnenteredHazard records the scenario B hazard (workflow
// trigger label present, depends_on declared in body, but no status:* label
// so the state machine cannot enter). Idempotent per (labels+depends_on)
// fingerprint.
func (sm *StateMachine) detectDependencyUnenteredHazard(wf *config.WorkflowConfig, event ChangeEvent) {
	if sm.store == nil {
		return
	}
	cache, err := sm.store.QueryIssueCache(event.Repo, event.IssueNum)
	if err != nil || cache == nil {
		return
	}
	decl := dependency.ParseDeclaration(event.Repo, cache.Body)
	if !decl.HasBlock {
		return
	}
	deps := make([]string, 0, len(decl.Dependencies))
	for _, d := range decl.Dependencies {
		ref := d.Normalized
		if ref == "" {
			ref = d.Raw
		}
		deps = append(deps, ref)
	}
	fingerprint := hazardFingerprint(labelsFingerprint(event.Labels), decl.SourceHash)
	hazard := store.PipelineHazard{
		Repo:        event.Repo,
		IssueNum:    event.IssueNum,
		Kind:        store.HazardKindAwaitingStatusLabel,
		Fingerprint: fingerprint,
	}
	changed, err := sm.store.UpsertIssuePipelineHazard(hazard)
	if err != nil {
		log.Printf("[statemachine] upsert awaiting-status-label hazard for %s#%d: %v", event.Repo, event.IssueNum, err)
		return
	}
	if !changed {
		return
	}
	sm.eventlog.Log(eventlog.TypeIssueDependencyUnentered, event.Repo, event.IssueNum, map[string]any{
		"workflow":   wf.Name,
		"reason":     "missing_status_label",
		"labels":     append([]string(nil), event.Labels...),
		"depends_on": deps,
		"hint":       "add status:blocked so the dependency gate can evaluate, or status:developing if the deps are already satisfied",
	})
}

// clearHazardIfKind removes a prior hazard row when its kind matches. We do
// not unconditionally delete: a different hazard kind may legitimately be
// present (e.g. a still-open scenario B even after a workflow trigger label
// match for an unrelated workflow), and clearing it would lose state.
func (sm *StateMachine) clearHazardIfKind(repo string, issueNum int, kind string) {
	if sm.store == nil {
		return
	}
	prev, err := sm.store.QueryIssuePipelineHazard(repo, issueNum)
	if err != nil || prev == nil {
		return
	}
	if prev.Kind != kind {
		return
	}
	if err := sm.store.ClearIssuePipelineHazard(repo, issueNum); err != nil {
		log.Printf("[statemachine] clear pipeline hazard for %s#%d: %v", repo, issueNum, err)
	}
}

func (sm *StateMachine) hasAnyWorkflow() bool {
	for _, wf := range sm.workflows {
		if wf != nil && strings.TrimSpace(wf.Trigger.IssueLabel) != "" {
			return true
		}
	}
	return false
}

func (sm *StateMachine) workflowTriggerLabels() []string {
	out := make([]string, 0, len(sm.workflows))
	for _, wf := range sm.workflows {
		if wf == nil {
			continue
		}
		label := strings.TrimSpace(wf.Trigger.IssueLabel)
		if label == "" {
			continue
		}
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

func hasStatusLabel(labels []string) bool {
	for _, l := range labels {
		if strings.HasPrefix(l, "status:") {
			return true
		}
	}
	return false
}

func labelsFingerprint(labels []string) string {
	sorted := append([]string(nil), labels...)
	sort.Strings(sorted)
	h := sha256.New()
	for _, l := range sorted {
		_, _ = h.Write([]byte(l))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hazardFingerprint(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func parseIssueKey(raw string) (string, int, bool) {
	idx := strings.LastIndex(raw, "#")
	if idx <= 0 || idx == len(raw)-1 {
		return "", 0, false
	}
	issueNum, err := strconv.Atoi(raw[idx+1:])
	if err != nil || issueNum <= 0 {
		return "", 0, false
	}
	return raw[:idx], issueNum, true
}
