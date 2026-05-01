// Package eventlog provides structured event logging backed by SQLite.
package eventlog

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// Supported event types.
const (
	TypePoll                        = "poll"
	TypeTransition                  = "transition"
	TypeDispatch                    = "dispatch"
	TypeTokenUsage                  = "token_usage"
	TypeCompleted                   = "completed"
	TypeError                       = "error"
	TypeReport                      = "report"
	TypeRetryLimit                  = "retry_limit"
	TypeWorkerRegistered            = "worker_registered"
	TypeWorkerOffline               = "worker_offline"
	TypeStateEntry                  = "state_entry"
	TypeErrorMultiWorkflow          = "error_multi_workflow"
	TypeCycleLimitReached           = "cycle_limit_reached"
	TypeTransitionToFailed          = "transition_to_failed"
	TypeStuckDetected               = "stuck_detected"
	TypeDispatchSkippedInflight     = "dispatch_skipped_inflight"
	TypeRateLimit                   = "rate_limit"
	TypeDependencyVerdictChanged    = "dependency_verdict_changed"
	TypeDependencyCycleDetected     = "dependency_cycle_detected"
	TypeDependencyOverrideActivated = "dependency_override_activated"
	TypeDispatchBlockedByDependency = "dispatch_blocked_by_dependency"
	TypeNotificationFailed          = "notification_failed"
	TypeOperatorInvoked             = "operator_invoked"
	TypeAlert                       = "alert"
	TypeConfigReloaded              = "config_reloaded"
	TypeConfigReloadFailed          = "config_reload_failed"
	TypeReportOverflow              = "report_overflow"
	TypeDispatchBlockedByFailureCap = "dispatch_blocked_by_failure_cap"
	TypeDispatchBlockedByDone       = "dispatch_blocked_by_done"
	TypeDispatchSkippedClaim        = "dispatch_skipped_claim"
	TypeIssueClaimExpired           = "issue_claim_expired"
	TypeIssueRestarted              = "issue_restarted"
	TypeInfraFailure                = "infra_failure"
	TypeDevReviewCycleCount         = "dev_review_cycle_count"
	TypeDevReviewCycleApproaching   = "dev_review_cycle_approaching"
	TypeDevReviewCycleCapReached    = "dev_review_cycle_cap_reached"
	TypeDevReviewCycleCountReset    = "dev_review_cycle_count_reset"
	TypeLongFlightStuck             = "long_flight_stuck_detected"
	TypeRolloutDispatched           = "rollout_dispatched"
	TypeRolloutCompleted            = "rollout_completed"
	TypeSynthesisDecision           = "synthesis_decision"
	// TypeIssueNoWorkflowMatch fires when poller observes an issue carrying a
	// status:* label but none of the configured workflow trigger labels match,
	// i.e. the issue cannot enter the state machine. Idempotent per
	// (labels) fingerprint via the issue_pipeline_hazards table. See REQ #255.
	TypeIssueNoWorkflowMatch = "issue_no_workflow_match"
	// TypeIssueDependencyUnentered fires when dependency declarations are
	// present but cannot fully enter the normal dependency flow: either the
	// issue lacks a status:* label, or at least one depends_on ref is
	// malformed and dropped. Idempotent per hazard fingerprint. See REQ #255
	// and #303.
	TypeIssueDependencyUnentered = "issue_dependency_unentered"
	// TypeCoordinatorStarted is emitted at the tail of coordinator/serve startup
	// (after the HTTP listener is reserved). Payload carries listen address,
	// pid, and version so operator hook actions can detect process restarts.
	// See issue #266.
	TypeCoordinatorStarted = "coordinator_started"
	// TypeCoordinatorStopping is emitted at the head of the graceful-shutdown
	// handler so hooks can react before workers/pollers drain. See issue #266.
	TypeCoordinatorStopping = "coordinator_stopping"
	// TypeHooksReloaded is emitted by `workbuddy hooks reload` so operator
	// dashboards can correlate auto-disable resets with the reload action.
	// See issue #266.
	TypeHooksReloaded = "hooks_reloaded"
)

// AllEventTypes lists every recognised event type.
var AllEventTypes = []string{
	TypePoll,
	TypeTransition,
	TypeDispatch,
	TypeTokenUsage,
	TypeCompleted,
	TypeError,
	TypeReport,
	TypeRetryLimit,
	TypeWorkerRegistered,
	TypeWorkerOffline,
	TypeStateEntry,
	TypeErrorMultiWorkflow,
	TypeCycleLimitReached,
	TypeTransitionToFailed,
	TypeStuckDetected,
	TypeDispatchSkippedInflight,
	TypeRateLimit,
	TypeDependencyVerdictChanged,
	TypeDependencyCycleDetected,
	TypeDependencyOverrideActivated,
	TypeDispatchBlockedByDependency,
	TypeNotificationFailed,
	TypeOperatorInvoked,
	TypeAlert,
	TypeConfigReloaded,
	TypeConfigReloadFailed,
	TypeReportOverflow,
	TypeDispatchBlockedByFailureCap,
	TypeDispatchBlockedByDone,
	TypeDispatchSkippedClaim,
	TypeIssueClaimExpired,
	TypeIssueRestarted,
	TypeInfraFailure,
	TypeDevReviewCycleCount,
	TypeDevReviewCycleApproaching,
	TypeDevReviewCycleCapReached,
	TypeDevReviewCycleCountReset,
	TypeLongFlightStuck,
	TypeRolloutDispatched,
	TypeRolloutCompleted,
	TypeSynthesisDecision,
	TypeIssueNoWorkflowMatch,
	TypeIssueDependencyUnentered,
	TypeCoordinatorStarted,
	TypeCoordinatorStopping,
	TypeHooksReloaded,
}

// EventFilter specifies optional criteria for querying events.
type EventFilter struct {
	Since    *time.Time
	Until    *time.Time
	Type     string
	Repo     string
	IssueNum int
}

// Health reports the observability state of an EventLogger.
//
// Event-log writes are best-effort telemetry: a single failed INSERT must not
// block the orchestration pipeline. But silently swallowing failures hides
// degraded audit trails from operators. Health exposes the degraded condition
// so it can be surfaced via /metrics, `workbuddy status`, or log scraping
// without changing the best-effort Log() contract.
type Health struct {
	// Degraded is true once any write has failed since process start.
	Degraded bool `json:"degraded"`
	// WriteFailures counts the number of failed InsertEvent calls.
	WriteFailures int64 `json:"write_failures"`
	// LastError is the string form of the most recent write error, if any.
	LastError string `json:"last_error,omitempty"`
	// LastFailureAt is the UTC timestamp of the most recent write error.
	LastFailureAt time.Time `json:"last_failure_at,omitempty"`
}

// Publisher is the optional sink the EventLogger forwards every successfully
// (or attempted) inserted event to. The hook dispatcher is the canonical
// implementation. Defining the interface here keeps internal/eventlog free of
// an internal/hooks import (the dependency direction is hooks → eventlog).
type Publisher interface {
	PublishFromRaw(eventType, repo string, issueNum int, payloadJSON string)
}

// EventLogger provides a higher-level API over store.Store for event logging.
// It marshals payloads to JSON and swallows write errors (printing to stderr),
// while tracking failures on a per-instance health counter so operators can
// detect degraded observability via Health().
type EventLogger struct {
	store     *store.Store
	errWriter io.Writer

	writeFailures atomic.Int64

	healthMu      sync.RWMutex
	lastErr       string
	lastFailureAt time.Time

	pubMu     sync.RWMutex
	publisher Publisher
}

// NewEventLogger creates an EventLogger backed by the given Store.
func NewEventLogger(s *store.Store) *EventLogger {
	return NewEventLoggerWithWriter(s, os.Stderr)
}

// NewEventLoggerWithWriter creates an EventLogger backed by the given Store
// and writes best-effort error diagnostics to errWriter.
func NewEventLoggerWithWriter(s *store.Store, errWriter io.Writer) *EventLogger {
	if errWriter == nil {
		errWriter = io.Discard
	}
	return &EventLogger{store: s, errWriter: errWriter}
}

// Log records an event. The payload is marshalled to JSON automatically.
// On failure the error is printed to stderr and the logger's health is
// marked degraded; the caller is never blocked.
func (l *EventLogger) Log(eventType, repo string, issueNum int, payload interface{}) {
	var payloadStr string
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			fmt.Fprintf(l.errWriter, "eventlog: marshal payload: %v\n", err)
			payloadStr = fmt.Sprintf(`{"marshal_error":%q}`, err.Error())
		} else {
			payloadStr = string(data)
		}
	}

	_, err := l.store.InsertEvent(store.Event{
		Type:     eventType,
		Repo:     repo,
		IssueNum: issueNum,
		Payload:  payloadStr,
	})
	if err != nil {
		fmt.Fprintf(l.errWriter, "eventlog: write failed: %v\n", err)
		l.recordFailure(err)
	}

	// Publish to the hook dispatcher after the SQLite insert. The dispatcher
	// is responsible for filtering its own self-events (hook_*) and for
	// non-blocking semantics — Log() never blocks even under hook back-pressure.
	l.pubMu.RLock()
	pub := l.publisher
	l.pubMu.RUnlock()
	if pub != nil {
		pub.PublishFromRaw(eventType, repo, issueNum, payloadStr)
	}
}

// SetPublisher attaches (or detaches with nil) a hook dispatcher. Safe to
// call from any goroutine and at any time during the process lifetime.
func (l *EventLogger) SetPublisher(p Publisher) {
	l.pubMu.Lock()
	l.publisher = p
	l.pubMu.Unlock()
}

func (l *EventLogger) recordFailure(err error) {
	l.writeFailures.Add(1)
	l.healthMu.Lock()
	l.lastErr = err.Error()
	l.lastFailureAt = time.Now().UTC()
	l.healthMu.Unlock()
}

// Health returns a snapshot of the logger's observability state. It is safe
// to call from any goroutine. Callers that treat event-log writes as
// best-effort can ignore this; operator surfaces (metrics, status, healthz)
// should surface Degraded as a first-class condition so a lost audit trail
// is visible instead of silent.
func (l *EventLogger) Health() Health {
	failures := l.writeFailures.Load()
	l.healthMu.RLock()
	lastErr := l.lastErr
	lastAt := l.lastFailureAt
	l.healthMu.RUnlock()
	return Health{
		Degraded:      failures > 0,
		WriteFailures: failures,
		LastError:     lastErr,
		LastFailureAt: lastAt,
	}
}

// Query returns events matching the filter criteria.
// All filter fields are optional; zero-value fields are ignored.
func (l *EventLogger) Query(filter EventFilter) ([]store.Event, error) {
	return l.store.QueryEventsFiltered(store.EventQueryFilter{
		Since:    filter.Since,
		Until:    filter.Until,
		Type:     filter.Type,
		Repo:     filter.Repo,
		IssueNum: filter.IssueNum,
	})
}
