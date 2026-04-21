package launcher

import (
	"encoding/json"
	"fmt"

	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/store"
)

func NewSessionManager(baseDir string, st *store.Store) *SessionManager {
	return runtimepkg.NewSessionManager(baseDir, st)
}

func persistToolCallEvent(handle *ManagedSession, runtime string, evt launcherevents.Event) error {
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
		"runtime": runtime,
		"kind":    evt.Kind,
		"seq":     evt.Seq,
		"turn_id": evt.TurnID,
		"payload": json.RawMessage(evt.Payload),
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("launcher: session manager: marshal tool call: %w", err)
	}
	return handle.WriteToolCall(append(data, '\n'))
}
