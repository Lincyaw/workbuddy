package launcher

import (
	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
)

func validateOutputContract(agent *config.AgentConfig, result *Result) error {
	return runtimepkg.ValidateOutputContract(agent, result)
}

func emitOutputContractFailure(events chan<- launcherevents.Event, seq *uint64, sessionID, turnID string, err error) {
	runtimepkg.EmitOutputContractFailure(events, seq, sessionID, turnID, err, runtimepkg.EmitEvent)
}
