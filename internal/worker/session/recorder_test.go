package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/store"
)

func TestRecorderCapturePropagatesDegradedStorageHealth(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	st, err := store.NewStore(filepath.Join(tmp, "sessions.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	recorder := NewRecorder(st, filepath.Join(tmp, ".workbuddy", "sessions"))
	sessionID := "session-123"
	result := &runtimepkg.Result{ExitCode: 0}

	if err := recorder.RecordEventSession(sessionID, "token_usage", "owner/repo", 42, map[string]any{"bad": func() {}}); err == nil {
		t.Fatal("RecordEventSession error = nil, want marshal failure")
	}
	if got := result.Meta; got != nil {
		t.Fatalf("result meta mutated before capture: %#v", got)
	}

	if err := recorder.Capture(sessionID, "task-123", "owner/repo", 42, "dev-agent", result); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if result.Meta[runtimepkg.MetaStorageDegraded] != "true" {
		t.Fatalf("storage degraded meta = %q, want true", result.Meta[runtimepkg.MetaStorageDegraded])
	}
	issuesJSON := result.Meta[runtimepkg.MetaStorageIssues]
	if issuesJSON == "" {
		t.Fatal("storage issues meta missing")
	}
	if !strings.Contains(issuesJSON, "unsupported type") {
		t.Fatalf("storage issues = %q, want marshal error", issuesJSON)
	}

	data, err := os.ReadFile(filepath.Join(tmp, ".workbuddy", "sessions", sessionID, "health.json"))
	if err != nil {
		t.Fatalf("ReadFile(health.json): %v", err)
	}
	var health SessionHealth
	if err := json.Unmarshal(data, &health); err != nil {
		t.Fatalf("Unmarshal(health.json): %v", err)
	}
	if !health.Degraded || len(health.Issues) != 1 {
		t.Fatalf("health = %+v, want one degraded issue", health)
	}
	if health.Issues[0].Scope != "eventlog" {
		t.Fatalf("health scope = %q, want eventlog", health.Issues[0].Scope)
	}
}

func TestRecorderCaptureFailureMarksResultMeta(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	st, err := store.NewStore(filepath.Join(tmp, "sessions.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	recorder := NewRecorder(st, filepath.Join(tmp, ".workbuddy", "sessions"))
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	result := &runtimepkg.Result{ExitCode: 0}
	err = recorder.Capture("session-closed", "task-closed", "owner/repo", 7, "dev-agent", result)
	if err == nil {
		t.Fatal("Capture error = nil, want closed-store failure")
	}
	if result.Meta[runtimepkg.MetaStorageDegraded] != "true" {
		t.Fatalf("storage degraded meta = %q, want true", result.Meta[runtimepkg.MetaStorageDegraded])
	}
	if !strings.Contains(result.Meta[runtimepkg.MetaStorageIssues], "closed") {
		t.Fatalf("storage issues = %q, want closed-store failure", result.Meta[runtimepkg.MetaStorageIssues])
	}
}
