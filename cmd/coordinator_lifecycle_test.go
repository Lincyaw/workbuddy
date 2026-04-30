package cmd

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// emitCoordinatorStarted/Stopping must produce eventlog entries with
// payload fields the operator dashboards rely on.
func TestEmitCoordinatorLifecycleEvents(t *testing.T) {
	dir := t.TempDir()
	st, err := store.NewStore(filepath.Join(dir, "lc.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	evlog := eventlog.NewEventLogger(st)

	emitCoordinatorStarted(evlog, "coordinator", "127.0.0.1:9999")
	emitCoordinatorStopping(evlog, "coordinator", "127.0.0.1:9999")

	// Wait for both — Log() is synchronous against SQLite, but allow a beat
	// for the dispatcher publish path that may run on a goroutine.
	deadline := time.Now().Add(time.Second)
	var started, stopping []store.Event
	for time.Now().Before(deadline) {
		started, _ = evlog.Query(eventlog.EventFilter{Type: eventlog.TypeCoordinatorStarted})
		stopping, _ = evlog.Query(eventlog.EventFilter{Type: eventlog.TypeCoordinatorStopping})
		if len(started) > 0 && len(stopping) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(started) == 0 {
		t.Fatalf("coordinator_started not recorded")
	}
	if len(stopping) == 0 {
		t.Fatalf("coordinator_stopping not recorded")
	}
	for _, want := range []string{`"listen":"127.0.0.1:9999"`, `"pid":`} {
		if !contains(started[0].Payload, want) {
			t.Errorf("started payload missing %s: %s", want, started[0].Payload)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || stringContains(haystack, needle))
}

// stringContains is a tiny re-roll to avoid importing strings just for tests
// that already pull eventlog/store.
func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
