package events

import "encoding/json"

type ToolResultPayload struct {
	CallID string          `json:"call_id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
}
