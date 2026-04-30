package hooks

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// channelCapacity is the dispatcher's bounded buffer per design doc.
const channelCapacity = 1024

// DefaultHookTimeout is applied to actions that don't override `timeout:`.
const DefaultHookTimeout = 5 * time.Second

// AutoDisableThreshold is the consecutive failure count that trips a hook
// into the disabled state. Per design (docs/decisions/2026-04-30-hook-system.md)
// no half-open probing — operator must `workbuddy hooks reload` to re-enable.
const AutoDisableThreshold = 5

// HookDisabledEvent is the eventlog type emitted when a hook trips
// auto-disable. Lives under HookEventPrefix so it does not re-enter the
// dispatcher (self-amplification guard).
const HookDisabledEvent = HookEventPrefix + "disabled"

// boundHook pairs a parsed Hook with its constructed Action and tracks the
// per-hook auto-disable state and observability counters.
type boundHook struct {
	hook   *Hook
	action Action

	mu              sync.Mutex
	failures        int
	disabled        bool
	lastErr         string
	lastFailureAt   time.Time
	successCount    uint64
	failureCount    uint64
	filteredCount   uint64
	disabledDrops   uint64
	durationSumNs   uint64
	durationCount   uint64
	durationBuckets [hookDurationBucketCount]uint64
	lastInvokedAt   time.Time
}

// hookDurationBuckets defines the histogram bucket upper bounds (seconds).
// Aligned with Prometheus default-style ladder for sub-second→multi-second
// hook actions.
var hookDurationBuckets = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

const hookDurationBucketCount = 11

// EventEmitter is the optional callback the dispatcher uses to surface
// hook-system self-events (notably hook_disabled) back into the eventlog.
// Signature matches eventlog.EventLogger.Log so wiring is `WithEventEmitter(evlog.Log)`.
type EventEmitter func(eventType, repo string, issueNum int, payload interface{})

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
//   - Per-hook timeout (default 5s); command actions are SIGTERMed then
//     SIGKILLed after a 2s grace.
//   - 5 consecutive failures auto-disable a hook in memory; a `hook_disabled`
//     eventlog entry is emitted via EventEmitter (no half-open probing).
type Dispatcher struct {
	hooks  []*boundHook
	in     chan Event
	stopCh chan struct{}
	wg     sync.WaitGroup
	logger *log.Logger

	overflow atomic.Uint64
	dropped  atomic.Uint64

	emitter EventEmitter

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

// WithEventEmitter installs a callback used to push hook self-events back
// into the eventlog. nil disables emission (useful in tests).
func WithEventEmitter(e EventEmitter) DispatcherOption {
	return func(d *Dispatcher) { d.emitter = e }
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
			d.hooks = append(d.hooks, &boundHook{hook: h, action: action})
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
		b.mu.Lock()
		disabled := b.disabled
		b.mu.Unlock()
		out = append(out, HookSummary{
			Name:       b.hook.Name,
			Events:     append([]string(nil), b.hook.Events...),
			ActionType: b.hook.Action.Type,
			Enabled:    b.hook.IsEnabled() && !disabled,
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

// IsDisabled reports whether a hook has been auto-disabled. Exposed for the
// `hooks list` / `hooks status` surfaces and tests.
func (d *Dispatcher) IsDisabled(hookName string) bool {
	for _, b := range d.hooks {
		if b.hook.Name == hookName {
			b.mu.Lock()
			defer b.mu.Unlock()
			return b.disabled
		}
	}
	return false
}

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
		b.mu.Lock()
		disabled := b.disabled
		b.mu.Unlock()
		if disabled {
			b.mu.Lock()
			b.disabledDrops++
			b.mu.Unlock()
			continue
		}
		if !b.hook.MatchesFilter(ev) {
			b.mu.Lock()
			b.filteredCount++
			b.mu.Unlock()
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

func (d *Dispatcher) runHook(ctx context.Context, b *boundHook, ch chan Event) {
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

func (d *Dispatcher) executeOne(ctx context.Context, b *boundHook, ev Event) {
	envelope := BuildEnvelope(ev)
	payload, err := MarshalEnvelope(envelope)
	if err != nil {
		d.logger.Printf("hooks: hook %q: marshal envelope: %v", b.hook.Name, err)
		return
	}

	timeout := b.hook.Timeout
	if timeout <= 0 {
		timeout = DefaultHookTimeout
	}
	start := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	err = b.action.Execute(runCtx, ev, payload)
	cancel()
	elapsed := time.Since(start)

	if err == nil {
		b.mu.Lock()
		b.failures = 0
		b.lastErr = ""
		b.successCount++
		b.lastInvokedAt = time.Now()
		b.observeDurationLocked(elapsed)
		b.mu.Unlock()
		return
	}

	d.logger.Printf("hooks: hook %q action %s failed: %v", b.hook.Name, b.action.Type(), err)

	b.mu.Lock()
	b.failures++
	b.lastErr = err.Error()
	b.failureCount++
	b.lastFailureAt = time.Now()
	b.lastInvokedAt = b.lastFailureAt
	b.observeDurationLocked(elapsed)
	tripped := false
	if !b.disabled && b.failures >= AutoDisableThreshold {
		b.disabled = true
		tripped = true
	}
	lastErr := b.lastErr
	b.mu.Unlock()

	if tripped {
		d.emitDisabled(b.hook.Name, lastErr)
	}
}

// emitDisabled forwards a hook_disabled event to the eventlog (if an emitter
// is wired). The dispatcher's Publish itself filters hook_* events, so even
// without the emitter this never re-enters the dispatcher.
func (d *Dispatcher) emitDisabled(hookName, lastErr string) {
	d.logger.Printf("hooks: hook %q auto-disabled after %d consecutive failures: %s", hookName, AutoDisableThreshold, lastErr)
	if d.emitter == nil {
		return
	}
	d.emitter(HookDisabledEvent, "", 0, map[string]interface{}{
		"hook":            hookName,
		"failures":        AutoDisableThreshold,
		"last_error":      lastErr,
		"requires_reload": true,
	})
}

// observeDurationLocked records an action duration in the histogram. Caller
// must hold b.mu.
func (b *boundHook) observeDurationLocked(d time.Duration) {
	b.durationCount++
	b.durationSumNs += uint64(d.Nanoseconds())
	secs := d.Seconds()
	for i, upper := range hookDurationBuckets {
		if secs <= upper {
			b.durationBuckets[i]++
		}
	}
}

// HookStats is a snapshot of one hook's runtime counters and disable state.
type HookStats struct {
	Name            string
	Events          []string
	ActionType      string
	Enabled         bool
	Disabled        bool
	Successes       uint64
	Failures        uint64
	Filtered        uint64
	DisabledDrops   uint64
	ConsecutiveFail int
	LastError       string
	LastFailureAt   time.Time
	LastInvokedAt   time.Time
	DurationCount   uint64
	DurationSumNs   uint64
	DurationBuckets [hookDurationBucketCount]uint64
}

// DurationBucketUpperBounds exposes the histogram bucket layout for renderers
// (CLI, Prometheus). Index aligns with HookStats.DurationBuckets.
func DurationBucketUpperBounds() []float64 {
	out := make([]float64, len(hookDurationBuckets))
	copy(out, hookDurationBuckets[:])
	return out
}

// Stats returns a per-hook snapshot for status/metrics surfaces. Safe to call
// from any goroutine. The order matches Hooks().
func (d *Dispatcher) Stats() []HookStats {
	if d == nil {
		return nil
	}
	out := make([]HookStats, 0, len(d.hooks))
	for _, b := range d.hooks {
		b.mu.Lock()
		stats := HookStats{
			Name:            b.hook.Name,
			Events:          append([]string(nil), b.hook.Events...),
			ActionType:      b.hook.Action.Type,
			Enabled:         b.hook.IsEnabled() && !b.disabled,
			Disabled:        b.disabled,
			Successes:       b.successCount,
			Failures:        b.failureCount,
			Filtered:        b.filteredCount,
			DisabledDrops:   b.disabledDrops,
			ConsecutiveFail: b.failures,
			LastError:       b.lastErr,
			LastFailureAt:   b.lastFailureAt,
			LastInvokedAt:   b.lastInvokedAt,
			DurationCount:   b.durationCount,
			DurationSumNs:   b.durationSumNs,
			DurationBuckets: b.durationBuckets,
		}
		b.mu.Unlock()
		out = append(out, stats)
	}
	return out
}

// Reload swaps the dispatcher's hook bindings. Called by `workbuddy hooks
// reload`. Reload preserves nothing — auto-disable state is cleared (the
// design's explicit re-enable path) and metrics counters reset to 0 so
// "since reload" semantics are unambiguous on the status surface.
//
// Reload is safe to call concurrently with Publish: the central channel
// keeps draining; any in-flight executeOne on a still-running worker
// completes against the prior boundHook copy. New events route through the
// new bindings as soon as the worker pool is replaced.
func (d *Dispatcher) Reload(cfg *Config, registry *ActionRegistry) ([]string, error) {
	if d == nil {
		return nil, fmt.Errorf("hooks: dispatcher is nil")
	}
	if registry == nil {
		registry = DefaultActionRegistry()
	}
	var newHooks []*boundHook
	var warnings []string
	if cfg != nil {
		for i := range cfg.Hooks {
			h := &cfg.Hooks[i]
			if !h.IsEnabled() {
				continue
			}
			action, w, err := registry.Build(h)
			if err != nil {
				return nil, err
			}
			warnings = append(warnings, w...)
			newHooks = append(newHooks, &boundHook{hook: h, action: action})
		}
	}

	d.mu.Lock()
	wasStarted := d.started
	// Tear down the old worker pool by closing stopCh, then build a fresh one
	// and start it under the same dispatcher context if we were running.
	if d.started && !d.stopped {
		close(d.stopCh)
		d.stopped = true
	}
	d.mu.Unlock()
	if wasStarted {
		d.wg.Wait()
	}

	d.mu.Lock()
	d.hooks = newHooks
	d.subQueue = map[string]chan Event{}
	d.stopCh = make(chan struct{})
	d.stopped = false
	if wasStarted {
		// reuse the dispatcher's existing in channel so buffered events are
		// not lost across reload; only workers are rebuilt.
		d.started = false
	}
	d.mu.Unlock()

	if wasStarted {
		d.Start(context.Background())
	}
	return warnings, nil
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

