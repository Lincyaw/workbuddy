package events

import "encoding/json"

type ToolCallPayload struct {
	Name   string          `json:"name"`
	CallID string          `json:"call_id"`
	Args   json.RawMessage `json:"args,omitempty"`
}
