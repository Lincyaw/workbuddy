package events

type TokenUsagePayload struct {
	Input  int `json:"input"`
	Output int `json:"output"`
	Cached int `json:"cached"`
	Total  int `json:"total"`
}
