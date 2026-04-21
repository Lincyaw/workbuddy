package session

import (
	"encoding/json"
	"fmt"

	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
)

// Stream writes Event Schema v1 events to the session handle's canonical
// events artifact and returns the artifact path plus a drain waiter.
func Stream(taskCtx *runtimepkg.TaskContext, eventsCh <-chan launcherevents.Event) (string, func() error) {
	handle := taskCtx.SessionHandle()
	if handle == nil {
		return "", func() error { return nil }
	}
	path := handle.EventsPath()
	errCh := make(chan error, 1)
	go func() {
		var streamErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				streamErr = fmt.Errorf("session stream panic: %v", recovered)
			}
			errCh <- streamErr
		}()
		for evt := range eventsCh {
			if streamErr != nil {
				continue
			}
			data, err := json.Marshal(evt)
			if err != nil {
				streamErr = err
				continue
			}
			if err := handle.WriteEvent(append(data, '\n')); err != nil {
				streamErr = err
			}
		}
	}()
	return path, func() error { return <-errCh }
}
