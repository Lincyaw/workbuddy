// Package eventlog provides structured event logging backed by SQLite.
package eventlog

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

// Supported event types.
const (
	TypePoll                   = "poll"
	TypeTransition             = "transition"
	TypeDispatch               = "dispatch"
	TypeCompleted              = "completed"
	TypeError                  = "error"
	TypeReport                 = "report"
	TypeRetryLimit             = "retry_limit"
	TypeWorkerRegistered       = "worker_registered"
	TypeWorkerOffline          = "worker_offline"
	TypeStateEntry             = "state_entry"
	TypeErrorMultiWorkflow     = "error_multi_workflow"
	TypeCycleLimitReached      = "cycle_limit_reached"
	TypeTransitionToFailed     = "transition_to_failed"
	TypeStuckDetected          = "stuck_detected"
	TypeDispatchSkippedInflight = "dispatch_skipped_inflight"
	TypeDependencyVerdictChanged = "dependency_verdict_changed"
	TypeDependencyQueueQueued    = "dependency_queue_queued"
	TypeDependencyCycleDetected  = "dependency_cycle_detected"
	TypeDependencyOverrideActivated = "dependency_override_activated"
)

// AllEventTypes lists every recognised event type.
var AllEventTypes = []string{
	TypePoll,
	TypeTransition,
	TypeDispatch,
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
	TypeDependencyVerdictChanged,
	TypeDependencyQueueQueued,
	TypeDependencyCycleDetected,
	TypeDependencyOverrideActivated,
}

// EventFilter specifies optional criteria for querying events.
type EventFilter struct {
	Since    *time.Time
	Until    *time.Time
	Type     string
	Repo     string
	IssueNum int
}

// EventLogger provides a higher-level API over store.Store for event logging.
// It marshals payloads to JSON and swallows write errors (printing to stderr).
type EventLogger struct {
	store *store.Store
}

// NewEventLogger creates an EventLogger backed by the given Store.
func NewEventLogger(s *store.Store) *EventLogger {
	return &EventLogger{store: s}
}

// Log records an event. The payload is marshalled to JSON automatically.
// On failure the error is printed to stderr; the caller is never blocked.
func (l *EventLogger) Log(eventType, repo string, issueNum int, payload interface{}) {
	var payloadStr string
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "eventlog: marshal payload: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "eventlog: write failed: %v\n", err)
	}
}

// Query returns events matching the filter criteria.
// All filter fields are optional; zero-value fields are ignored.
func (l *EventLogger) Query(filter EventFilter) ([]store.Event, error) {
	db := l.store.DB()

	query := `SELECT id, ts, type, repo, issue_num, payload FROM events WHERE 1=1`
	var args []interface{}

	if filter.Repo != "" {
		query += ` AND repo = ?`
		args = append(args, filter.Repo)
	}
	if filter.Type != "" {
		query += ` AND type = ?`
		args = append(args, filter.Type)
	}
	if filter.IssueNum != 0 {
		query += ` AND issue_num = ?`
		args = append(args, filter.IssueNum)
	}
	if filter.Since != nil {
		query += ` AND ts >= ?`
		args = append(args, filter.Since.UTC().Format("2006-01-02 15:04:05"))
	}
	if filter.Until != nil {
		query += ` AND ts <= ?`
		args = append(args, filter.Until.UTC().Format("2006-01-02 15:04:05"))
	}

	query += ` ORDER BY id`

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("eventlog: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []store.Event
	for rows.Next() {
		var ev store.Event
		var ts string
		if err := rows.Scan(&ev.ID, &ts, &ev.Type, &ev.Repo, &ev.IssueNum, &ev.Payload); err != nil {
			return nil, fmt.Errorf("eventlog: scan: %w", err)
		}
		ev.TS, _ = time.Parse("2006-01-02 15:04:05", ts)
		out = append(out, ev)
	}
	return out, rows.Err()
}
