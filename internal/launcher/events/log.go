package events

const KindLog EventKind = "log"

type LogPayload struct {
	Stream string `json:"stream"`
	Line   string `json:"line"`
}
