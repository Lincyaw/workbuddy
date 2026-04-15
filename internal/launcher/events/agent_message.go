package events

const KindAgentMessage EventKind = "agent.message"

type AgentMessagePayload struct {
	Text  string `json:"text"`
	Delta bool   `json:"delta"`
	Final bool   `json:"final"`
}
