package launcher

import (
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/store"
)

func NewSessionManager(baseDir string, st *store.Store) *SessionManager {
	return runtimepkg.NewSessionManager(baseDir, st)
}

func persistToolCallEvent(handle *ManagedSession, runtime string, evt launcherevents.Event) error {
	return runtimepkg.PersistToolCallEvent(handle, runtime, evt)
}
