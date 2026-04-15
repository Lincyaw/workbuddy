package events

import "encoding/json"

const KindToolCall EventKind = "tool.call"

type ToolCallPayload struct {
	Name   string          `json:"name"`
	CallID string          `json:"call_id"`
	Args   json.RawMessage `json:"args,omitempty"`
}
