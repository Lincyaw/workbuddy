package launcher

import (
	"os"
	"path/filepath"
	"testing"
)

func installFakeCodex(t *testing.T) func() {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "codex")
	script := `#!/usr/bin/env python3
import json
import os
import sys

final_text = "OK"
log_path = os.environ.get("FAKE_CODEX_LOG", "")

def log(obj):
    if not log_path:
        return
    with open(log_path, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(obj) + "\n")

log({"argv": sys.argv[1:]})

for line in sys.stdin:
    msg = json.loads(line)
    method = msg.get("method")
    log({"method": method, "params": msg.get("params")})
    if method == "initialize":
        print(json.dumps({"id": msg["id"], "result": {"userAgent": "fake", "codexHome": "/tmp", "platformFamily": "unix", "platformOs": "linux"}}), flush=True)
    elif method == "initialized":
        continue
    elif method == "thread/start":
        print(json.dumps({"id": msg["id"], "result": {"thread": {"id": "thread-test"}, "model": "gpt-5.4-mini", "modelProvider": "openai", "cwd": msg.get("params", {}).get("cwd", ""), "approvalPolicy": msg.get("params", {}).get("approvalPolicy", "never"), "approvalsReviewer": "user", "sandbox": {"type": "workspaceWrite"}}}), flush=True)
    elif method == "turn/start":
        prompt = ""
        for item in msg.get("params", {}).get("input", []):
            if item.get("type") == "text":
                prompt = item.get("text", "")
                break
        if "HELLO" in prompt:
            final_text = "HELLO"
        elif "PONG" in prompt:
            final_text = "PONG"
        print(json.dumps({"id": msg["id"], "result": {"turn": {"id": "turn-test", "items": [], "status": "inProgress"}}}), flush=True)
        print(json.dumps({"method": "turn/started", "params": {"threadId": "thread-test", "turn": {"id": "turn-test", "items": [], "status": "inProgress"}}}), flush=True)
        print(json.dumps({"method": "item/started", "params": {"threadId": "thread-test", "turnId": "turn-test", "item": {"type": "agentMessage", "id": "msg-1", "text": "", "phase": "final_answer"}}}), flush=True)
        print(json.dumps({"method": "item/agentMessage/delta", "params": {"threadId": "thread-test", "turnId": "turn-test", "itemId": "msg-1", "delta": final_text}}), flush=True)
        print(json.dumps({"method": "item/completed", "params": {"threadId": "thread-test", "turnId": "turn-test", "item": {"type": "agentMessage", "id": "msg-1", "text": final_text, "phase": "final_answer"}}}), flush=True)
        print(json.dumps({"method": "turn/completed", "params": {"threadId": "thread-test", "turn": {"id": "turn-test", "items": [], "status": "completed", "durationMs": 15}}}), flush=True)
        sys.exit(0)
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex binary: %v", err)
	}

	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath); err != nil {
		t.Fatalf("set PATH: %v", err)
	}
	return func() {
		_ = os.Setenv("PATH", oldPath)
	}
}
