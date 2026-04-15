package launcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/store"
)

func TestSessionManagerLifecycle(t *testing.T) {
	st := newTestStoreForManager(t)
	baseDir := filepath.Join(t.TempDir(), ".workbuddy", "sessions")
	manager := NewSessionManager(baseDir, st)

	handle, err := manager.Create(SessionCreateInput{
		SessionID: "session-1",
		TaskID:    "task-1",
		Repo:      "owner/repo",
		IssueNum:  39,
		AgentName: "dev-agent",
		Runtime:   "codex-exec",
		WorkerID:  "worker-1",
		Attempt:   2,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := handle.WriteStdout([]byte("out\n")); err != nil {
		t.Fatalf("WriteStdout: %v", err)
	}
	if err := handle.WriteStderr([]byte("err\n")); err != nil {
		t.Fatalf("WriteStderr: %v", err)
	}
	if err := handle.WriteToolCall([]byte("{\"kind\":\"tool.call\"}\n")); err != nil {
		t.Fatalf("WriteToolCall: %v", err)
	}
	if err := handle.Close(store.TaskStatusCompleted); err != nil {
		t.Fatalf("Close: %v", err)
	}

	record, err := manager.Get("session-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if record == nil {
		t.Fatal("expected session record")
	}
	if record.Status != store.TaskStatusCompleted || record.Attempt != 2 {
		t.Fatalf("unexpected record: %+v", record)
	}

	sessions, err := manager.List(store.SessionFilter{Repo: "owner/repo", AgentName: "dev-agent"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}

	for _, path := range []string{handle.StdoutPath(), handle.StderrPath(), handle.ToolCallsPath(), handle.MetadataPath()} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected artifact %s: %v", path, err)
		}
	}

	data, err := os.ReadFile(handle.MetadataPath())
	if err != nil {
		t.Fatalf("ReadFile(metadata): %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("Unmarshal(metadata): %v", err)
	}
	if meta["status"] != store.TaskStatusCompleted {
		t.Fatalf("metadata status = %v", meta["status"])
	}
}

func TestSessionManagerConcurrentWrites(t *testing.T) {
	manager := NewSessionManager(filepath.Join(t.TempDir(), ".workbuddy", "sessions"), nil)
	handle, err := manager.Create(SessionCreateInput{
		SessionID: "session-2",
		Repo:      "owner/repo",
		IssueNum:  39,
		AgentName: "dev-agent",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = handle.WriteStdout([]byte("o\n"))
			_ = handle.WriteStderr([]byte("e\n"))
			_ = handle.WriteToolCall([]byte("{\"runtime\":\"codex\"}\n"))
		}()
	}
	wg.Wait()
	if err := handle.Close(store.TaskStatusCompleted); err != nil {
		t.Fatalf("Close: %v", err)
	}

	stdoutData, err := os.ReadFile(handle.StdoutPath())
	if err != nil {
		t.Fatalf("ReadFile(stdout): %v", err)
	}
	if got := strings.Count(string(stdoutData), "o\n"); got != n {
		t.Fatalf("stdout writes = %d, want %d", got, n)
	}
	stderrData, err := os.ReadFile(handle.StderrPath())
	if err != nil {
		t.Fatalf("ReadFile(stderr): %v", err)
	}
	if got := strings.Count(string(stderrData), "e\n"); got != n {
		t.Fatalf("stderr writes = %d, want %d", got, n)
	}
	toolData, err := os.ReadFile(handle.ToolCallsPath())
	if err != nil {
		t.Fatalf("ReadFile(tool-calls): %v", err)
	}
	if got := strings.Count(string(toolData), "\n"); got != n {
		t.Fatalf("tool call writes = %d, want %d", got, n)
	}
}

func newTestStoreForManager(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.NewStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
