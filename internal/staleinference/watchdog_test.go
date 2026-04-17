package staleinference

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fakeChecker struct {
	mtime       time.Time
	mtimeErr    error
	hasChildren bool
	childErr    error
}

func (f *fakeChecker) SessionFileModTime() (time.Time, error) {
	return f.mtime, f.mtimeErr
}

func (f *fakeChecker) HasChildProcesses() (bool, error) {
	return f.hasChildren, f.childErr
}

func TestIsStale_NoSessionFile(t *testing.T) {
	cfg := Config{IdleThreshold: 10 * time.Minute}
	checker := &fakeChecker{mtimeErr: errors.New("no file")}
	if isStale(cfg, checker) {
		t.Fatal("expected not stale when session file doesn't exist")
	}
}

func TestIsStale_RecentOutput(t *testing.T) {
	cfg := Config{IdleThreshold: 10 * time.Minute}
	checker := &fakeChecker{mtime: time.Now().Add(-1 * time.Minute)}
	if isStale(cfg, checker) {
		t.Fatal("expected not stale when output is recent")
	}
}

func TestIsStale_OldOutputWithChildren(t *testing.T) {
	cfg := Config{IdleThreshold: 10 * time.Minute}
	checker := &fakeChecker{
		mtime:       time.Now().Add(-15 * time.Minute),
		hasChildren: true,
	}
	if isStale(cfg, checker) {
		t.Fatal("expected not stale when process has children")
	}
}

func TestIsStale_OldOutputNoChildren(t *testing.T) {
	cfg := Config{IdleThreshold: 10 * time.Minute}
	checker := &fakeChecker{
		mtime:       time.Now().Add(-15 * time.Minute),
		hasChildren: false,
	}
	if !isStale(cfg, checker) {
		t.Fatal("expected stale when no output and no children")
	}
}

func TestIsStale_ChildCheckError(t *testing.T) {
	cfg := Config{IdleThreshold: 10 * time.Minute}
	checker := &fakeChecker{
		mtime:    time.Now().Add(-15 * time.Minute),
		childErr: errors.New("proc error"),
	}
	if isStale(cfg, checker) {
		t.Fatal("expected not stale when child check errors")
	}
}

func TestWatch_CancelsOnStale(t *testing.T) {
	ctx, outerCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer outerCancel()

	taskCtx, taskCancel := context.WithCancel(ctx)

	checker := &fakeChecker{
		mtime:       time.Now().Add(-15 * time.Minute),
		hasChildren: false,
	}
	cfg := Config{
		IdleThreshold: 10 * time.Minute,
		CheckInterval: 10 * time.Millisecond,
	}

	done := make(chan struct{})
	go func() {
		Watch(taskCtx, cfg, checker, taskCancel)
		close(done)
	}()

	select {
	case <-done:
		// Watch returned, which means it called taskCancel.
		// Verify the context is cancelled.
		if taskCtx.Err() == nil {
			t.Fatal("expected taskCtx to be cancelled")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for watchdog to fire")
	}
}

func TestWatch_DoesNotCancelWhenHealthy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var cancelled atomic.Bool
	taskCancel := func() { cancelled.Store(true) }

	checker := &fakeChecker{
		mtime:       time.Now(),
		hasChildren: true,
	}
	cfg := Config{
		IdleThreshold: 10 * time.Minute,
		CheckInterval: 10 * time.Millisecond,
	}

	Watch(ctx, cfg, checker, taskCancel)
	if cancelled.Load() {
		t.Fatal("expected cancel NOT to be called for healthy process")
	}
}

func TestWatch_DefaultConfig(t *testing.T) {
	cfg := Config{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	checker := &fakeChecker{mtime: time.Now()}
	Watch(ctx, cfg, checker, func() {})
	// Just ensure it doesn't panic or hang.
}

func TestEventTracker_RecordActivity(t *testing.T) {
	tracker := NewEventTracker()
	before := time.Now()
	time.Sleep(time.Millisecond)
	tracker.RecordActivity()
	time.Sleep(time.Millisecond)

	mtime, err := tracker.SessionFileModTime()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mtime.Before(before) {
		t.Fatal("mtime should be after test start")
	}
}

func TestEventTracker_HasChildProcesses(t *testing.T) {
	tracker := NewEventTracker()
	has, err := tracker.HasChildProcesses()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if has {
		t.Fatal("EventTracker should always report no children")
	}
}

func TestEventTracker_StaleWhenNoRecentActivity(t *testing.T) {
	tracker := &EventTracker{}
	// Manually set last activity to 15 minutes ago.
	tracker.lastActivity.Store(time.Now().Add(-15 * time.Minute).UnixNano())

	cfg := Config{IdleThreshold: 10 * time.Minute}
	if !isStale(cfg, tracker) {
		t.Fatal("expected stale when EventTracker has old activity")
	}
}

func TestEventTracker_NotStaleWhenRecentActivity(t *testing.T) {
	tracker := NewEventTracker()
	cfg := Config{IdleThreshold: 10 * time.Minute}
	if isStale(cfg, tracker) {
		t.Fatal("expected not stale when EventTracker has recent activity")
	}
}

func TestIsStale_ThresholdBoundary(t *testing.T) {
	// Exactly at threshold should not trigger (needs to exceed).
	cfg := Config{IdleThreshold: 10 * time.Minute}
	checker := &fakeChecker{
		mtime:       time.Now().Add(-10*time.Minute + time.Second),
		hasChildren: false,
	}
	if isStale(cfg, checker) {
		t.Fatal("expected not stale at boundary")
	}
}
