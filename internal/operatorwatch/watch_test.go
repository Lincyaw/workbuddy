package operatorwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/store"
)

func TestRunProcessesIncidentsSequentiallyFromFsnotify(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "operator", "inbox")
	configDir := writeConfig(t, root, true)
	st := newEventStore(t, root)

	runner := &fakeRunner{
		runFn: func(ctx context.Context, _ string, incidentPath string) (int, error) {
			time.Sleep(25 * time.Millisecond)
			return 0, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runService(t, ctx, Options{
		InboxDir:      inbox,
		ConfigDir:     configDir,
		Timeout:       time.Second,
		PauseInterval: time.Hour,
		Logger:        eventlog.NewEventLogger(st),
		Runner:        runner,
	})

	time.Sleep(100 * time.Millisecond)
	for i := 1; i <= 5; i++ {
		writeIncident(t, inbox, filepath.Join(inbox, incidentName(i)), map[string]any{"incident_id": incidentID(i)})
	}

	waitForFiles(t, filepath.Join(root, "operator", "processed"), 5)
	cancel()
	<-done

	if got := runner.maxActiveRuns(); got != 1 {
		t.Fatalf("max concurrent runs = %d, want 1", got)
	}
	if got := len(runner.calls()); got != 5 {
		t.Fatalf("runner calls = %d, want 5", got)
	}

	events, err := st.QueryEvents(DefaultEventRepo)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if got := countEventsByType(events, eventlog.TypeOperatorInvoked); got != 5 {
		t.Fatalf("operator_invoked events = %d, want 5", got)
	}
}

func TestRunTimeoutMovesIncidentToFailed(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "operator", "inbox")
	configDir := writeConfig(t, root, true)
	st := newEventStore(t, root)

	runner := &fakeRunner{
		runFn: func(ctx context.Context, _ string, incidentPath string) (int, error) {
			<-ctx.Done()
			return -1, ctx.Err()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runService(t, ctx, Options{
		InboxDir:      inbox,
		ConfigDir:     configDir,
		Timeout:       100 * time.Millisecond,
		PauseInterval: time.Hour,
		Logger:        eventlog.NewEventLogger(st),
		Runner:        runner,
	})

	time.Sleep(100 * time.Millisecond)
	writeIncident(t, inbox, filepath.Join(inbox, "hang.json"), map[string]any{"incident_id": "hang"})
	waitForFile(t, filepath.Join(root, "operator", "failed", "timeout-hang.json"))
	cancel()
	<-done

	events, err := st.QueryEvents(DefaultEventRepo)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if got := countEventsByType(events, eventlog.TypeOperatorInvoked); got != 1 {
		t.Fatalf("operator_invoked events = %d, want 1", got)
	}
	if !strings.Contains(events[0].Payload, `"exit_code":-1`) {
		t.Fatalf("event payload = %s, want timeout exit code", events[0].Payload)
	}
}

func TestRunHonorsPausedFile(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "operator", "inbox")
	configDir := writeConfig(t, root, true)

	runner := &fakeRunner{
		runFn: func(ctx context.Context, _ string, incidentPath string) (int, error) {
			return 0, nil
		},
	}

	pausedPath := filepath.Join(root, "operator", "paused")
	if err := os.MkdirAll(filepath.Dir(pausedPath), 0o755); err != nil {
		t.Fatalf("mkdir paused dir: %v", err)
	}
	if err := os.WriteFile(pausedPath, []byte("pause"), 0o644); err != nil {
		t.Fatalf("write paused file: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runService(t, ctx, Options{
		InboxDir:      inbox,
		ConfigDir:     configDir,
		Timeout:       time.Second,
		PauseInterval: 50 * time.Millisecond,
		Runner:        runner,
	})

	time.Sleep(100 * time.Millisecond)
	writeIncident(t, inbox, filepath.Join(inbox, "paused.json"), map[string]any{"incident_id": "paused"})
	time.Sleep(150 * time.Millisecond)
	if got := len(runner.calls()); got != 0 {
		t.Fatalf("runner calls while paused = %d, want 0", got)
	}

	if err := os.Remove(pausedPath); err != nil {
		t.Fatalf("remove paused file: %v", err)
	}
	waitForFile(t, filepath.Join(root, "operator", "processed", "paused.json"))
	cancel()
	<-done
}

func TestRunHonorsOperatorEnabledConfig(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "operator", "inbox")
	configDir := writeConfig(t, root, false)

	runner := &fakeRunner{
		runFn: func(ctx context.Context, _ string, incidentPath string) (int, error) {
			return 0, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runService(t, ctx, Options{
		InboxDir:      inbox,
		ConfigDir:     configDir,
		Timeout:       time.Second,
		PauseInterval: 50 * time.Millisecond,
		Runner:        runner,
	})

	time.Sleep(100 * time.Millisecond)
	writeIncident(t, inbox, filepath.Join(inbox, "disabled.json"), map[string]any{"incident_id": "disabled"})
	time.Sleep(150 * time.Millisecond)
	if got := len(runner.calls()); got != 0 {
		t.Fatalf("runner calls while operator disabled = %d, want 0", got)
	}

	writeConfig(t, root, true)
	waitForFile(t, filepath.Join(root, "operator", "processed", "disabled.json"))
	cancel()
	<-done
}

func TestRunRecoversStaleProcessingFiles(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "operator", "inbox")
	configDir := writeConfig(t, root, true)

	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatalf("mkdir inbox: %v", err)
	}
	stalePath := filepath.Join(inbox, "stale.json.processing")
	if err := os.WriteFile(stalePath, []byte(`{"incident_id":"stale"}`), 0o644); err != nil {
		t.Fatalf("write stale processing file: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runService(t, ctx, Options{
		InboxDir:      inbox,
		ConfigDir:     configDir,
		Timeout:       time.Second,
		PauseInterval: time.Hour,
		Runner: &fakeRunner{
			runFn: func(ctx context.Context, _ string, incidentPath string) (int, error) {
				return 0, nil
			},
		},
	})

	waitForFile(t, filepath.Join(root, "operator", "failed", "crash-stale.json"))
	cancel()
	<-done
}

type fakeRunner struct {
	mu        sync.Mutex
	active    int
	maxActive int
	paths     []string
	runFn     func(ctx context.Context, claudePath, incidentPath string) (int, error)
}

func (f *fakeRunner) Run(ctx context.Context, claudePath, incidentPath string) (int, error) {
	f.mu.Lock()
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.paths = append(f.paths, incidentPath)
	f.mu.Unlock()

	exitCode, err := f.runFn(ctx, claudePath, incidentPath)

	f.mu.Lock()
	f.active--
	f.mu.Unlock()
	return exitCode, err
}

func (f *fakeRunner) maxActiveRuns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActive
}

func (f *fakeRunner) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.paths))
	copy(out, f.paths)
	return out
}

func runService(t *testing.T, ctx context.Context, opts Options) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, opts)
	}()
	return done
}

func writeConfig(t *testing.T, root string, enabled bool) string {
	t.Helper()
	configDir := filepath.Join(root, ".github", "workbuddy")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	content := []byte("operator:\n  enabled: " + map[bool]string{true: "true", false: "false"}[enabled] + "\n")
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), content, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return configDir
}

func newEventStore(t *testing.T, root string) *store.Store {
	t.Helper()
	st, err := store.NewStore(filepath.Join(root, "workbuddy.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func writeIncident(t *testing.T, inbox, path string, payload map[string]any) {
	t.Helper()
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatalf("mkdir inbox: %v", err)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal incident: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write incident %s: %v", path, err)
	}
}

func waitForFiles(t *testing.T, dir string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(dir)
		if err == nil && len(entries) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	entries, _ := os.ReadDir(dir)
	t.Fatalf("files in %s = %d, want %d", dir, len(entries), want)
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func countEventsByType(events []store.Event, eventType string) int {
	count := 0
	for _, ev := range events {
		if ev.Type == eventType {
			count++
		}
	}
	return count
}

func incidentID(i int) string {
	return fmt.Sprintf("incident-%d", i)
}

func incidentName(i int) string {
	return incidentID(i) + ".json"
}
