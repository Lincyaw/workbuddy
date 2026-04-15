package events

const KindTurnStarted EventKind = "turn.started"

type TurnStartedPayload struct {
	TurnID string `json:"turn_id"`
}
