package events

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEventJSONRoundTrip(t *testing.T) {
	evt := Event{
		Kind:      KindAgentMessage,
		Timestamp: time.Unix(1710000000, 0).UTC(),
		SessionID: "session-123",
		TurnID:    "turn-1",
		Seq:       7,
		Payload:   MustPayload(AgentMessagePayload{Text: "hello", Delta: false, Final: true}),
		Raw:       json.RawMessage(`{"type":"item.completed"}`),
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
	var payload AgentMessagePayload
	if err := json.Unmarshal(decoded.Payload, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if payload.Text != "hello" || !payload.Final || payload.Delta {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}
