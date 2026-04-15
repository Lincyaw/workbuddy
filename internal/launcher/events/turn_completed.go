package events

const KindTurnCompleted EventKind = "turn.completed"

type TurnCompletedPayload struct {
	TurnID string `json:"turn_id"`
	Status string `json:"status"`
}
