package worker

// This file owns the issue #245 boot-time resume orchestration: a stateless
// worker rebuilds its supervisor-agent attachments. For every running task
// this worker still owns we ask the local supervisor whether the agent is
// alive; if it is, we re-subscribe to its SSE event stream from the offset
// matching what is already on disk so events-v1.jsonl continues without
// duplicates. If the supervisor doesn't know the agent (404) we mark the
// task failed — we cannot reconstruct an agent that no longer exists. If
// the supervisor reports the agent already exited we run the finalize
// callback so the task's result is recorded. The actual supervisor IPC and
// event-line writing are pluggable so the unit tests can drive the flow
// with httptest.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/supervisor"
	supclient "github.com/Lincyaw/workbuddy/internal/supervisor/client"
)

// SupervisorClient is the slice of supervisor/client.Client the resumer
// actually needs. Defined as an interface so tests can plug in a fake.
type SupervisorClient interface {
	Status(ctx context.Context, agentID string) (*supervisor.AgentStatus, error)
	StreamEvents(ctx context.Context, agentID string, fromOffset int64, onEvent func(supclient.StreamEvent) error) error
}

// ResumeOutcomeHandler reacts to terminal conditions for a resumed task.
// OnFailed runs when the supervisor returns 404 — there is no agent to
// resume and the task must be marked failed instead of being left
// in-flight. OnExited runs when the supervisor reports status=exited; the
// caller (typically the worker boot path) can then post a result with the
// recorded exit code and free the lease.
type ResumeOutcomeHandler interface {
	OnFailed(ctx context.Context, task store.InFlightTaskForWorker, reason string) error
	OnExited(ctx context.Context, task store.InFlightTaskForWorker, status *supervisor.AgentStatus) error
}

// ResumeInFlightTasks spawns one goroutine per task and returns once every
// goroutine completes its initial supervisor probe + (for live agents) its
// SSE re-subscription closes. The boot path calls it from a goroutine so the
// main poll loop is not blocked. The first non-nil error returned by an
// OnFailed/OnExited callback becomes the wait-group's error result.
func ResumeInFlightTasks(ctx context.Context, sup SupervisorClient, handler ResumeOutcomeHandler, tasks []store.InFlightTaskForWorker) {
	if len(tasks) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, task := range tasks {
		wg.Add(1)
		go func(t store.InFlightTaskForWorker) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[worker] resume: task %s panic: %v", t.TaskID, r)
				}
			}()
			if err := resumeOne(ctx, sup, handler, t); err != nil {
				log.Printf("[worker] resume: task %s: %v", t.TaskID, err)
			}
		}(task)
	}
	wg.Wait()
}

func resumeOne(ctx context.Context, sup SupervisorClient, handler ResumeOutcomeHandler, t store.InFlightTaskForWorker) error {
	if t.SupervisorAgentID == "" {
		// Should not happen — the store query already filters these out,
		// but defending against future store changes is cheap.
		return fmt.Errorf("missing supervisor_agent_id")
	}

	status, err := sup.Status(ctx, t.SupervisorAgentID)
	if err != nil {
		if errors.Is(err, supclient.ErrAgentNotFound) {
			reason := fmt.Sprintf("supervisor does not recognize agent %s", t.SupervisorAgentID)
			return handler.OnFailed(ctx, t, reason)
		}
		// Transport / other errors are NOT retried here per AC: bail out
		// and let the next worker boot try again. We do not mark failed
		// because the agent may still be alive.
		return fmt.Errorf("supervisor status: %w", err)
	}

	switch status.Status {
	case "exited":
		return handler.OnExited(ctx, t, status)
	case "running":
		return resumeRunning(ctx, sup, handler, t)
	default:
		return fmt.Errorf("unexpected supervisor status %q for agent %s", status.Status, t.SupervisorAgentID)
	}
}

func resumeRunning(ctx context.Context, sup SupervisorClient, handler ResumeOutcomeHandler, t store.InFlightTaskForWorker) error {
	fromOffset, err := countEventLines(t.EventsV1Path)
	if err != nil {
		return fmt.Errorf("count events-v1.jsonl lines: %w", err)
	}

	out, closeErr := openEventsAppend(t.EventsV1Path)
	if closeErr != nil {
		return fmt.Errorf("open events-v1.jsonl: %w", closeErr)
	}
	defer func() { _ = out.Close() }()

	streamErr := sup.StreamEvents(ctx, t.SupervisorAgentID, fromOffset, func(ev supclient.StreamEvent) error {
		// Defence-in-depth dedup: the supervisor honours from_offset, but
		// if a server bug ever re-sent earlier offsets we would silently
		// duplicate without this guard.
		if ev.Offset <= fromOffset {
			return nil
		}
		if _, werr := fmt.Fprintln(out, ev.Line); werr != nil {
			return fmt.Errorf("append events-v1.jsonl: %w", werr)
		}
		fromOffset = ev.Offset
		return nil
	})
	if streamErr != nil {
		if errors.Is(streamErr, supclient.ErrAgentNotFound) {
			// Supervisor lost the agent between Status and StreamEvents.
			reason := fmt.Sprintf("supervisor lost agent %s mid-resume", t.SupervisorAgentID)
			return handler.OnFailed(ctx, t, reason)
		}
		if errors.Is(streamErr, context.Canceled) {
			return nil
		}
		return fmt.Errorf("stream events: %w", streamErr)
	}

	// SSE closed cleanly — the supervisor either drained after exit or the
	// stream connection ended. Probe once more so the task can be finalized
	// when the agent has actually exited.
	final, ferr := sup.Status(ctx, t.SupervisorAgentID)
	if ferr != nil {
		if errors.Is(ferr, supclient.ErrAgentNotFound) {
			reason := fmt.Sprintf("supervisor lost agent %s after stream close", t.SupervisorAgentID)
			return handler.OnFailed(ctx, t, reason)
		}
		return fmt.Errorf("supervisor status after stream close: %w", ferr)
	}
	if final.Status == "exited" {
		return handler.OnExited(ctx, t, final)
	}
	// Still running but stream closed (e.g. proxy dropped connection):
	// nothing to do here — the next boot or heartbeat will catch it.
	return nil
}

func countEventLines(path string) (int64, error) {
	if path == "" {
		return 0, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var n int64
	for scanner.Scan() {
		n++
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return n, nil
}

func openEventsAppend(path string) (*os.File, error) {
	if path == "" {
		// Write to a discard sink rather than failing — keeps the resumer
		// useful for tasks whose session row was lost (e.g. during the
		// pre-supervisor migration). Caller can still observe outcomes.
		return os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}
