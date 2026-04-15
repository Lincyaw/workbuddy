package events

type TurnCompletedPayload struct {
	TurnID string `json:"turn_id"`
	Status string `json:"status"`
}
