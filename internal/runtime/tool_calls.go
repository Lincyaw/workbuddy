package runtime

import (
	"encoding/json"
	"fmt"

	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

func PersistToolCallEvent(handle *ManagedSession, runtimeName string, evt launcherevents.Event) error {
	if handle == nil {
		return nil
	}
	switch evt.Kind {
	case launcherevents.KindToolCall,
		launcherevents.KindToolResult,
		launcherevents.KindCommandExec,
		launcherevents.KindCommandOutput,
		launcherevents.KindFileChange:
	default:
		return nil
	}
	record := map[string]any{
		"runtime": runtimeName,
		"kind":    evt.Kind,
		"seq":     evt.Seq,
		"turn_id": evt.TurnID,
		"payload": json.RawMessage(evt.Payload),
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("runtime: session manager: marshal tool call: %w", err)
	}
	return handle.WriteToolCall(append(data, '\n'))
}
