package agent

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

// SessionHandle is the subset of launcher.ManagedSession the bridge uses.
type SessionHandle interface {
	WriteStdout([]byte) error
}

// BridgeSession wraps an agent.Session and translates its Events into
// launcher events, writing JSONL to a SessionHandle if available.
type BridgeSession struct {
	Sess   Session
	Handle SessionHandle
}

// Run reads events from the agent session, translates them to launcher events,
// writes JSONL to the session handle, and blocks until the session completes.
func (bs *BridgeSession) Run(ctx context.Context, events chan<- launcherevents.Event) (*BridgeResult, error) {
	var seq uint64
	sessionID := bs.Sess.ID()

	// Drain events from agent session.
	go func() {
		for evt := range bs.Sess.Events() {
			// Write raw JSONL to session handle.
			if bs.Handle != nil && len(evt.Body) > 0 {
				line := append(append([]byte(nil), evt.Body...), '\n')
				_ = bs.Handle.WriteStdout(line)
			}

			if events == nil {
				continue
			}

			// Translate agent.Event -> launcher events.Event
			kind := translateKind(evt.Kind)
			if kind == "" {
				continue
			}

			body := evt.Body
			if len(body) == 0 {
				body = json.RawMessage("{}")
			}

			seq++
			le := launcherevents.Event{
				Kind:      kind,
				Timestamp: time.Now().UTC(),
				SessionID: sessionID,
				TurnID:    sessionID,
				Seq:       seq,
				Payload:   body,
				Raw:       body,
			}
			select {
			case events <- le:
			case <-ctx.Done():
				return
			}
		}
	}()

	result, err := bs.Sess.Wait(ctx)

	br := &BridgeResult{
		ExitCode:    result.ExitCode,
		Duration:    result.Duration,
		LastMessage: result.FinalMsg,
	}
	if len(result.FilesChanged) > 0 {
		br.Meta = map[string]string{
			"files_changed": strings.Join(result.FilesChanged, ","),
		}
	}
	return br, err
}

// Close closes the underlying agent session.
func (bs *BridgeSession) Close() error {
	return bs.Sess.Close()
}

// BridgeResult holds the outcome of a bridged session run.
type BridgeResult struct {
	ExitCode    int
	Duration    time.Duration
	LastMessage string
	Meta        map[string]string
}

// translateKind maps agent.Event.Kind strings to launcher events.EventKind.
func translateKind(kind string) launcherevents.EventKind {
	switch kind {
	case "turn.started":
		return launcherevents.KindTurnStarted
	case "turn.completed":
		return launcherevents.KindTurnCompleted
	case "agent.message":
		return launcherevents.KindAgentMessage
	case "tool.call":
		return launcherevents.KindToolCall
	case "tool.result":
		return launcherevents.KindToolResult
	case "error":
		return launcherevents.KindError
	case "reasoning":
		return launcherevents.KindReasoning
	case "command.exec":
		return launcherevents.KindCommandExec
	case "command.output":
		return launcherevents.KindCommandOutput
	case "file.change":
		return launcherevents.KindFileChange
	case "token.usage":
		return launcherevents.KindTokenUsage
	case "task.complete":
		return launcherevents.KindTaskComplete
	case "log":
		return launcherevents.KindLog
	case "internal":
		return "" // skip
	default:
		return launcherevents.KindLog
	}
}
