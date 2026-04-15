package events

type CommandOutputPayload struct {
	CallID string `json:"call_id"`
	Stream string `json:"stream"`
	Data   string `json:"data"`
}
