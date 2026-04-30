package hooks

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// captureAction implements both Action and CapturingAction so we can pin down
// what the dispatcher records into the per-hook ring buffer.
type captureAction struct {
	stdout []byte
	stderr []byte
	err    error
	calls  atomic.Int64
}

func (a *captureAction) Type() string { return "capture" }
func (a *captureAction) Execute(ctx context.Context, ev Event, payload []byte) error {
	return a.Capture(ctx, ev, payload).Err
}
func (a *captureAction) Capture(_ context.Context, _ Event, _ []byte) ActionCapture {
	a.calls.Add(1)
	return ActionCapture{Stdout: a.stdout, Stderr: a.stderr, Err: a.err}
}

func dispatcherWithCapture(t *testing.T, name string, action *captureAction) *Dispatcher {
	t.Helper()
	reg := NewActionRegistry()
	reg.Register("capture", func(_ *Hook) (Action, []string, error) { return action, nil, nil })
	cfg := &Config{SchemaVersion: 1, Hooks: []Hook{{Name: name, Events: []string{"alert"}, Action: ActionConfig{Type: "capture"}}}}
	d, _, err := NewDispatcher(cfg, reg)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return d
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within 2s")
}

func TestDispatcherInvocationRecordsSuccessAndCapturesOutput(t *testing.T) {
	a := &captureAction{stdout: []byte("OK\nready"), stderr: []byte("warn: foo")}
	d := dispatcherWithCapture(t, "h", a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	defer d.Stop()

	d.Publish(Event{Type: "alert", Repo: "o/r", IssueNum: 7})
	waitFor(t, func() bool { return a.calls.Load() == 1 })

	// Give the worker a beat to write the invocation record after the action returns.
	waitFor(t, func() bool {
		invs, _ := d.Invocations("h", 0)
		return len(invs) == 1
	})
	invs, ok := d.Invocations("h", 0)
	if !ok {
		t.Fatalf("hook not found")
	}
	if len(invs) != 1 {
		t.Fatalf("invocations=%d want 1", len(invs))
	}
	got := invs[0]
	if got.Result != InvocationResultSuccess {
		t.Fatalf("result=%q want success", got.Result)
	}
	if got.Stdout != "OK\nready" {
		t.Fatalf("stdout=%q", got.Stdout)
	}
	if got.Stderr != "warn: foo" {
		t.Fatalf("stderr=%q", got.Stderr)
	}
	if got.EventType != "alert" || got.Repo != "o/r" || got.IssueNum != 7 {
		t.Fatalf("metadata not propagated: %+v", got)
	}
	if got.DurationNs < 0 {
		t.Fatalf("duration should be ≥0: %d", got.DurationNs)
	}
}

func TestDispatcherInvocationRecordsFailure(t *testing.T) {
	a := &captureAction{err: errors.New("boom")}
	d := dispatcherWithCapture(t, "h", a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	defer d.Stop()

	d.Publish(Event{Type: "alert"})
	waitFor(t, func() bool {
		invs, _ := d.Invocations("h", 0)
		return len(invs) == 1
	})
	invs, _ := d.Invocations("h", 0)
	if invs[0].Result != InvocationResultFailure {
		t.Fatalf("result=%q want failure", invs[0].Result)
	}
	if invs[0].Error != "boom" {
		t.Fatalf("error=%q", invs[0].Error)
	}
}

func TestDispatcherInvocationsRingBufferOldestEvicted(t *testing.T) {
	a := &captureAction{stdout: []byte("ok")}
	d := dispatcherWithCapture(t, "h", a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	defer d.Stop()

	// The per-hook channel only has 1 slot, so publishing in a tight loop
	// would just overflow. Send one at a time and let the worker drain.
	const total = invocationBufferSize + 10
	for i := 0; i < total; i++ {
		d.Publish(Event{Type: "alert", IssueNum: i})
		want := int64(i + 1)
		waitFor(t, func() bool { return a.calls.Load() >= want })
	}
	waitFor(t, func() bool {
		invs, _ := d.Invocations("h", 0)
		return len(invs) == invocationBufferSize
	})
	invs, _ := d.Invocations("h", 0)
	if len(invs) != invocationBufferSize {
		t.Fatalf("len=%d want %d", len(invs), invocationBufferSize)
	}
	if invs[0].IssueNum != total-1 {
		t.Fatalf("newest issue_num=%d want %d", invs[0].IssueNum, total-1)
	}
	expectOldest := total - invocationBufferSize
	if invs[len(invs)-1].IssueNum != expectOldest {
		t.Fatalf("oldest issue_num=%d want %d", invs[len(invs)-1].IssueNum, expectOldest)
	}
}

func TestDispatcherInvocationsLimitClamps(t *testing.T) {
	a := &captureAction{stdout: []byte("ok")}
	d := dispatcherWithCapture(t, "h", a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	defer d.Stop()

	for i := 0; i < 5; i++ {
		d.Publish(Event{Type: "alert", IssueNum: i})
		want := int64(i + 1)
		waitFor(t, func() bool { return a.calls.Load() >= want })
	}
	waitFor(t, func() bool {
		invs, _ := d.Invocations("h", 0)
		return len(invs) == 5
	})

	got, ok := d.Invocations("h", 3)
	if !ok || len(got) != 3 {
		t.Fatalf("limit=3 returned len=%d ok=%v", len(got), ok)
	}
	// Newest first, so the limited slice keeps the freshest.
	if got[0].IssueNum != 4 || got[2].IssueNum != 2 {
		t.Fatalf("limit slice off: %v", []int{got[0].IssueNum, got[1].IssueNum, got[2].IssueNum})
	}
}

func TestDispatcherInvocationsUnknownHook(t *testing.T) {
	a := &captureAction{stdout: []byte("ok")}
	d := dispatcherWithCapture(t, "h", a)
	if _, ok := d.Invocations("does-not-exist", 0); ok {
		t.Fatalf("expected ok=false for missing hook")
	}
}

func TestDispatcherInvocationStdoutTruncated(t *testing.T) {
	huge := strings.Repeat("x", invocationPreviewLimit*2)
	a := &captureAction{stdout: []byte(huge)}
	d := dispatcherWithCapture(t, "h", a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	defer d.Stop()

	d.Publish(Event{Type: "alert"})
	waitFor(t, func() bool {
		invs, _ := d.Invocations("h", 0)
		return len(invs) == 1
	})
	invs, _ := d.Invocations("h", 0)
	if len(invs[0].Stdout) != invocationPreviewLimit {
		t.Fatalf("stdout len=%d want %d", len(invs[0].Stdout), invocationPreviewLimit)
	}
}
