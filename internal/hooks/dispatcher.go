package hooks

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
)

// channelCapacity is the dispatcher's bounded buffer per design doc.
const channelCapacity = 1024

// boundHook pairs a parsed Hook with its constructed Action.
type boundHook struct {
	hook   *Hook
	action Action
}

// Dispatcher fans event-log entries out to declarative hooks.
//
// Design (see docs/decisions/2026-04-30-hook-system.md):
//   - Publish() never blocks the caller. The internal channel is bounded
//     (channelCapacity); when full, the message is dropped and the
//     overflow counter is incremented.
//   - Each hook has its own goroutine and its own bounded slot, so a slow
//     hook cannot starve other hooks.
//   - Events with a `hook_` prefix are filtered out (see IsHookSelfEvent)
//     so a failing hook bound to `*` does not amplify itself.
type Dispatcher struct {
	hooks  []boundHook
	in     chan Event
	stopCh chan struct{}
	wg     sync.WaitGroup
	logger *log.Logger

	overflow atomic.Uint64
	dropped  atomic.Uint64

	mu       sync.Mutex
	started  bool
	stopped  bool
	subQueue map[string]chan Event
}

// DispatcherOption tunes Dispatcher construction.
type DispatcherOption func(*Dispatcher)

// WithLogger overrides the default stderr logger.
func WithLogger(l *log.Logger) DispatcherOption {
	return func(d *Dispatcher) {
		if l != nil {
			d.logger = l
		}
	}
}

// NewDispatcher binds the parsed config to actions via the registry.
// Returned warnings (e.g. unresolved env vars) are non-fatal.
func NewDispatcher(cfg *Config, registry *ActionRegistry, opts ...DispatcherOption) (*Dispatcher, []string, error) {
	if registry == nil {
		registry = DefaultActionRegistry()
	}
	d := &Dispatcher{
		in:       make(chan Event, channelCapacity),
		stopCh:   make(chan struct{}),
		logger:   log.New(io.Discard, "", 0),
		subQueue: map[string]chan Event{},
	}
	for _, opt := range opts {
		opt(d)
	}

	var warnings []string
	if cfg != nil {
		for i := range cfg.Hooks {
			h := &cfg.Hooks[i]
			if !h.IsEnabled() {
				continue
			}
			action, w, err := registry.Build(h)
			if err != nil {
				return nil, nil, err
			}
			warnings = append(warnings, w...)
			d.hooks = append(d.hooks, boundHook{hook: h, action: action})
		}
	}
	return d, warnings, nil
}

// Hooks returns a snapshot of the bound hooks for `workbuddy hooks list`.
func (d *Dispatcher) Hooks() []HookSummary {
	if d == nil {
		return nil
	}
	out := make([]HookSummary, 0, len(d.hooks))
	for _, b := range d.hooks {
		out = append(out, HookSummary{
			Name:       b.hook.Name,
			Events:     append([]string(nil), b.hook.Events...),
			ActionType: b.hook.Action.Type,
			Enabled:    b.hook.IsEnabled(),
		})
	}
	return out
}

// HookSummary is the projection used by CLI surfaces.
type HookSummary struct {
	Name       string
	Events     []string
	ActionType string
	Enabled    bool
}

// Start launches the central fan-in loop and per-hook workers. Calling Start
// more than once is a no-op.
func (d *Dispatcher) Start(ctx context.Context) {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return
	}
	d.started = true
	for _, b := range d.hooks {
		ch := make(chan Event, 1)
		d.subQueue[b.hook.Name] = ch
		d.wg.Add(1)
		go d.runHook(ctx, b, ch)
	}
	d.mu.Unlock()

	d.wg.Add(1)
	go d.runFanIn(ctx)
}

// Stop signals the dispatcher to drain and waits for workers to exit.
func (d *Dispatcher) Stop() {
	d.mu.Lock()
	if d.stopped || !d.started {
		d.stopped = true
		d.mu.Unlock()
		return
	}
	d.stopped = true
	close(d.stopCh)
	d.mu.Unlock()
	d.wg.Wait()
}

// Publish enqueues an event without blocking. If the central buffer is full
// the event is dropped and OverflowCount() is incremented. hook_* events are
// filtered out per the self-amplification guard.
func (d *Dispatcher) Publish(ev Event) {
	if d == nil {
		return
	}
	if IsHookSelfEvent(ev.Type) {
		return
	}
	select {
	case d.in <- ev:
	default:
		d.overflow.Add(1)
	}
}

// OverflowCount is the cumulative count of central-buffer drops since start.
func (d *Dispatcher) OverflowCount() uint64 { return d.overflow.Load() }

// DroppedCount is the cumulative count of per-hook slot drops (slow hook).
func (d *Dispatcher) DroppedCount() uint64 { return d.dropped.Load() }

func (d *Dispatcher) runFanIn(ctx context.Context) {
	defer d.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stopCh:
			return
		case ev := <-d.in:
			d.fanOut(ev)
		}
	}
}

func (d *Dispatcher) fanOut(ev Event) {
	for _, b := range d.hooks {
		if !b.hook.MatchesEvent(ev.Type) {
			continue
		}
		ch := d.subQueue[b.hook.Name]
		if ch == nil {
			continue
		}
		select {
		case ch <- ev:
		default:
			d.dropped.Add(1)
			d.logger.Printf("hooks: hook %q queue full, dropping event %s", b.hook.Name, ev.Type)
		}
	}
}

func (d *Dispatcher) runHook(ctx context.Context, b boundHook, ch chan Event) {
	defer d.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stopCh:
			return
		case ev := <-ch:
			d.executeOne(ctx, b, ev)
		}
	}
}

func (d *Dispatcher) executeOne(ctx context.Context, b boundHook, ev Event) {
	envelope := BuildEnvelope(ev)
	payload, err := MarshalEnvelope(envelope)
	if err != nil {
		d.logger.Printf("hooks: hook %q: marshal envelope: %v", b.hook.Name, err)
		return
	}
	if err := b.action.Execute(ctx, payload); err != nil {
		d.logger.Printf("hooks: hook %q action %s failed: %v", b.hook.Name, b.action.Type(), err)
		return
	}
}

// PublishFromRaw is a convenience for callers that already have a JSON-encoded
// payload string (matches the eventlog.EventLogger.Log signature).
func (d *Dispatcher) PublishFromRaw(eventType, repo string, issueNum int, payloadJSON string) {
	d.Publish(Event{
		Type:     eventType,
		Repo:     repo,
		IssueNum: issueNum,
		Payload:  []byte(payloadJSON),
	})
}

// Ensure fmt is used (no-op import shield against re-org churn).
var _ = fmt.Sprintf
