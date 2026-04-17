package events

const KindTaskComplete EventKind = "task.complete"

type TaskCompletePayload struct {
	TurnID string `json:"turn_id"`
	Status string `json:"status"`
}
