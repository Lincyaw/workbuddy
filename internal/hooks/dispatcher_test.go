package hooks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingAction blocks until released; lets us drive overflow / queue-full
// scenarios deterministically.
type blockingAction struct {
	gate    chan struct{}
	count   atomic.Int64
}

func (a *blockingAction) Type() string { return "blocking" }
func (a *blockingAction) Execute(ctx context.Context, _ Event, _ []byte) error {
	a.count.Add(1)
	select {
	case <-a.gate:
	case <-ctx.Done():
	}
	return nil
}

// countingAction just counts calls without blocking.
type countingAction struct {
	count atomic.Int64
	done  chan struct{}
	once  sync.Once
}

func (a *countingAction) Type() string { return "counting" }
func (a *countingAction) Execute(_ context.Context, _ Event, _ []byte) error {
	a.count.Add(1)
	a.once.Do(func() { close(a.done) })
	return nil
}

func makeDispatcherWithAction(t *testing.T, name string, events []string, action Action) *Dispatcher {
	t.Helper()
	reg := NewActionRegistry()
	reg.Register("test", func(h *Hook) (Action, []string, error) { return action, nil, nil })
	cfg := &Config{
		SchemaVersion: 1,
		Hooks: []Hook{{
			Name:   name,
			Events: events,
			Action: ActionConfig{Type: "test"},
		}},
	}
	d, _, err := NewDispatcher(cfg, reg)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return d
}

func TestDispatcherPublishIsAsync(t *testing.T) {
	gate := make(chan struct{})
	action := &blockingAction{gate: gate}
	d := makeDispatcherWithAction(t, "h1", []string{"alert"}, action)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	defer d.Stop()

	// Publish 1000 events; caller must never block. Use a watchdog.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			d.Publish(Event{Type: "alert"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Publish blocked under load")
	}
	close(gate)
}

func TestDispatcherOverflowIncrementsCounter(t *testing.T) {
	// Block the worker so the central buffer can fill.
	gate := make(chan struct{})
	action := &blockingAction{gate: gate}
	d := makeDispatcherWithAction(t, "h1", []string{"alert"}, action)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	defer func() {
		close(gate)
		d.Stop()
	}()

	// Push enough to overflow channelCapacity (1024) plus some headroom.
	const total = 2000
	for i := 0; i < total; i++ {
		d.Publish(Event{Type: "alert"})
	}
	// Either central overflow or per-hook drops account for the excess.
	overflow := d.OverflowCount() + d.DroppedCount()
	if overflow == 0 {
		t.Fatalf("expected overflow drops, got 0")
	}
	if overflow > total {
		t.Fatalf("overflow %d exceeds total publishes %d", overflow, total)
	}
}

func TestDispatcherFiltersHookSelfEvents(t *testing.T) {
	action := &countingAction{done: make(chan struct{})}
	d := makeDispatcherWithAction(t, "wildcard", []string{"*"}, action)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	defer d.Stop()

	d.Publish(Event{Type: "hook_overflow"})
	d.Publish(Event{Type: "hook_failed"})
	d.Publish(Event{Type: "alert"})

	select {
	case <-action.done:
	case <-time.After(time.Second):
		t.Fatalf("dispatcher never delivered the non-hook event")
	}
	// Give scheduler a beat to confirm no extra deliveries from hook_* events.
	time.Sleep(50 * time.Millisecond)
	if got := action.count.Load(); got != 1 {
		t.Fatalf("expected exactly 1 delivery (alert only), got %d", got)
	}
}

func TestDispatcherWebhookIntegration(t *testing.T) {
	var (
		mu       sync.Mutex
		received [][]byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		mu.Lock()
		received = append(received, buf)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	yaml := []byte("hooks:\n  - name: w\n    events: [alert]\n    action:\n      type: webhook\n      url: " + srv.URL + "\n")
	cfg, _, err := ParseConfig(yaml)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	d, _, err := NewDispatcher(cfg, DefaultActionRegistry())
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	defer d.Stop()

	d.Publish(Event{Type: "alert", Repo: "x/y", IssueNum: 1, Payload: []byte(`{"k":"v"}`)})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(received)
		mu.Unlock()
		if got > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("webhook never received POST")
}

func TestWebhookActionNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	w := &WebhookAction{url: srv.URL, method: "POST", client: http.DefaultClient}
	if err := w.Execute(context.Background(), Event{}, []byte(`{}`)); err == nil {
		t.Fatalf("expected non-2xx error")
	}
}

func TestWebhookActionTimeoutIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	w := &WebhookAction{url: srv.URL, method: "POST", client: http.DefaultClient}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := w.Execute(ctx, Event{}, []byte(`{}`)); err == nil {
		t.Fatalf("expected timeout/cancelled error")
	}
}
