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
	StdoutPath() string
}

// BridgeSession wraps an agent.Session and translates its Events into
// launcher events, writing JSONL to a SessionHandle if available.
type BridgeSession struct {
	SessionID string
	Sess      Session
	Handle    SessionHandle
}

// Run reads events from the agent session, translates them to launcher events,
// writes JSONL to the session handle, and blocks until the session completes.
func (bs *BridgeSession) Run(ctx context.Context, events chan<- launcherevents.Event) (*BridgeResult, error) {
	var seq uint64
	sessionID := bs.SessionID
	if sessionID == "" {
		sessionID = bs.Sess.ID()
	}

	// Drain events from agent session.
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for evt := range bs.Sess.Events() {
			// Write raw JSONL to session handle.
			raw := evt.Raw
			if len(raw) == 0 {
				raw = evt.Body
			}
			if bs.Handle != nil && len(raw) > 0 {
				line := append(append([]byte(nil), raw...), '\n')
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
			raw = evt.Raw
			if len(raw) == 0 {
				raw = body
			}
			turnID := evt.TurnID
			if turnID == "" {
				turnID = sessionID
			}

			seq++
			le := launcherevents.Event{
				Kind:      kind,
				Timestamp: time.Now().UTC(),
				SessionID: sessionID,
				TurnID:    turnID,
				Seq:       seq,
				Payload:   body,
				Raw:       raw,
			}
			select {
			case events <- le:
			case <-ctx.Done():
				return
			}
		}
	}()

	result, err := bs.Sess.Wait(ctx)
	if err != nil && ctx.Err() != nil {
		_ = bs.Sess.Close()
	}
	<-pumpDone

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
	br.SessionPath = sessionArtifactPath(bs.Handle)
	br.SessionRef = result.SessionRef
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
	SessionPath string
	SessionRef  SessionRef
}

func sessionArtifactPath(handle SessionHandle) string {
	if handle == nil {
		return ""
	}
	return handle.StdoutPath()
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
