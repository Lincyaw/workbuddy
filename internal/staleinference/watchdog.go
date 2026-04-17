// Package staleinference implements a watchdog that detects hung agent
// processes (no JSONL output, no child processes) and cancels them via
// context cancellation.
package staleinference

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Config holds the watchdog parameters.
type Config struct {
	// IdleThreshold is the maximum allowed time since last session output.
	IdleThreshold time.Duration
	// CheckInterval is how often the watchdog checks for staleness.
	CheckInterval time.Duration
	// CompletedGracePeriod is the shorter grace period applied after an
	// agent has already reported completion but has not exited yet.
	CompletedGracePeriod time.Duration
}

// ActivityChecker provides an interface for checking process activity,
// making the watchdog testable without real processes.
type ActivityChecker interface {
	// SessionFileModTime returns the last modification time of the session
	// output file. Returns zero time and an error if the file does not exist.
	SessionFileModTime() (time.Time, error)

	// HasChildProcesses returns true if the agent process has live child
	// processes.
	HasChildProcesses() (bool, error)
}

// CompletionAwareActivityChecker exposes whether completion has been observed.
type CompletionAwareActivityChecker interface {
	CompletionObservedAt() (time.Time, bool)
}

// Watch starts the stale inference watchdog. It periodically checks
// whether the agent process is producing output or has child processes.
// If the process is stale for longer than cfg.IdleThreshold, the
// provided cancel function is called to kill the agent.
//
// Watch blocks until ctx is done. It should be called in a goroutine.
func Watch(ctx context.Context, cfg Config, checker ActivityChecker, cancel context.CancelFunc) {
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = 30 * time.Second
	}
	if cfg.IdleThreshold <= 0 {
		cfg.IdleThreshold = 10 * time.Minute
	}
	if cfg.CompletedGracePeriod <= 0 {
		cfg.CompletedGracePeriod = time.Minute
	}

	ticker := time.NewTicker(cfg.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if isStale(cfg, checker) {
				log.Printf("[stale-inference] agent process stale for >%s with no output and no children, killing", cfg.IdleThreshold)
				cancel()
				return
			}
		}
	}
}

// isStale returns true when the agent has produced no session file output
// and has no child processes for longer than the idle threshold.
func isStale(cfg Config, checker ActivityChecker) bool {
	mtime, err := checker.SessionFileModTime()
	if err != nil {
		// Session file doesn't exist yet — check how long we've been
		// waiting. We can't determine staleness without a baseline,
		// so skip this check.
		return false
	}

	threshold := cfg.IdleThreshold
	if completionAware, ok := checker.(CompletionAwareActivityChecker); ok {
		if completedAt, completed := completionAware.CompletionObservedAt(); completed && !completedAt.IsZero() {
			threshold = cfg.CompletedGracePeriod
			if mtime.Before(completedAt) {
				mtime = completedAt
			}
		}
	}

	idleDuration := time.Since(mtime)
	if idleDuration < threshold {
		return false
	}

	// File hasn't been written to for a while. Check if there are child
	// processes that might indicate the agent is still doing work.
	hasChildren, err := checker.HasChildProcesses()
	if err != nil {
		// Can't determine child process state, assume not stale.
		return false
	}

	return !hasChildren
}

// ProcChecker implements ActivityChecker using the /proc filesystem
// and os.Stat for the session file.
type ProcChecker struct {
	SessionPath string
	PID         int
}

// SessionFileModTime returns the modification time of the session file.
func (p *ProcChecker) SessionFileModTime() (time.Time, error) {
	info, err := os.Stat(p.SessionPath)
	if err != nil {
		return time.Time{}, fmt.Errorf("stat session file: %w", err)
	}
	return info.ModTime(), nil
}

// HasChildProcesses checks /proc/<pid>/task/../children or falls back
// to scanning /proc for processes whose PPid matches.
func (p *ProcChecker) HasChildProcesses() (bool, error) {
	// Try reading /proc/<pid>/task/<pid>/children (Linux 3.5+).
	childrenPath := fmt.Sprintf("/proc/%d/task/%d/children", p.PID, p.PID)
	data, err := os.ReadFile(childrenPath)
	if err == nil {
		return strings.TrimSpace(string(data)) != "", nil
	}

	// Fallback: scan /proc for any process with PPid == p.PID.
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, fmt.Errorf("read /proc: %w", err)
	}
	ppidPrefix := fmt.Sprintf("PPid:\t%d", p.PID)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		statusPath := fmt.Sprintf("/proc/%s/status", entry.Name())
		statusData, err := os.ReadFile(statusPath)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(statusData), "\n") {
			if line == ppidPrefix {
				return true, nil
			}
		}
	}
	return false, nil
}

// EventTracker tracks the last time an event was received on the events
// channel and implements ActivityChecker using this timestamp.
type EventTracker struct {
	lastActivity atomic.Int64 // unix nano
	completedAt  atomic.Int64 // unix nano
}

// NewEventTracker creates a new EventTracker with initial activity set to now.
func NewEventTracker() *EventTracker {
	t := &EventTracker{}
	t.RecordActivity()
	return t
}

// RecordActivity records that activity was observed right now.
func (t *EventTracker) RecordActivity() {
	t.lastActivity.Store(time.Now().UnixNano())
}

// RecordCompletion marks the tracker as having seen a terminal completion event.
func (t *EventTracker) RecordCompletion() {
	now := time.Now().UnixNano()
	t.completedAt.Store(now)
	t.lastActivity.Store(now)
}

// SessionFileModTime returns the time of last observed activity.
func (t *EventTracker) SessionFileModTime() (time.Time, error) {
	ns := t.lastActivity.Load()
	if ns == 0 {
		return time.Time{}, fmt.Errorf("no activity recorded")
	}
	return time.Unix(0, ns), nil
}

// HasChildProcesses always returns false for EventTracker since we
// don't have access to the PID. The staleness decision is based purely
// on event flow.
func (t *EventTracker) HasChildProcesses() (bool, error) {
	return false, nil
}

// CompletionObservedAt reports when task completion was observed.
func (t *EventTracker) CompletionObservedAt() (time.Time, bool) {
	ns := t.completedAt.Load()
	if ns == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, ns), true
}
