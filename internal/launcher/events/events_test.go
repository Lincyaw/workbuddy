package events

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestEventJSONRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		kind    EventKind
		payload any
	}{
		{name: "turn started", kind: KindTurnStarted, payload: TurnStartedPayload{TurnID: "turn-1"}},
		{name: "turn completed", kind: KindTurnCompleted, payload: TurnCompletedPayload{TurnID: "turn-1", Status: "ok"}},
		{name: "agent message", kind: KindAgentMessage, payload: AgentMessagePayload{Text: "hello", Delta: false, Final: true}},
		{name: "reasoning", kind: KindReasoning, payload: ReasoningPayload{Text: "thinking", Delta: true}},
		{name: "tool call", kind: KindToolCall, payload: ToolCallPayload{Name: "search", CallID: "call-1", Args: json.RawMessage(`{"q":"ping"}`)}},
		{name: "tool result", kind: KindToolResult, payload: ToolResultPayload{CallID: "call-1", OK: true, Result: json.RawMessage(`{"ok":true}`)}},
		{name: "command exec", kind: KindCommandExec, payload: CommandExecPayload{Cmd: []string{"echo", "PONG"}, CWD: "/tmp", CallID: "call-2"}},
		{name: "command output", kind: KindCommandOutput, payload: CommandOutputPayload{CallID: "call-2", Stream: "stdout", Data: "PONG\n"}},
		{name: "file change", kind: KindFileChange, payload: FileChangePayload{Path: "main.go", ChangeKind: "modify", Diff: "--- a/main.go\n+++ b/main.go\n"}},
		{name: "token usage", kind: KindTokenUsage, payload: TokenUsagePayload{Input: 10, Output: 4, Cached: 2, Total: 14}},
		{name: "error", kind: KindError, payload: ErrorPayload{Code: "boom", Message: "failed", Recoverable: false}},
		{name: "log", kind: KindLog, payload: LogPayload{Stream: "stderr", Line: "raw line"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evt := Event{
				Kind:      tc.kind,
				Timestamp: time.Unix(1710000000, 0).UTC(),
				SessionID: "session-123",
				TurnID:    "turn-1",
				Seq:       7,
				Payload:   MustPayload(tc.payload),
				Raw:       json.RawMessage(`{"type":"native"}`),
			}
			data, err := json.Marshal(evt)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded Event
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if decoded.Kind != evt.Kind || decoded.SessionID != evt.SessionID || decoded.TurnID != evt.TurnID || decoded.Seq != evt.Seq {
				t.Fatalf("decoded mismatch: %+v", decoded)
			}

			dst := reflect.New(reflect.TypeOf(tc.payload))
			if err := json.Unmarshal(decoded.Payload, dst.Interface()); err != nil {
				t.Fatalf("payload unmarshal: %v", err)
			}
			if !reflect.DeepEqual(tc.payload, dst.Elem().Interface()) {
				t.Fatalf("payload mismatch: got %#v want %#v", dst.Elem().Interface(), tc.payload)
			}
		})
	}
}
