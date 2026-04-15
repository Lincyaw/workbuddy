package events

type ReasoningPayload struct {
	Text  string `json:"text"`
	Delta bool   `json:"delta"`
}
