package events

const KindCommandExec EventKind = "command.exec"

type CommandExecPayload struct {
	Cmd    []string `json:"cmd"`
	CWD    string   `json:"cwd,omitempty"`
	CallID string   `json:"call_id"`
}
