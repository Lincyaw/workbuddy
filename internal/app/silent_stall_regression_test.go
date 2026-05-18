//go:build faultinject

package app

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/failpoints"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// TestSilentStallRecoversAfterAgentDieMid is the #345 regression test.
//
// It reproduces the silent-stall pattern that motivated Wave 2:
//
//  1. An issue is dispatched to a worker and the worker's task row moves to
//     status=running.
//  2. The worker's agent subprocess dies mid-flight (here simulated by arming
//     the `worker.agent_exec.die_mid` failpoint and dropping the task's
//     heartbeat to an hour ago).
//  3. Pre-Wave-2, the row sat in status=running forever; HasAnyActiveTask
//     kept returning true; no new task was dispatched.
//
// Wave 2 closes this with three components:
//
//   - W2-B (TaskReaper, REQ-151): periodic goroutine flips the stale running
//     row to failed and emits TypeTaskReaped.
//   - W2-C (recoverAllPeriodically, REQ-152): periodic sweep re-dispatches
//     the orphaned active state and emits TypePeriodicRecoveryTick with
//     issues_redispatched >= 1.
//   - W2-A (EventIssueResynced, REQ-150): is a defence-in-depth at the
//     poller layer and is exercised in poller_test.go; this test pins the
//     reaper + recovery path because they are the ones that flip the
//     persistent state.
//
// Build tag: only runs under `-tags faultinject` so the failpoint API is
// real (Arm / Hit actually do something rather than being inlined no-ops).
//
// Run with:
//
//	go test -tags faultinject -run TestSilentStallRecoversAfterAgentDieMid ./internal/app/...
//
// See issue #345.
func TestSilentStallRecoversAfterAgentDieMid(t *testing.T) {
	if !failpoints.Enabled() {
		t.Skip("requires -tags faultinject")
	}
	runSilentStallScenario(t, scenarioOpts{
		enableReaper:   true,
		enableRecovery: true,
	}, expectGreen)
}

// TestSilentStallRedWithoutReaper is the RED arm of the regression: with the
// TaskReaper goroutine disabled, the stale running row never transitions to
// failed and the orphaned-state recovery never gets a chance to re-dispatch.
// This proves the regression test is sensitive to the W2-B component.
//
// We assert the failure mode directly rather than letting t.Fatalf fire from
// inside runSilentStallScenario, so the test stays green while still
// demonstrating that the GREEN test's pass condition is non-trivial.
func TestSilentStallRedWithoutReaper(t *testing.T) {
	if !failpoints.Enabled() {
		t.Skip("requires -tags faultinject")
	}
	runSilentStallScenario(t, scenarioOpts{
		enableReaper:   false,
		enableRecovery: true,
	}, expectRedStaleRowNeverFails)
}

// TestSilentStallRedWithoutRecovery is the symmetric RED arm for W2-C: the
// TaskReaper runs (so the stale running row DOES flip to failed) but the
// periodic recovery sweep is disabled, so no re-dispatch fires. The orphan
// active state therefore stays orphaned even though its bookkeeping is now
// consistent. This proves the GREEN test is sensitive to the recovery
// component independently from the reaper.
//
// As with TestSilentStallRedWithoutReaper, we assert the failure mode
// directly so this test stays green while pinning that the cross-cutting
// recovery loop is load-bearing.
func TestSilentStallRedWithoutRecovery(t *testing.T) {
	if !failpoints.Enabled() {
		t.Skip("requires -tags faultinject")
	}
	runSilentStallScenario(t, scenarioOpts{
		enableReaper:   true,
		enableRecovery: false,
	}, expectRedNoReDispatch)
}

type scenarioExpectation int

const (
	expectGreen scenarioExpectation = iota
	expectRedStaleRowNeverFails
	expectRedNoReDispatch
)

type scenarioOpts struct {
	enableReaper   bool
	enableRecovery bool
}

func runSilentStallScenario(t *testing.T, opts scenarioOpts, want scenarioExpectation) {
	t.Helper()

	const (
		repo     = "owner/silent-stall"
		issueNum = 345
		taskID   = "task-pre-die"
		workerID = "worker-dead"
	)

	// Always reset the registry on entry and exit so tests do not leak
	// armed failpoints to each other.
	failpoints.Reset()
	t.Cleanup(failpoints.Reset)

	// Arm the agent_exec.die_mid failpoint. We never actually exec a
	// subprocess in this test — the arm is the contractual proof that the
	// hook point exists and is observable from the regression scenario; the
	// real failure mode is reproduced below by hand-aging the heartbeat.
	failpoints.Arm("worker.agent_exec.die_mid", failpoints.Effect{
		Kind: "error",
		Err:  "simulated SIGKILL mid-flight",
	})

	f := newPeriodicRecoveryFixture(t, repo)

	// Seed an orphaned active state: the issue exists in the cache labelled
	// for the reviewing state with no live task, plus a separate stale
	// running task_queue row that the reaper must drain.
	f.seedOrphanedReviewingIssue(t, repo, issueNum)
	if err := f.store.InsertTask(store.TaskRecord{
		ID:        taskID,
		Repo:      repo,
		IssueNum:  issueNum,
		AgentName: "review-agent",
		Status:    store.TaskStatusPending,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	if _, err := f.store.Exec(
		`UPDATE task_queue SET status = ?, worker_id = ?, claim_token = 'tok',
			heartbeat_at = datetime('now', '-3600 seconds') WHERE id = ?`,
		store.TaskStatusRunning, workerID, taskID,
	); err != nil {
		t.Fatalf("seed stale running task: %v", err)
	}

	// HasAnyActiveTask must currently return true so the recovery sweep
	// would skip the orphan if we ran it before the reaper finishes. This
	// pre-condition is what made the pre-Wave-2 bug silent.
	active, err := f.store.HasAnyActiveTask(repo, issueNum)
	if err != nil {
		t.Fatalf("HasAnyActiveTask pre-reap: %v", err)
	}
	if !active {
		t.Fatalf("expected stale running row to count as active task pre-reap")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup

	// Optionally spin up the reaper. The RED arm omits it to demonstrate
	// the regression: without W2-B the orphaned row sits forever.
	if opts.enableReaper {
		reaper := NewTaskReaper(f.store, f.evlog, 50*time.Millisecond, 1*time.Second)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = reaper.Run(ctx)
		}()
	}

	// Optionally spin up the periodic recovery loop.
	if opts.enableRecovery {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.pm.recoverAllPeriodically(ctx, 50*time.Millisecond)
		}()
	}

	switch want {
	case expectGreen:
		assertStaleRowFlippedToFailed(t, f.store, taskID, 2*time.Second)
		assertTaskReapedEventEmitted(t, f.evlog, taskID, workerID, repo, issueNum)
		assertOrphanReDispatched(t, f.dispatchCh, repo, issueNum, 2*time.Second)
		assertPeriodicRecoveryTickEmitted(t, f.evlog, 2*time.Second)
	case expectRedStaleRowNeverFails:
		// Wait a window comfortably longer than the recovery interval to
		// make sure neither the reaper nor a stray sweep flipped the row.
		// 400ms = 8 recovery ticks; if the reaper were running, the row
		// would have flipped on tick #1 (~50ms).
		time.Sleep(400 * time.Millisecond)
		got, err := f.store.GetTask(taskID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.Status != store.TaskStatusRunning {
			t.Fatalf("RED arm: expected stale row to stay running, got %q (W2-B disabled but row transitioned anyway)", got.Status)
		}
		// And no DispatchRequest because HasAnyActiveTask still returns true.
		select {
		case req := <-f.dispatchCh:
			t.Fatalf("RED arm: unexpected dispatch %+v — recovery should have been blocked by the stale running row", req)
		default:
		}
	case expectRedNoReDispatch:
		// The reaper is enabled so the stale row should flip to failed
		// (W2-B is load-bearing for clearing the orphan-block). But with
		// recovery disabled, no re-dispatch fires for the orphaned
		// active state.
		assertStaleRowFlippedToFailed(t, f.store, taskID, 2*time.Second)
		// Wait a window comfortably longer than the recovery interval.
		// 400ms = 8 ticks at the configured 50ms cadence; if recovery
		// were running, the dispatch would have already arrived.
		select {
		case req := <-f.dispatchCh:
			t.Fatalf("RED arm: unexpected dispatch %+v — recovery should be disabled", req)
		case <-time.After(400 * time.Millisecond):
		}
	default:
		t.Fatalf("unknown expectation %v", want)
	}

	cancel()
	// Drain dispatchCh so the scenario shutdown is clean; we do not block
	// here because the channel may have additional resync/recovery emissions
	// that arrived after our first dispatch was observed.
	go func() {
		for range f.dispatchCh {
		}
	}()
	wg.Wait()
}

func assertStaleRowFlippedToFailed(t *testing.T, st store.Store, taskID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got, err := st.GetTask(taskID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.Status == store.TaskStatusFailed {
			if got.ExitCode != -2 {
				t.Fatalf("stale row flipped to failed but exit_code=%d, want -2 (REAPED sentinel)", got.ExitCode)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("reaper did not flip stale row to failed within %s; status=%q", timeout, got.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func assertTaskReapedEventEmitted(t *testing.T, evlog *eventlog.EventLogger, wantTaskID, wantWorkerID, wantRepo string, wantIssueNum int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		events, err := evlog.Query(eventlog.EventFilter{Type: eventlog.TypeTaskReaped})
		if err != nil {
			t.Fatalf("Query TypeTaskReaped: %v", err)
		}
		for _, ev := range events {
			if ev.Repo != wantRepo || ev.IssueNum != wantIssueNum {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(ev.Payload), &payload); err != nil {
				t.Fatalf("payload not JSON: %v (%q)", err, ev.Payload)
			}
			if payload["task_id"] != wantTaskID {
				continue
			}
			if payload["worker_id"] != wantWorkerID {
				t.Fatalf("worker_id mismatch: %+v", payload)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no matching %s event for task=%s within deadline", eventlog.TypeTaskReaped, wantTaskID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func assertOrphanReDispatched(t *testing.T, dispatchCh <-chan statemachine.DispatchRequest, wantRepo string, wantIssueNum int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case req := <-dispatchCh:
			if req.Repo == wantRepo && req.IssueNum == wantIssueNum {
				return
			}
		case <-deadline:
			t.Fatalf("orphaned active state was NOT re-dispatched for %s#%d within %s", wantRepo, wantIssueNum, timeout)
		}
	}
}

func assertPeriodicRecoveryTickEmitted(t *testing.T, evlog *eventlog.EventLogger, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		events, err := evlog.Query(eventlog.EventFilter{Type: eventlog.TypePeriodicRecoveryTick})
		if err != nil {
			t.Fatalf("Query TypePeriodicRecoveryTick: %v", err)
		}
		for _, ev := range events {
			var payload map[string]any
			if err := json.Unmarshal([]byte(ev.Payload), &payload); err != nil {
				continue
			}
			if got, ok := payload["issues_redispatched"].(float64); ok && int(got) >= 1 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s event with issues_redispatched >= 1 within deadline", eventlog.TypePeriodicRecoveryTick)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

