package statemachine

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// ChangeEvent represents a state change detected by the Poller.
type ChangeEvent struct {
	Type     string // "label_added", "label_removed", "pr_created", etc.
	Repo     string
	IssueNum int
	Labels   []string // current labels on the issue
	Detail   string   // extra info (e.g. label name, comment body)
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

// StateMachine evaluates Poller events against workflow definitions,
// manages transitions, detects cycles, and dispatches agent tasks.
type StateMachine struct {
	workflows map[string]*config.WorkflowConfig
	store     *store.Store
	dispatch  chan<- DispatchRequest
	eventlog  EventRecorder

	// processedEvents tracks (repo, issueNum, eventKey) to ensure idempotency.
	processedEvents sync.Map // key: string → struct{}

	// inflightMu protects inflight map.
	inflightMu sync.Mutex
	// inflight tracks issues that have a running agent. key: "repo#issueNum"
	inflight map[string]bool

	// stuckTimeout configurable for tests; defaults to StuckTimeout.
	stuckTimeout time.Duration

	// completionTimes tracks when an agent finished for stuck detection.
	// key: "repo#issueNum" → (completionTime, labelsAtCompletion)
	completionMu    sync.Mutex
	completionTimes map[string]completionRecord
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
) *StateMachine {
	return &StateMachine{
		workflows:       workflows,
		store:           st,
		dispatch:        dispatch,
		eventlog:        eventlog,
		inflight:        make(map[string]bool),
		stuckTimeout:    StuckTimeout,
		completionTimes: make(map[string]completionRecord),
	}
}

// SetStuckTimeout overrides the stuck timeout (useful for tests).
func (sm *StateMachine) SetStuckTimeout(d time.Duration) {
	sm.stuckTimeout = d
}

// ResetDedup clears the processed-events deduplication set.
// Call this at the start of each poll cycle so that events from
// different cycles are not suppressed.
func (sm *StateMachine) ResetDedup() {
	sm.processedEvents = sync.Map{}
}

// HandleEvent processes a single ChangeEvent from the Poller.
func (sm *StateMachine) HandleEvent(event ChangeEvent) error {
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
		sm.eventlog.Log("error_multi_workflow", event.Repo, event.IssueNum, string(payload))
		return fmt.Errorf("statemachine: issue %s#%d matches %d workflows", event.Repo, event.IssueNum, len(matched))
	}

	wf := matched[0]
	return sm.processWorkflowEvent(wf, event)
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
func (sm *StateMachine) processWorkflowEvent(wf *config.WorkflowConfig, event ChangeEvent) error {
	// Determine current state from labels.
	currentStateName, currentState := sm.findCurrentState(wf, event.Labels)
	if currentState == nil {
		// Issue has the workflow trigger but no status label matching any state.
		// Could be initial state or misconfigured. Skip silently.
		return nil
	}

	// State-entry detection: dispatch the agent if:
	// 1. label_added matches the current state's enter_label (label just changed), OR
	// 2. issue_created and the issue already has a state label with an agent (first seen)
	stateEntryDetected := (event.Type == "label_added" && event.Detail == currentState.EnterLabel) ||
		(event.Type == "issue_created")
	if stateEntryDetected && currentState.Agent != "" {
		log.Printf("[statemachine] state entry detected: %s#%d entered %q, dispatching agent %q",
			event.Repo, event.IssueNum, currentStateName, currentState.Agent)
		sm.eventlog.Log("state_entry", event.Repo, event.IssueNum,
			fmt.Sprintf(`{"state":"%s","agent":"%s"}`, currentStateName, currentState.Agent))

		// Clear any stale inflight flag: if the label changed, the previous agent's
		// work is done (it was the one that changed the label). The worker goroutine
		// may not have called MarkAgentCompleted yet due to a race condition.
		issueKey := fmt.Sprintf("%s#%d", event.Repo, event.IssueNum)
		sm.inflightMu.Lock()
		delete(sm.inflight, issueKey)
		sm.inflightMu.Unlock()

		return sm.dispatchAgent(event.Repo, event.IssueNum, currentState.Agent, wf.Name, currentStateName)
	}

	// Build evaluation context.
	ctx := &EvalContext{
		EventType:     event.Type,
		Labels:        event.Labels,
		LabelAdded:    "",
		LabelRemoved:  "",
		LatestComment: event.Detail,
	}
	switch event.Type {
	case "label_added":
		ctx.LabelAdded = event.Detail
	case "label_removed":
		ctx.LabelRemoved = event.Detail
	}

	// Evaluate transitions.
	for _, tr := range currentState.Transitions {
		if !EvaluateCondition(tr.When, ctx) {
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
				return fmt.Errorf("statemachine: increment transition: %w", err)
			}

			maxRetries := wf.MaxRetries
			if maxRetries <= 0 {
				maxRetries = 3 // sensible default
			}

			if count >= maxRetries {
				// Reject back-edge, transition to failed.
				sm.eventlog.Log("cycle_limit_reached", event.Repo, event.IssueNum,
					fmt.Sprintf(`{"from":"%s","to":"%s","count":%d,"max_retries":%d}`,
						currentStateName, targetStateName, count, maxRetries))

				// Mark as failed — dispatch request not sent.
				// The "failed" state and "needs-human" label would be applied
				// by the system. Since Go code doesn't write labels (agents do),
				// we record the event. In v0.1.0, we still record the intent.
				sm.eventlog.Log("transition_to_failed", event.Repo, event.IssueNum,
					fmt.Sprintf(`{"from":"%s","rejected_to":"%s","needs_human":true}`,
						currentStateName, targetStateName))
				return nil
			}
		} else {
			// Not a back-edge, but still record the transition for history.
			_, err := sm.store.IncrementTransition(event.Repo, event.IssueNum, currentStateName, targetStateName)
			if err != nil {
				return fmt.Errorf("statemachine: increment transition: %w", err)
			}
		}

		// Log the transition.
		sm.eventlog.Log("transition", event.Repo, event.IssueNum,
			fmt.Sprintf(`{"from":"%s","to":"%s"}`, currentStateName, targetStateName))

		// If the target state has an agent, dispatch it.
		if targetState.Agent != "" {
			if err := sm.dispatchAgent(event.Repo, event.IssueNum, targetState.Agent, wf.Name, targetStateName); err != nil {
				return err
			}
		}

		// Only process the first matching transition.
		return nil
	}

	return nil
}

// dispatchAgent sends a dispatch request for the given agent, respecting the inflight mutex.
func (sm *StateMachine) dispatchAgent(repo string, issueNum int, agentName, workflow, state string) error {
	issueKey := fmt.Sprintf("%s#%d", repo, issueNum)

	// Execution mutex: don't dispatch if agent is already running.
	sm.inflightMu.Lock()
	if sm.inflight[issueKey] {
		sm.inflightMu.Unlock()
		sm.eventlog.Log("dispatch_skipped_inflight", repo, issueNum,
			fmt.Sprintf(`{"agent":"%s","reason":"agent already running"}`, agentName))
		return nil
	}
	sm.inflight[issueKey] = true
	sm.inflightMu.Unlock()

	sm.dispatch <- DispatchRequest{
		Repo:      repo,
		IssueNum:  issueNum,
		AgentName: agentName,
		Workflow:  workflow,
		State:     state,
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
// It clears the inflight flag and records the completion time for stuck detection.
func (sm *StateMachine) MarkAgentCompleted(repo string, issueNum int, currentLabels []string) {
	issueKey := fmt.Sprintf("%s#%d", repo, issueNum)

	sm.inflightMu.Lock()
	delete(sm.inflight, issueKey)
	sm.inflightMu.Unlock()

	labelsJSON, _ := json.Marshal(currentLabels)
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
	issueKey := fmt.Sprintf("%s#%d", repo, issueNum)

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
		sm.eventlog.Log("stuck_detected", repo, issueNum,
			fmt.Sprintf(`{"since":"%s","labels":%s}`, rec.at.Format(time.RFC3339), rec.labels))

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

// IsInflight returns true if an agent is currently running for the given issue.
func (sm *StateMachine) IsInflight(repo string, issueNum int) bool {
	issueKey := fmt.Sprintf("%s#%d", repo, issueNum)
	sm.inflightMu.Lock()
	defer sm.inflightMu.Unlock()
	return sm.inflight[issueKey]
}
