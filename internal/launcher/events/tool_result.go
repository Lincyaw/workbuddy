package events

import "encoding/json"

const KindToolResult EventKind = "tool.result"

type ToolResultPayload struct {
	CallID string          `json:"call_id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
}
