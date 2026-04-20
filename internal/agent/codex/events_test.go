package codex

import (
	"encoding/json"
	"testing"
)

func TestMapNotificationKinds(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		params   string
		wantKind string
	}{
		{
			name:     "turn started",
			method:   "turn/started",
			params:   `{"threadId":"t1","turn":{"id":"turn-1","items":[],"status":"inProgress"}}`,
			wantKind: "turn.started",
		},
		{
			name:     "agent delta",
			method:   "item/agentMessage/delta",
			params:   `{"threadId":"t1","turnId":"turn-1","itemId":"msg-1","delta":"hello"}`,
			wantKind: "agent.message",
		},
		{
			name:     "command output",
			method:   "item/commandExecution/outputDelta",
			params:   `{"threadId":"t1","turnId":"turn-1","itemId":"cmd-1","delta":"ls\n"}`,
			wantKind: "command.output",
		},
		{
			name:     "turn completed",
			method:   "turn/completed",
			params:   `{"threadId":"t1","turn":{"id":"turn-1","items":[],"status":"completed"}}`,
			wantKind: "turn.completed",
		},
		{
			name:     "error",
			method:   "error",
			params:   `{"threadId":"t1","turnId":"turn-1","willRetry":false,"error":{"message":"oops","codexErrorInfo":"badRequest"}}`,
			wantKind: "error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := mapNotification(tt.method, json.RawMessage(tt.params), json.RawMessage(`{"method":"`+tt.method+`"}`))
			if len(events) == 0 {
				t.Fatalf("mapNotification(%q) returned no events", tt.method)
			}
			if events[0].Kind != tt.wantKind {
				t.Fatalf("mapNotification(%q).Kind = %q, want %q", tt.method, events[0].Kind, tt.wantKind)
			}
			if !json.Valid(events[0].Body) {
				t.Fatalf("event body is not valid JSON: %s", events[0].Body)
			}
			if !json.Valid(events[0].Raw) {
				t.Fatalf("event raw is not valid JSON: %s", events[0].Raw)
			}
		})
	}
}

func TestMapNotificationTurnCompletedSynthesizesTaskComplete(t *testing.T) {
	events := mapNotification(
		"turn/completed",
		json.RawMessage(`{"threadId":"t1","turn":{"id":"turn-1","items":[],"status":"completed"}}`),
		json.RawMessage(`{"method":"turn/completed"}`),
	)
	if len(events) != 2 {
		t.Fatalf("turn/completed emitted %d events, want 2", len(events))
	}
	if events[0].Kind != "turn.completed" || events[1].Kind != "task.complete" {
		t.Fatalf("turn/completed events = %q, %q", events[0].Kind, events[1].Kind)
	}
}

func TestExtractThreadID(t *testing.T) {
	tests := []struct {
		name   string
		params string
		want   string
	}{
		{name: "camelCase", params: `{"threadId":"abc-123"}`, want: "abc-123"},
		{name: "nested thread", params: `{"thread":{"id":"nested-1"}}`, want: "nested-1"},
		{name: "conversation fallback", params: `{"conversationId":"conv-1"}`, want: "conv-1"},
		{name: "invalid json", params: `not json`, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractThreadID(json.RawMessage(tt.params))
			if got != tt.want {
				t.Fatalf("extractThreadID(%s) = %q, want %q", tt.params, got, tt.want)
			}
		})
	}
}
