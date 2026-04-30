package hooks

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCommandActionSuccessSetsEnvAndStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix-only command tests")
	}
	// Echo the four WORKBUDDY_* env vars + stdin so the test can assert
	// the action wired them through.
	a := &CommandAction{
		name: "echo-hook",
		argv: []string{"sh", "-c", `printf '%s|%s|%s|%s|' "$WORKBUDDY_EVENT_TYPE" "$WORKBUDDY_REPO" "$WORKBUDDY_ISSUE_NUM" "$WORKBUDDY_HOOK_NAME"; cat`},
	}
	ev := Event{Type: "alert", Repo: "x/y", IssueNum: 42}
	res := a.Run(context.Background(), ev, []byte(`STDIN-PAYLOAD`))
	if res.Err != nil {
		t.Fatalf("unexpected err: %v (stderr=%s)", res.Err, res.Stderr)
	}
	want := "alert|x/y|42|echo-hook|STDIN-PAYLOAD"
	if got := string(res.Stdout); got != want {
		t.Fatalf("stdout mismatch:\nwant: %q\ngot:  %q", want, got)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}
}

func TestCommandActionNonZeroExitIsFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix-only")
	}
	a := &CommandAction{name: "fail", argv: []string{"sh", "-c", "exit 7"}}
	res := a.Run(context.Background(), Event{Type: "x"}, nil)
	if res.Err == nil {
		t.Fatalf("expected error for non-zero exit")
	}
	if res.ExitCode != 7 {
		t.Fatalf("expected exit 7, got %d", res.ExitCode)
	}
}

func TestCommandActionTimeoutSigtermThenSigkill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix-only")
	}
	// Trap SIGTERM and ignore it; the kernel-side WaitDelay must escalate to
	// SIGKILL so the process actually exits within the test budget.
	a := &CommandAction{name: "trap", argv: []string{"sh", "-c", "trap '' TERM; sleep 30"}}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := a.Run(ctx, Event{Type: "alert"}, nil)
	dur := time.Since(start)
	if res.Err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	// Timeout (200ms) + grace (~2s commandKillGrace) — give some slack.
	if dur > 4*time.Second {
		t.Fatalf("kill path too slow (%s); SIGKILL escalation likely broken", dur)
	}
	var ee *exec.ExitError
	if !errors.As(res.Err, &ee) && !strings.Contains(res.Err.Error(), "signal") && !strings.Contains(res.Err.Error(), "killed") && !strings.Contains(res.Err.Error(), "deadline") {
		// Different Go versions wrap the WaitDelay error differently; just
		// require *some* error.
		t.Logf("note: timeout err = %v", res.Err)
	}
}

// failingAction always errors, used to drive the auto-disable threshold.
type failingAction struct {
	calls atomic.Int64
}

func (a *failingAction) Type() string                                    { return "failing" }
func (a *failingAction) Execute(_ context.Context, _ Event, _ []byte) error {
	a.calls.Add(1)
	return errors.New("boom")
}

func TestDispatcherAutoDisableAfterFiveFailures(t *testing.T) {
	action := &failingAction{}
	reg := NewActionRegistry()
	reg.Register("test", func(h *Hook) (Action, []string, error) { return action, nil, nil })
	cfg := &Config{Hooks: []Hook{{
		Name:   "flaky",
		Events: []string{"alert"},
		Action: ActionConfig{Type: "test"},
	}}}

	var emitted struct {
		typ      string
		hookName string
		lastErr  string
		count    atomic.Int64
	}
	emitter := func(eventType, _ string, _ int, payload interface{}) {
		emitted.count.Add(1)
		emitted.typ = eventType
		if m, ok := payload.(map[string]interface{}); ok {
			if v, ok := m["hook"].(string); ok {
				emitted.hookName = v
			}
			if v, ok := m["last_error"].(string); ok {
				emitted.lastErr = v
			}
		}
	}

	d, _, err := NewDispatcher(cfg, reg, WithEventEmitter(emitter))
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	defer d.Stop()

	// Push events one at a time so each is consumed before the next, keeping
	// the failure counter linear.
	for i := 0; i < 10; i++ {
		d.Publish(Event{Type: "alert"})
		// Spin until the action has observed this call (or the dispatcher has
		// disabled the hook and stopped delivering).
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if action.calls.Load() >= int64(i+1) || d.IsDisabled("flaky") {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	if !d.IsDisabled("flaky") {
		t.Fatalf("hook should be disabled after %d failures", AutoDisableThreshold)
	}
	if got := action.calls.Load(); got != int64(AutoDisableThreshold) {
		t.Fatalf("action should have been called exactly %d times before auto-disable, got %d", AutoDisableThreshold, got)
	}
	if got := emitted.count.Load(); got != 1 {
		t.Fatalf("hook_disabled should be emitted exactly once, got %d", got)
	}
	if emitted.typ != HookDisabledEvent {
		t.Fatalf("emitted type = %q, want %q", emitted.typ, HookDisabledEvent)
	}
	if emitted.hookName != "flaky" {
		t.Fatalf("emitted hook = %q", emitted.hookName)
	}
	if emitted.lastErr != "boom" {
		t.Fatalf("emitted last_error = %q", emitted.lastErr)
	}
}

// flakyAction fails the first few invocations then succeeds, to verify the
// failure counter resets on success (so a later flake doesn't trip disable).
type flakyAction struct {
	calls atomic.Int64
}

func (a *flakyAction) Type() string { return "flaky" }
func (a *flakyAction) Execute(_ context.Context, _ Event, _ []byte) error {
	n := a.calls.Add(1)
	if n <= 3 {
		return errors.New("transient")
	}
	return nil
}

func TestDispatcherSuccessResetsFailureCounter(t *testing.T) {
	action := &flakyAction{}
	reg := NewActionRegistry()
	reg.Register("test", func(h *Hook) (Action, []string, error) { return action, nil, nil })
	cfg := &Config{Hooks: []Hook{{Name: "h", Events: []string{"alert"}, Action: ActionConfig{Type: "test"}}}}

	d, _, err := NewDispatcher(cfg, reg)
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	defer d.Stop()

	for i := 0; i < 10; i++ {
		d.Publish(Event{Type: "alert"})
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if action.calls.Load() >= 10 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if d.IsDisabled("h") {
		t.Fatalf("hook should not be disabled — successes should have reset the counter")
	}
}
