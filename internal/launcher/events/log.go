package events

type LogPayload struct {
	Stream string `json:"stream"`
	Line   string `json:"line"`
}
