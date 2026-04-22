package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/agent"
)

func TestCodexNewBackendMissingBinary(t *testing.T) {
	cfg := Config{Binary: "/nonexistent/codex-binary-that-does-not-exist"}
	_, err := NewBackend(cfg)
	if err == nil {
		t.Fatal("NewBackend with missing binary should return error")
	}
}

func TestCodexSessionLifecycleViaAppServer(t *testing.T) {
	bin, logPath := writeFakeCodexBinary(t, "complete")
	backend, err := NewBackend(Config{Binary: bin, ClientName: "workbuddy-test", ClientVersion: "1.2.3"})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	spec := agent.Spec{
		Workdir:  t.TempDir(),
		Prompt:   "fix the bug",
		Model:    "gpt-5.4-mini",
		Sandbox:  "workspace-write",
		Approval: "never",
		Env: map[string]string{
			"FAKE_CODEX_LOG": logPath,
			"WB_TEST_ENV":    "scoped-token",
		},
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
	var lines []map[string]any
	for _, line := range splitNonEmptyLines(string(logData)) {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		lines = append(lines, obj)
	}
	if len(lines) < 5 {
		t.Fatalf("fake app-server log lines = %d, want >= 5", len(lines))
	}
	if env, _ := lines[0]["env"].(string); env != "scoped-token" {
		t.Fatalf("fake app-server env = %q, want %q", env, "scoped-token")
	}
	if method, _ := lines[2]["method"].(string); method != "initialize" {
		t.Fatalf("first request method = %q, want initialize", method)
	}
	if method, _ := lines[3]["method"].(string); method != "initialized" {
		t.Fatalf("second client message = %q, want initialized", method)
	}
	if method, _ := lines[4]["method"].(string); method != "thread/start" {
		t.Fatalf("third request method = %q, want thread/start", method)
	}
	if method, _ := lines[5]["method"].(string); method != "turn/start" {
		t.Fatalf("fourth request method = %q, want turn/start", method)
	}
}

func TestCodexDangerouslyBypassSandboxUsesCLIFlag(t *testing.T) {
	bin, logPath := writeFakeCodexBinary(t, "complete")
	backend, err := NewBackend(Config{Binary: bin})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	sess, err := backend.NewSession(t.Context(), agent.Spec{
		Workdir:  t.TempDir(),
		Prompt:   "fix the bug",
		Sandbox:  "danger-full-access",
		Approval: "never",
		Env: map[string]string{
			"FAKE_CODEX_LOG": logPath,
		},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if _, err := sess.Wait(t.Context()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake log: %v", err)
	}
	var (
		argv        []any
		threadStart map[string]any
	)
	for _, line := range splitNonEmptyLines(string(logData)) {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		if rawArgv, ok := obj["argv"].([]any); ok {
			argv = rawArgv
		}
		if obj["method"] == "thread/start" {
			threadStart, _ = obj["params"].(map[string]any)
		}
	}
	if len(argv) == 0 {
		t.Fatal("missing argv log entry")
	}
	foundBypass := false
	for _, arg := range argv {
		if s, _ := arg.(string); s == "--dangerously-bypass-approvals-and-sandbox" {
			foundBypass = true
			break
		}
	}
	if !foundBypass {
		t.Fatalf("argv missing bypass flag: %#v", argv)
	}
	if got, _ := argv[0].(string); got != "--dangerously-bypass-approvals-and-sandbox" {
		t.Fatalf("argv[0] = %q, want top-level bypass flag; argv=%#v", got, argv)
	}
	if got, _ := argv[1].(string); got != "app-server" {
		t.Fatalf("argv[1] = %q, want app-server; argv=%#v", got, argv)
	}
	if threadStart == nil {
		t.Fatal("missing thread/start request")
	}
	if got, _ := threadStart["sandbox"].(string); got != "danger-full-access" {
		t.Fatalf("thread/start sandbox = %q, want danger-full-access; params=%#v", got, threadStart)
	}
}

func TestCodexSessionInterruptUsesTurnID(t *testing.T) {
	bin, logPath := writeFakeCodexBinary(t, "interrupt")
	backend, err := NewBackend(Config{Binary: bin})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	sess, err := backend.NewSession(t.Context(), agent.Spec{
		Workdir:  t.TempDir(),
		Prompt:   "wait for interrupt",
		Approval: "never",
		Env: map[string]string{
			"FAKE_CODEX_LOG": logPath,
		},
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

func TestCodexSessionCapturesStderrAsLogEvent(t *testing.T) {
	bin, logPath := writeFakeCodexBinary(t, "stderr")
	backend, err := NewBackend(Config{Binary: bin})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	sess, err := backend.NewSession(t.Context(), agent.Spec{
		Workdir:  t.TempDir(),
		Prompt:   "show stderr",
		Approval: "never",
		Env: map[string]string{
			"FAKE_CODEX_LOG": logPath,
		},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = sess.Close() }()

	var sawStderr bool
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
			if payload["stream"] == "stderr" && payload["line"] == "rpc warning on stderr" {
				sawStderr = true
			}
		}
	}()

	if _, err := sess.Wait(t.Context()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	<-drained

	if !sawStderr {
		t.Fatal("expected stderr log event")
	}
}

func TestCodexBackendInterfaceCompliance(t *testing.T) {
	var _ agent.Backend = (*Backend)(nil)
}

func TestCodexSessionInterfaceCompliance(t *testing.T) {
	var _ agent.Session = (*session)(nil)
}

func writeFakeCodexBinary(t *testing.T, mode string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "fake-codex.log")
	scriptPath := filepath.Join(dir, "codex")
	script := `#!/usr/bin/env python3
import json
import os
import sys

mode = os.environ.get("FAKE_CODEX_MODE", "` + mode + `")
log_path = os.environ.get("FAKE_CODEX_LOG", "")

def log(obj):
    if not log_path:
        return
    with open(log_path, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(obj) + "\n")

log({"env": os.environ.get("WB_TEST_ENV", "")})
log({"argv": sys.argv[1:]})

for line in sys.stdin:
    msg = json.loads(line)
    log(msg)
    method = msg.get("method")
    if method == "initialize":
        print(json.dumps({"id": msg["id"], "result": {"userAgent": "fake", "codexHome": "/tmp", "platformFamily": "unix", "platformOs": "linux"}}), flush=True)
    elif method == "initialized":
        continue
    elif method == "thread/start":
        print(json.dumps({"id": msg["id"], "result": {"thread": {"id": "thread-test"}, "model": "gpt-5.4-mini", "modelProvider": "openai", "cwd": msg.get("params", {}).get("cwd", ""), "approvalPolicy": msg.get("params", {}).get("approvalPolicy", "never"), "approvalsReviewer": "user", "sandbox": {"type": "workspaceWrite"}}}), flush=True)
    elif method == "turn/start":
        print(json.dumps({"id": msg["id"], "result": {"turn": {"id": "turn-test", "items": [], "status": "inProgress"}}}), flush=True)
        print(json.dumps({"method": "turn/started", "params": {"threadId": "thread-test", "turn": {"id": "turn-test", "items": [], "status": "inProgress"}}}), flush=True)
        if mode == "stderr":
            print("rpc warning on stderr", file=sys.stderr, flush=True)
        if mode == "complete" or mode == "stderr":
            print(json.dumps({"method": "item/started", "params": {"threadId": "thread-test", "turnId": "turn-test", "item": {"type": "agentMessage", "id": "msg-1", "text": "", "phase": "final_answer"}}}), flush=True)
            print(json.dumps({"method": "item/agentMessage/delta", "params": {"threadId": "thread-test", "turnId": "turn-test", "itemId": "msg-1", "delta": "OK"}}), flush=True)
            print(json.dumps({"method": "item/completed", "params": {"threadId": "thread-test", "turnId": "turn-test", "item": {"type": "agentMessage", "id": "msg-1", "text": "OK", "phase": "final_answer"}}}), flush=True)
            print(json.dumps({"method": "thread/tokenUsage/updated", "params": {"threadId": "thread-test", "turnId": "turn-test", "tokenUsage": {"total": {"inputTokens": 10, "outputTokens": 2, "cachedInputTokens": 1, "totalTokens": 12, "reasoningOutputTokens": 0}, "last": {"inputTokens": 10, "outputTokens": 2, "cachedInputTokens": 1, "totalTokens": 12, "reasoningOutputTokens": 0}}}}), flush=True)
            print(json.dumps({"method": "turn/completed", "params": {"threadId": "thread-test", "turn": {"id": "turn-test", "items": [], "status": "completed", "durationMs": 15}}}), flush=True)
        else:
            continue
    elif method == "turn/interrupt":
        print(json.dumps({"id": msg["id"], "result": {}}), flush=True)
        print(json.dumps({"method": "turn/completed", "params": {"threadId": "thread-test", "turn": {"id": "turn-test", "items": [], "status": "interrupted", "durationMs": 7}}}), flush=True)
        sys.exit(0)
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex binary: %v", err)
	}
	return scriptPath, logPath
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
