package app

import (
	"context"
	"log"
	"time"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// Default knobs for the TaskReaper. The reaper is best-effort house-keeping
// for status=running rows whose worker stopped heart-beating; the values are
// deliberately conservative so a transient hiccup never racing with a real
// worker recovery. See REQ-151 (issue #345 Wave 2).
const (
	// DefaultTaskReaperInterval is how often the reaper checks for stale
	// running rows when no explicit override is supplied.
	DefaultTaskReaperInterval = 60 * time.Second
	// DefaultTaskReaperGrace is the grace period (no heartbeat) after which
	// a status=running row is considered stale. Chosen as 5× the default
	// worker heartbeat interval (15s) so a worker that misses one or two
	// heartbeats due to GC or transient load does NOT get its task reaped
	// out from underneath it.
	DefaultTaskReaperGrace = 5 * time.Minute
)

// taskReaperEventLogger is the narrow eventlog surface the reaper needs.
// Defining it locally keeps the reaper testable with a fake and avoids
// importing eventlog.EventLogger as a concrete type into other tests.
type taskReaperEventLogger interface {
	Log(eventType, repo string, issueNum int, payload interface{})
}

// TaskReaper periodically converts stale status=running task_queue rows to
// status=failed so HasAnyActiveTask stops returning true and the periodic
// recovery sweep (see W2-C) can re-dispatch the issue. This closes silent-
// stall root cause #2 in #345.
//
// The reaper deliberately runs as its own goroutine separate from the
// operator detector (which only observes, never mutates task state) and from
// the worker-side heartbeat logic. Its only state mutation is the SELECT +
// UPDATE inside store.ReapStaleRunningTasks.
type TaskReaper struct {
	store    store.Store
	eventlog taskReaperEventLogger // optional, nil-safe
	interval time.Duration
	grace    time.Duration
}

// NewTaskReaper builds a TaskReaper. Zero/negative interval or grace fall
// back to the defaults. The eventlog argument may be nil; the reaper will
// then still drain stale rows but emit no audit events.
func NewTaskReaper(st store.Store, evlog *eventlog.EventLogger, interval, grace time.Duration) *TaskReaper {
	if interval <= 0 {
		interval = DefaultTaskReaperInterval
	}
	if grace <= 0 {
		grace = DefaultTaskReaperGrace
	}
	// Convert the concrete *EventLogger to the interface so nil checks work
	// uniformly (an interface holding a typed-nil pointer would compare !=
	// nil). The reaper guards both the interface variable and the embedded
	// pointer being nil.
	var sink taskReaperEventLogger
	if evlog != nil {
		sink = evlog
	}
	return &TaskReaper{
		store:    st,
		eventlog: sink,
		interval: interval,
		grace:    grace,
	}
}

// Run blocks until ctx is cancelled, sweeping the task_queue once per
// interval. It runs an immediate pass on entry so a coordinator restart
// after a worker crash doesn't have to wait the first interval before
// clearing the stale row.
func (r *TaskReaper) Run(ctx context.Context) error {
	if r == nil || r.store == nil {
		return nil
	}
	r.runOnce(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.runOnce(ctx)
		}
	}
}

// runOnce performs a single reaper pass. Errors are logged but never
// propagated: the reaper is best-effort housekeeping, exactly like the
// operator detector and the recovery sweep.
func (r *TaskReaper) runOnce(_ context.Context) {
	reaped, err := r.store.ReapStaleRunningTasks(r.grace)
	if err != nil {
		log.Printf("[task-reaper] reap stale running tasks: %v", err)
		return
	}
	if len(reaped) == 0 {
		return
	}
	log.Printf("[task-reaper] reaped %d stale running task(s)", len(reaped))
	if r.eventlog == nil {
		return
	}
	for _, t := range reaped {
		payload := map[string]any{
			"task_id":           t.ID,
			"repo":              t.Repo,
			"issue_num":         t.IssueNum,
			"agent":             t.AgentName,
			"worker_id":         t.WorkerID,
			"last_heartbeat_at": formatReapedTime(t.HeartbeatAt),
			"reason":            "no_heartbeat",
		}
		r.eventlog.Log(eventlog.TypeTaskReaped, t.Repo, t.IssueNum, payload)
	}
}

// formatReapedTime renders a time.Time as RFC3339 UTC, or empty string when
// the value is zero. The reaper payload distinguishes "worker heart-beat
// once then died" (non-empty) from "worker never heart-beat at all" (empty)
// — both legitimate stall causes worth telling apart in postmortems.
func formatReapedTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
