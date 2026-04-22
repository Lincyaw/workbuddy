package session

import (
	"encoding/json"
	"fmt"

	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
)

// Stream writes Event Schema v1 events to the session handle's canonical
// events artifact and returns the artifact path plus a drain waiter.
//
// When the session handle is absent (e.g. a misconfigured topology that did
// not wire a SessionManager) the returned waiter still drains eventsCh so the
// bridge pump cannot wedge on a full channel with no reader — liveness wins
// over recording. The previous behavior leaked a pump goroutine and held the
// per-issue executor lock until stale-inference killed the agent at 30m.
func Stream(taskCtx *runtimepkg.TaskContext, eventsCh <-chan launcherevents.Event) (string, func() error) {
	handle := taskCtx.SessionHandle()
	if handle == nil {
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range eventsCh {
			}
		}()
		return "", func() error { <-done; return nil }
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
