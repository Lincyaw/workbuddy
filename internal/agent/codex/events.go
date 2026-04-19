package codex

import (
	"encoding/json"

	"github.com/Lincyaw/workbuddy/internal/agent"
)

// mapNotification converts a codex/event/* JSON-RPC notification into an agent.Event.
func mapNotification(method string, params json.RawMessage) agent.Event {
	kind := notificationKind(method)
	body := params
	if len(body) == 0 {
		body = json.RawMessage("{}")
	}
	return agent.Event{Kind: kind, Body: body}
}

func notificationKind(method string) string {
	switch method {
	case "codex/event/turn_started":
		return "turn.started"
	case "codex/event/agent_message":
		return "agent.message"
	case "codex/event/tool_call":
		return "tool.call"
	case "codex/event/tool_result":
		return "tool.result"
	case "codex/event/turn_completed":
		return "turn.completed"
	case "codex/event/error":
		return "error"
	case "codex/event/reasoning":
		return "reasoning"
	case "codex/event/command_exec":
		return "command.exec"
	case "codex/event/command_output":
		return "command.output"
	case "codex/event/file_change":
		return "file.change"
	case "codex/event/token_usage":
		return "token.usage"
	default:
		return "log"
	}
}
