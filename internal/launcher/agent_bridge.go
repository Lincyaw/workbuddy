package launcher

import (
	"github.com/Lincyaw/workbuddy/internal/agent"
	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
)

func newBackendFromConfig(runtimeName string) (agent.Backend, error) {
	return runtimepkg.NewBackendFromConfig(runtimeName)
}

type agentBridgeRuntime = runtimepkg.AgentBridgeRuntime
type agentBridgeSession = runtimepkg.AgentBridgeSession
type bridgeSessionHandle = runtimepkg.BridgeSessionHandle

func newAgentBridgeRuntime(runtimeName string, factory func() (agent.Backend, error)) *agentBridgeRuntime {
	return runtimepkg.NewAgentBridgeRuntime(runtimeName, factory)
}

func translateAgentEventKind(kind string) launcherevents.EventKind {
	return runtimepkg.TranslateAgentEventKind(kind)
}

func newAgentBridgeSession(sess agent.Session, handle bridgeSessionHandle, agentCfg *config.AgentConfig, task *TaskContext) *agentBridgeSession {
	return &runtimepkg.AgentBridgeSession{Session: sess, Handle: handle, AgentCfg: agentCfg, Task: task}
}
