package events

import (
	"encoding/json"
	"time"
)

type EventKind string

type Event struct {
	Kind      EventKind       `json:"kind"`
	Timestamp time.Time       `json:"ts"`
	SessionID string          `json:"session_id"`
	TurnID    string          `json:"turn_id,omitempty"`
	Seq       uint64          `json:"seq"`
	Payload   json.RawMessage `json:"payload"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

func EncodePayload(v any) (json.RawMessage, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func MustPayload(v any) json.RawMessage {
	data, err := EncodePayload(v)
	if err != nil {
		panic(err)
	}
	return data
}
