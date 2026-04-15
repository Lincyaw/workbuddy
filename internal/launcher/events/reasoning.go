package events

const KindReasoning EventKind = "reasoning"

type ReasoningPayload struct {
	Text  string `json:"text"`
	Delta bool   `json:"delta"`
}
