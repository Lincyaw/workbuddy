package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// TestTaskReaperEmitsEvent pins the audit-trail contract for the periodic
// reaper (REQ-151, issue #345 Wave 2). It seeds a stale running task, runs
// the reaper for a short window, and asserts a `task_reaped` event landed
// with the original task's task_id / worker_id / repo / issue_num so an
// operator post-mortem can correlate the silent-stall cause with the row.
func TestTaskReaperEmitsEvent(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "reaper.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InsertTask(store.TaskRecord{
		ID:        "stale-1",
		Repo:      "org/r",
		IssueNum:  42,
		AgentName: "dev",
		Status:    store.TaskStatusPending,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	// Flip the row to running with a heartbeat that is well outside the
	// reaper's 1-second grace.
	if _, err := st.Exec(
		`UPDATE task_queue SET status = ?, worker_id = 'worker-dead', claim_token = 'tok-dead',
			heartbeat_at = datetime('now', '-3600 seconds') WHERE id = 'stale-1'`,
		store.TaskStatusRunning,
	); err != nil {
		t.Fatalf("seed stale task: %v", err)
	}

	evlog := eventlog.NewEventLogger(st)
	reaper := NewTaskReaper(st, evlog, 50*time.Millisecond, 1*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = reaper.Run(ctx)
	}()

	// Wait until the row flips to failed (proves the reaper fired) instead
	// of polling on a wall-clock timeout — keeps the test resilient on
	// slow CI runners without leaning on a global sleep.
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := st.GetTask("stale-1")
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.Status == store.TaskStatusFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reaper did not reap stale-1 within deadline; status=%q", got.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	<-done

	// Read back events and find the task_reaped one.
	events, err := evlog.Query(eventlog.EventFilter{Type: eventlog.TypeTaskReaped})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 task_reaped event, got %d", len(events))
	}
	ev := events[0]
	if ev.Repo != "org/r" || ev.IssueNum != 42 {
		t.Fatalf("event repo/issue mismatch: got (%q,%d)", ev.Repo, ev.IssueNum)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(ev.Payload), &payload); err != nil {
		t.Fatalf("payload not valid JSON: %v (%q)", err, ev.Payload)
	}
	if payload["task_id"] != "stale-1" {
		t.Fatalf("payload task_id mismatch: %+v", payload)
	}
	if payload["worker_id"] != "worker-dead" {
		t.Fatalf("payload worker_id mismatch: %+v", payload)
	}
	if payload["reason"] != "no_heartbeat" {
		t.Fatalf("payload reason mismatch: %+v", payload)
	}
	if payload["agent"] != "dev" {
		t.Fatalf("payload agent mismatch: %+v", payload)
	}
	if s, ok := payload["last_heartbeat_at"].(string); !ok || s == "" {
		t.Fatalf("payload last_heartbeat_at missing or empty: %+v", payload)
	}
}

// TestTaskReaperNilEventlogIsSafe pins the nil-logger contract: the
// coordinator wires evlog in, but the reaper itself must not panic when
// constructed without one — a guarantee callers (including future tests)
// can lean on.
func TestTaskReaperNilEventlogIsSafe(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "reaper-nil.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InsertTask(store.TaskRecord{
		ID: "stale-nil", Repo: "org/r", IssueNum: 1, AgentName: "dev", Status: store.TaskStatusPending,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	if _, err := st.Exec(
		`UPDATE task_queue SET status = ?, heartbeat_at = datetime('now', '-3600 seconds') WHERE id = 'stale-nil'`,
		store.TaskStatusRunning,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	reaper := NewTaskReaper(st, nil, 50*time.Millisecond, 1*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = reaper.Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := st.GetTask("stale-nil")
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.Status == store.TaskStatusFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nil-evlog reaper failed to reap stale-nil")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wg.Wait()
}
