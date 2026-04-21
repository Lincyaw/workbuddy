package launcher

import (
	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
)

func buildScopedEnv(agent *config.AgentConfig, task *TaskContext) []string {
	return runtimepkg.BuildScopedEnv(agent, task)
}

func effectivePermissionsPayload(agent *config.AgentConfig) launcherevents.PermissionPayload {
	return runtimepkg.EffectivePermissionsPayload(agent)
}

func emitPermissionEvent(events chan<- launcherevents.Event, seq *uint64, sessionID, turnID string, agent *config.AgentConfig) {
	runtimepkg.EmitPermissionEvent(events, seq, sessionID, turnID, agent, runtimepkg.EmitEvent)
}
