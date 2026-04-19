package codex

import (
	"encoding/json"
	"testing"
)

func TestMapCodexEvent(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		params   string
		wantKind string
	}{
		{
			name:     "turn started",
			method:   "codex/event/turn_started",
			params:   `{"thread_id":"t1"}`,
			wantKind: "turn.started",
		},
		{
			name:     "agent message",
			method:   "codex/event/agent_message",
			params:   `{"text":"hello"}`,
			wantKind: "agent.message",
		},
		{
			name:     "tool call",
			method:   "codex/event/tool_call",
			params:   `{"name":"bash"}`,
			wantKind: "tool.call",
		},
		{
			name:     "tool result",
			method:   "codex/event/tool_result",
			params:   `{"output":"ok"}`,
			wantKind: "tool.result",
		},
		{
			name:     "turn completed",
			method:   "codex/event/turn_completed",
			params:   `{"status":"done"}`,
			wantKind: "turn.completed",
		},
		{
			name:     "error",
			method:   "codex/event/error",
			params:   `{"message":"oops"}`,
			wantKind: "error",
		},
		{
			name:     "reasoning",
			method:   "codex/event/reasoning",
			params:   `{"text":"thinking..."}`,
			wantKind: "reasoning",
		},
		{
			name:     "command exec",
			method:   "codex/event/command_exec",
			params:   `{"command":"ls"}`,
			wantKind: "command.exec",
		},
		{
			name:     "command output",
			method:   "codex/event/command_output",
			params:   `{"output":"file.txt"}`,
			wantKind: "command.output",
		},
		{
			name:     "file change",
			method:   "codex/event/file_change",
			params:   `{"path":"main.go"}`,
			wantKind: "file.change",
		},
		{
			name:     "token usage",
			method:   "codex/event/token_usage",
			params:   `{"input":100,"output":50}`,
			wantKind: "token.usage",
		},
		{
			name:     "unknown method",
			method:   "codex/event/unknown",
			params:   `{}`,
			wantKind: "log",
		},
		{
			name:     "non-codex method",
			method:   "something/else",
			params:   `{}`,
			wantKind: "log",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evt := mapNotification(tt.method, json.RawMessage(tt.params))
			if evt.Kind != tt.wantKind {
				t.Fatalf("mapNotification(%q).Kind = %q, want %q", tt.method, evt.Kind, tt.wantKind)
			}
			if !json.Valid(evt.Body) {
				t.Fatalf("mapNotification(%q).Body is not valid JSON: %s", tt.method, evt.Body)
			}
		})
	}
}

func TestMapNotificationEmptyParams(t *testing.T) {
	evt := mapNotification("codex/event/turn_started", nil)
	if evt.Kind != "turn.started" {
		t.Fatalf("Kind = %q, want %q", evt.Kind, "turn.started")
	}
	// Empty params should default to "{}".
	if string(evt.Body) != "{}" {
		t.Fatalf("Body = %s, want {}", evt.Body)
	}
}

func TestNotificationKind(t *testing.T) {
	// Test the internal notificationKind function directly.
	tests := []struct {
		method string
		want   string
	}{
		{"codex/event/turn_started", "turn.started"},
		{"codex/event/agent_message", "agent.message"},
		{"codex/event/tool_call", "tool.call"},
		{"codex/event/tool_result", "tool.result"},
		{"codex/event/turn_completed", "turn.completed"},
		{"codex/event/error", "error"},
		{"codex/event/reasoning", "reasoning"},
		{"codex/event/command_exec", "command.exec"},
		{"codex/event/command_output", "command.output"},
		{"codex/event/file_change", "file.change"},
		{"codex/event/token_usage", "token.usage"},
		{"", "log"},
		{"something/other", "log"},
	}

	for _, tt := range tests {
		got := notificationKind(tt.method)
		if got != tt.want {
			t.Fatalf("notificationKind(%q) = %q, want %q", tt.method, got, tt.want)
		}
	}
}
