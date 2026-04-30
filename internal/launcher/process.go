package launcher

import (
	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
)

func hasPrintFlag(args []string) bool {
	return runtimepkg.HasPrintFlag(args)
}

func hasVerboseFlag(args []string) bool {
	return runtimepkg.HasVerboseFlag(args)
}

func claudePolicyArgs(policy config.PolicyConfig) []string {
	return runtimepkg.ClaudePolicyArgs(policy)
}

func isClaudeRuntime(runtimeName string) bool {
	return runtimepkg.IsClaudeRuntime(runtimeName)
}

func emitEvent(ch chan<- launcherevents.Event, seq *uint64, sessionID, turnID string, kind launcherevents.EventKind, payload any, raw []byte) {
	runtimepkg.EmitEvent(ch, seq, sessionID, turnID, kind, payload, raw)
}
