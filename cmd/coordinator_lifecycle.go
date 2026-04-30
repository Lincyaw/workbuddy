package cmd

import (
	"os"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
)

// emitCoordinatorStarted records a coordinator_started event at the tail of
// startup so operator hooks (and the audit trail) can detect process
// restarts. Called from both `workbuddy serve` (after coordinator/worker are
// up) and `workbuddy coordinator` (after the HTTP listener is reserved).
func emitCoordinatorStarted(evlog *eventlog.EventLogger, mode, listenAddr string) {
	if evlog == nil {
		return
	}
	evlog.Log(eventlog.TypeCoordinatorStarted, "", 0, map[string]interface{}{
		"mode":        mode,
		"listen":      listenAddr,
		"pid":         os.Getpid(),
		"version":     versionString(),
	})
}

// emitCoordinatorStopping records a coordinator_stopping event at the head of
// the graceful-shutdown path so hooks can react before workers/pollers drain.
func emitCoordinatorStopping(evlog *eventlog.EventLogger, mode, listenAddr string) {
	if evlog == nil {
		return
	}
	evlog.Log(eventlog.TypeCoordinatorStopping, "", 0, map[string]interface{}{
		"mode":   mode,
		"listen": listenAddr,
		"pid":    os.Getpid(),
	})
}
