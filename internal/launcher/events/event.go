package events

import (
	"encoding/json"
	"time"
)

type EventKind string

const (
	KindTurnStarted   EventKind = "turn.started"
	KindTurnCompleted EventKind = "turn.completed"
	KindAgentMessage  EventKind = "agent.message"
	KindReasoning     EventKind = "reasoning"
	KindToolCall      EventKind = "tool.call"
	KindToolResult    EventKind = "tool.result"
	KindCommandExec   EventKind = "command.exec"
	KindCommandOutput EventKind = "command.output"
	KindFileChange    EventKind = "file.change"
	KindTokenUsage    EventKind = "token.usage"
	KindError         EventKind = "error"
	KindLog           EventKind = "log"
)

type Event struct {
	Kind      EventKind       `json:"kind"`
	Timestamp time.Time       `json:"ts"`
	SessionID string          `json:"session_id"`
	TurnID    string          `json:"turn_id,omitempty"`
	Seq       uint64          `json:"seq"`
	Payload   json.RawMessage `json:"payload"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

func MustPayload(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
