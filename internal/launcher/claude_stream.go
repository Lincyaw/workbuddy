package launcher

import (
	"encoding/json"

	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
)

type claudeStreamEventMapper = runtimepkg.ClaudeStreamEventMapper

func newClaudeStreamEventMapper(sessionID string) *claudeStreamEventMapper {
	return runtimepkg.NewClaudeStreamEventMapper(sessionID)
}

func claudeTokenUsage(raw map[string]json.RawMessage) *launcherevents.TokenUsagePayload {
	return runtimepkg.ClaudeTokenUsage(raw)
}

func claudeCommandExecPayload(name, callID string, rawArgs json.RawMessage) *launcherevents.CommandExecPayload {
	return runtimepkg.ClaudeCommandExecPayload(name, callID, rawArgs)
}

func claudeFileChanges(name string, rawArgs json.RawMessage) []launcherevents.FileChangePayload {
	return runtimepkg.ClaudeFileChanges(name, rawArgs)
}

func claudeToolResultText(raw json.RawMessage) string {
	return runtimepkg.ClaudeToolResultText(raw)
}
