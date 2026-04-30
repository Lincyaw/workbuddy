package hooks

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Reload must clear auto-disable state and route subsequent events to the
// new bindings.
func TestDispatcherReloadClearsAutoDisable(t *testing.T) {
	// Build a dispatcher with a hook bound to a failing action so we can
	// trip auto-disable, then reload and confirm the hook is healthy again.
	enabled := true
	cfg1 := &Config{
		SchemaVersion: 1,
		Hooks: []Hook{{
			Name:    "h1",
			Enabled: &enabled,
			Events:  []string{"alert"},
			Action:  ActionConfig{Type: "fail"},
		}},
	}
	reg := NewActionRegistry()
	reg.Register("fail", func(h *Hook) (Action, []string, error) { return &alwaysFailAction{}, nil, nil })
	d, _, err := NewDispatcher(cfg1, reg)
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	defer d.Stop()

	// Trip auto-disable. Publish one at a time and wait for processing
	// (per-hook channel cap is 1, so back-to-back publishes get dropped).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !d.IsDisabled("h1") {
		d.Publish(Event{Type: "alert"})
		time.Sleep(20 * time.Millisecond)
	}
	if !d.IsDisabled("h1") {
		t.Fatalf("hook should be auto-disabled before reload")
	}

	// Reload with a healthy action — auto-disable must clear.
	cfg2 := &Config{
		SchemaVersion: 1,
		Hooks: []Hook{{
			Name:    "h1",
			Enabled: &enabled,
			Events:  []string{"alert"},
			Action:  ActionConfig{Type: "count"},
		}},
	}
	reg2 := NewActionRegistry()
	counter := &countingAction{done: make(chan struct{})}
	reg2.Register("count", func(h *Hook) (Action, []string, error) { return counter, nil, nil })
	if _, err := d.Reload(cfg2, reg2); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if d.IsDisabled("h1") {
		t.Fatalf("reload must clear auto-disable")
	}
	d.Publish(Event{Type: "alert"})
	select {
	case <-counter.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("post-reload event was not delivered")
	}
}

func TestDispatcherReloadFiltersOutDisabledHooks(t *testing.T) {
	disabled := false
	cfg := &Config{
		SchemaVersion: 1,
		Hooks: []Hook{{
			Name:    "h",
			Enabled: &disabled,
			Events:  []string{"alert"},
			Action:  ActionConfig{Type: "count"},
		}},
	}
	reg := NewActionRegistry()
	reg.Register("count", func(h *Hook) (Action, []string, error) { return &countingAction{done: make(chan struct{})}, nil, nil })
	d, _, err := NewDispatcher(cfg, reg)
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	if got := len(d.Stats()); got != 0 {
		t.Fatalf("disabled hook must not be bound; got %d stats", got)
	}
}

type alwaysFailAction struct {
	calls atomic.Int64
}

func (a *alwaysFailAction) Type() string { return "fail" }
func (a *alwaysFailAction) Execute(ctx context.Context, _ Event, _ []byte) error {
	a.calls.Add(1)
	return errAlwaysFail
}

type sentinelErr string

func (e sentinelErr) Error() string { return string(e) }

const errAlwaysFail = sentinelErr("always fail")
