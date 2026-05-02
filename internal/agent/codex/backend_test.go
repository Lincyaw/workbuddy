package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/agent"
	"github.com/Lincyaw/workbuddy/internal/agent/codex/codextest"
)

func TestCodexBackend_DialFailure_FailsFastWithGuidance(t *testing.T) {
	// Pre-REQ-127 the missing-binary test exercised the spawn path. With
	// the WS transport the equivalent failure mode is "no codex sidecar
	// listening on the URL". The error must point operators at the
	// supervisor flag they likely forgot.
	t.Setenv("WORKBUDDY_CODEX_URL", "ws://127.0.0.1:1") // RFC 1700: TCP port 1, intentionally unreachable
	backend, err := NewBackend(Config{ClientName: "test"})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	defer func() { _ = backend.Shutdown(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = backend.NewSession(ctx, agent.Spec{Workdir: t.TempDir(), Prompt: "x"})
	if err == nil {
		t.Fatal("expected dial failure, got nil")
	}
	if !strings.Contains(err.Error(), "supervisor must be started with --codex-binary") {
		t.Fatalf("error must guide operator to the supervisor flag; got: %v", err)
	}
}

func TestCodexSessionLifecycleViaAppServer(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "fake.log")
	srv := codextest.NewServer(t, codextest.Config{Mode: codextest.ModeComplete, LogPath: logPath})
	defer srv.Close()

	t.Setenv("WORKBUDDY_CODEX_URL", srv.URL)
	backend, err := NewBackend(Config{ClientName: "workbuddy-test", ClientVersion: "1.2.3"})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	defer func() { _ = backend.Shutdown(context.Background()) }()

	spec := agent.Spec{
		Workdir:  t.TempDir(),
		Prompt:   "fix the bug",
		Model:    "gpt-5.4-mini",
		Sandbox:  "workspace-write",
		Approval: "never",
	}

	sess, err := backend.NewSession(t.Context(), spec)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = sess.Close() }()

	var events []agent.Event
	drained := make(chan struct{})
	go func() {
		for evt := range sess.Events() {
			events = append(events, evt)
		}
		close(drained)
	}()

	result, err := sess.Wait(t.Context())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	<-drained

	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.FinalMsg != "OK" {
		t.Fatalf("FinalMsg = %q, want %q", result.FinalMsg, "OK")
	}
	if result.SessionRef.ID != "thread-test" || result.SessionRef.Kind != "codex-thread" {
		t.Fatalf("SessionRef = %+v", result.SessionRef)
	}

	var kinds []string
	for _, evt := range events {
		kinds = append(kinds, evt.Kind)
	}
	wantKinds := []string{"turn.started", "agent.message", "agent.message", "token.usage", "turn.completed", "task.complete"}
	if len(kinds) != len(wantKinds) {
		t.Fatalf("event kind count = %d, want %d (%v)", len(kinds), len(wantKinds), kinds)
	}
	for i, want := range wantKinds {
		if kinds[i] != want {
			t.Fatalf("event[%d].Kind = %q, want %q", i, kinds[i], want)
		}
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake log: %v", err)
	}
	lines := splitNonEmptyLines(string(logData))
	// initialize, initialized, thread/start, turn/start = 4 messages
	// (exit varies; assert >= 4).
	if len(lines) < 4 {
		t.Fatalf("fake app-server log lines = %d, want >= 4", len(lines))
	}
	expectMethods := []string{"initialize", "initialized", "thread/start", "turn/start"}
	for i, want := range expectMethods {
		var obj map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &obj); err != nil {
			t.Fatalf("unmarshal log line %d %q: %v", i, lines[i], err)
		}
		got, _ := obj["method"].(string)
		if got != want {
			t.Fatalf("log[%d].method = %q, want %q", i, got, want)
		}
	}
}

func TestCodexSessionInterruptUsesTurnID(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "fake.log")
	srv := codextest.NewServer(t, codextest.Config{Mode: codextest.ModeInterrupt, LogPath: logPath})
	defer srv.Close()
	t.Setenv("WORKBUDDY_CODEX_URL", srv.URL)
	backend, err := NewBackend(Config{})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	defer func() { _ = backend.Shutdown(context.Background()) }()

	sess, err := backend.NewSession(t.Context(), agent.Spec{
		Workdir:  t.TempDir(),
		Prompt:   "wait for interrupt",
		Approval: "never",
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = sess.Close() }()

	interruptCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sess.Interrupt(interruptCtx); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	_, _ = sess.Wait(context.Background())

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake log: %v", err)
	}
	found := false
	for _, line := range splitNonEmptyLines(string(logData)) {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		if obj["method"] == "turn/interrupt" {
			params, _ := obj["params"].(map[string]any)
			if params["threadId"] != "thread-test" || params["turnId"] != "turn-test" {
				t.Fatalf("turn/interrupt params = %#v", params)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("turn/interrupt request not observed")
	}
}

func TestCodexProcessScopedNotificationFansOut(t *testing.T) {
	// Replaces the pre-REQ-127 stderr-capture test. With WS transport the
	// worker no longer sees codex stderr (the supervised sidecar's
	// journalctl owns it). The replacement signal is process-scoped
	// JSON-RPC notifications (configWarning, mcpServer/startupStatus)
	// which the new readLoop fans out to active sessions as log events
	// so operators still see them in the per-session artifacts.
	srv := codextest.NewServer(t, codextest.Config{Mode: codextest.ModeStderrAsLog})
	defer srv.Close()
	t.Setenv("WORKBUDDY_CODEX_URL", srv.URL)
	backend, err := NewBackend(Config{})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	defer func() { _ = backend.Shutdown(context.Background()) }()

	sess, err := backend.NewSession(t.Context(), agent.Spec{
		Workdir:  t.TempDir(),
		Prompt:   "show warnings",
		Approval: "never",
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = sess.Close() }()

	var sawCodexLog bool
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for evt := range sess.Events() {
			if evt.Kind != "log" {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal(evt.Body, &payload); err != nil {
				continue
			}
			if payload["stream"] == "codex" {
				sawCodexLog = true
			}
		}
	}()

	if _, err := sess.Wait(t.Context()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	<-drained

	if !sawCodexLog {
		t.Fatal("expected process-scoped log event from configWarning notification")
	}
}

func TestCodexConcurrentSessionsShareOneConnection(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "fake.log")
	srv := codextest.NewServer(t, codextest.Config{Mode: codextest.ModeComplete, LogPath: logPath})
	defer srv.Close()
	t.Setenv("WORKBUDDY_CODEX_URL", srv.URL)
	backend, err := NewBackend(Config{})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	defer func() { _ = backend.Shutdown(context.Background()) }()

	ctx := context.Background()
	sess1, err := backend.NewSession(ctx, agent.Spec{Workdir: t.TempDir(), Prompt: "first"})
	if err != nil {
		t.Fatalf("NewSession 1: %v", err)
	}
	sess2, err := backend.NewSession(ctx, agent.Spec{Workdir: t.TempDir(), Prompt: "second"})
	if err != nil {
		t.Fatalf("NewSession 2: %v", err)
	}

	for _, s := range []agent.Session{sess1, sess2} {
		s := s
		go func() {
			for range s.Events() {
			}
		}()
	}

	if _, err := sess1.Wait(ctx); err != nil {
		t.Fatalf("Wait 1: %v", err)
	}
	if _, err := sess2.Wait(ctx); err != nil {
		t.Fatalf("Wait 2: %v", err)
	}

	if sess1.ID() == sess2.ID() {
		t.Fatalf("expected distinct thread ids, got %q and %q", sess1.ID(), sess2.ID())
	}
	if sess1.ID() == "" || sess2.ID() == "" {
		t.Fatalf("empty thread ids: %q %q", sess1.ID(), sess2.ID())
	}

	// Exactly one initialize and two thread/start: shared connection,
	// multiplexed threads (the property the singleton model preserves).
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake log: %v", err)
	}
	initializeCount := 0
	threadStartCount := 0
	for _, line := range splitNonEmptyLines(string(data)) {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		if m, _ := obj["method"].(string); m == "initialize" {
			initializeCount++
		}
		if m, _ := obj["method"].(string); m == "thread/start" {
			threadStartCount++
		}
	}
	if initializeCount != 1 {
		t.Fatalf("initialize calls = %d, want 1 (one handshake for shared connection)", initializeCount)
	}
	if threadStartCount != 2 {
		t.Fatalf("thread/start calls = %d, want 2 (one per session)", threadStartCount)
	}

	_ = sess1.Close()
	_ = sess2.Close()
}

func TestCodexBackendInterfaceCompliance(t *testing.T) {
	var _ agent.Backend = (*Backend)(nil)
}

func TestCodexSessionInterfaceCompliance(t *testing.T) {
	var _ agent.Session = (*session)(nil)
}

// Pins the drop-on-full emit: a flood that overruns the events buffer
// without a consumer must still let Wait return, and must actually record
// drops — otherwise the test would silently stop exercising the regression.
func TestCodexSessionDoesNotDeadlockOnSlowConsumer(t *testing.T) {
	srv := codextest.NewServer(t, codextest.Config{Mode: codextest.ModeFlood})
	defer srv.Close()
	t.Setenv("WORKBUDDY_CODEX_URL", srv.URL)
	backend, err := NewBackend(Config{})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	defer func() { _ = backend.Shutdown(context.Background()) }()

	spec := agent.Spec{
		Workdir:  t.TempDir(),
		Prompt:   "flood me",
		Sandbox:  "workspace-write",
		Approval: "never",
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	sess, err := backend.NewSession(ctx, spec)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = sess.Close() }()

	result, err := sess.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	dropped := atomic.LoadInt64(&sess.(*session).droppedEvents)
	if dropped == 0 {
		t.Fatalf("expected events to be dropped under flood; got 0 — flood may no longer overflow the buffer")
	}
}

func splitNonEmptyLines(data string) []string {
	var out []string
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] != '\n' {
			continue
		}
		if i > start {
			out = append(out, data[start:i])
		}
		start = i + 1
	}
	if start < len(data) {
		out = append(out, data[start:])
	}
	return out
}
