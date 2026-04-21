package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

type ClaudeToolCall struct {
	Name string
	Args json.RawMessage
}

type ClaudeStreamEventMapper struct {
	SessionID        string
	SessionRefValue  SessionRef
	TurnID           string
	TokenUsageValue  *launcherevents.TokenUsagePayload
	ToolCalls        map[string]ClaudeToolCall
	CurrentMessage   strings.Builder
	LastMessageValue string
	TurnCompleteSaw  bool
}

func NewClaudeStreamEventMapper(sessionID string) *ClaudeStreamEventMapper {
	return &ClaudeStreamEventMapper{
		SessionID: sessionID,
		ToolCalls: map[string]ClaudeToolCall{},
	}
}

func (m *ClaudeStreamEventMapper) EffectiveTurnID() string {
	if m.TurnID != "" {
		return m.TurnID
	}
	return m.SessionID
}

func (m *ClaudeStreamEventMapper) Map(line []byte, seq *uint64) []launcherevents.Event {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindLog, launcherevents.LogPayload{Stream: "stdout", Line: string(line)}, line)}
	}

	msgType := rawString(raw, "type")
	switch msgType {
	case "system.init":
		turnID := firstNonEmpty(rawString(raw, "session_id"), rawString(raw, "sessionId"), m.EffectiveTurnID())
		m.TurnID = turnID
		if sessionID := firstNonEmpty(rawString(raw, "session_id"), rawString(raw, "sessionId")); sessionID != "" {
			m.SessionRefValue = SessionRef{ID: sessionID, Kind: "claude-session"}
		}
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindTurnStarted, launcherevents.TurnStartedPayload{TurnID: turnID}, line)}

	case "assistant.message_start", "assistant.content_block_stop", "assistant.message_delta":
		return nil

	case "assistant.content_block_delta":
		delta := rawObject(raw, "delta")
		switch rawString(delta, "type") {
		case "text_delta":
			text := rawString(delta, "text")
			m.CurrentMessage.WriteString(text)
			return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindAgentMessage, launcherevents.AgentMessagePayload{Text: text, Delta: true, Final: false}, line)}
		case "thinking_delta":
			return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindReasoning, launcherevents.ReasoningPayload{Text: rawString(delta, "thinking"), Delta: true}, line)}
		default:
			return nil
		}

	case "assistant.content_block_start":
		block := rawObject(raw, "content_block")
		if block == nil {
			block = rawObject(raw, "contentBlock")
		}
		if rawString(block, "type") != "tool_use" {
			return nil
		}

		callID := rawString(block, "id")
		name := rawString(block, "name")
		args := cloneRaw(block["input"])
		m.ToolCalls[callID] = ClaudeToolCall{Name: name, Args: args}

		out := []launcherevents.Event{m.makeEvent(seq, launcherevents.KindToolCall, launcherevents.ToolCallPayload{
			Name:   name,
			CallID: callID,
			Args:   args,
		}, line)}
		if execPayload := ClaudeCommandExecPayload(name, callID, args); execPayload != nil {
			out = append(out, m.makeEvent(seq, launcherevents.KindCommandExec, *execPayload, line))
		}
		return out

	case "user.tool_result":
		callID := rawString(raw, "tool_use_id")
		ok := !rawBool(raw, "is_error")
		result := cloneRaw(raw["content"])
		if len(result) == 0 {
			result = append(json.RawMessage(nil), line...)
		}

		out := []launcherevents.Event{m.makeEvent(seq, launcherevents.KindToolResult, launcherevents.ToolResultPayload{
			CallID: callID,
			OK:     ok,
			Result: result,
		}, line)}

		call := m.ToolCalls[callID]
		if output := ClaudeToolResultText(raw["content"]); output != "" && isClaudeCommandTool(call.Name) {
			out = append(out, m.makeEvent(seq, launcherevents.KindCommandOutput, launcherevents.CommandOutputPayload{
				CallID: callID,
				Stream: "stdout",
				Data:   output,
			}, line))
		}
		if ok {
			for _, change := range ClaudeFileChanges(call.Name, call.Args) {
				out = append(out, m.makeEvent(seq, launcherevents.KindFileChange, change, line))
			}
		}
		delete(m.ToolCalls, callID)
		return out

	case "assistant.message_stop":
		if text := strings.TrimSpace(m.CurrentMessage.String()); text != "" {
			m.LastMessageValue = text
		}
		m.CurrentMessage.Reset()

		var out []launcherevents.Event
		if usage := ClaudeTokenUsage(rawObject(raw, "usage")); usage != nil {
			m.TokenUsageValue = usage
			out = append(out, m.makeEvent(seq, launcherevents.KindTokenUsage, *usage, line))
		}
		m.TurnCompleteSaw = true
		out = append(out, m.makeEvent(seq, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{
			TurnID: m.EffectiveTurnID(),
			Status: "ok",
		}, line))
		return out

	case "system.error":
		code := rawString(raw, "code")
		if code == "" {
			code = "unknown"
		}
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindError, launcherevents.ErrorPayload{
			Code:        code,
			Message:     firstNonEmpty(rawString(raw, "message"), string(line)),
			Recoverable: false,
		}, line)}

	default:
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindLog, launcherevents.LogPayload{Stream: "stdout", Line: string(line)}, line)}
	}
}

func (m *ClaudeStreamEventMapper) makeEvent(seq *uint64, kind launcherevents.EventKind, payload any, raw []byte) launcherevents.Event {
	*seq = *seq + 1
	payloadJSON, err := launcherevents.EncodePayload(payload)
	if err != nil {
		payloadJSON = []byte(`{"message":"event payload encode failed"}`)
	}
	return launcherevents.Event{
		Kind:      kind,
		Timestamp: time.Now().UTC(),
		SessionID: m.SessionID,
		TurnID:    m.EffectiveTurnID(),
		Seq:       *seq,
		Payload:   payloadJSON,
		Raw:       cloneRaw(raw),
	}
}

func (s *ProcessSession) runClaudeStream(ctx context.Context, timeout time.Duration, events chan<- launcherevents.Event) (*Result, error) {
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, err := s.BuildCommand(execCtx)
	if err != nil {
		infra := &Result{ExitCode: -1, Meta: map[string]string{}}
		MarkInfraFailure(infra, "build command failed (template render)")
		return infra, err
	}
	if s.Task.WorkDir != "" {
		cmd.Dir = s.Task.WorkDir
	}
	cmd.Env = BuildScopedEnv(s.Agent, s.Task)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		infra := &Result{ExitCode: -1, Meta: map[string]string{}}
		MarkInfraFailure(infra, "stdout pipe setup failed")
		return infra, fmt.Errorf("runtime: %s: stdout pipe: %w", s.RuntimeName, err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		infra := &Result{ExitCode: -1, Meta: map[string]string{}}
		MarkInfraFailure(infra, "stderr pipe setup failed")
		return infra, fmt.Errorf("runtime: %s: stderr pipe: %w", s.RuntimeName, err)
	}
	if err := cmd.Start(); err != nil {
		infra := &Result{ExitCode: -1, Meta: map[string]string{}}
		reason := "claude process start failed"
		if IsExecStartError(err) {
			reason = "claude binary not runnable (exec start error)"
		}
		MarkInfraFailure(infra, reason)
		return infra, fmt.Errorf("runtime: %s: start: %w", s.RuntimeName, err)
	}

	start := time.Now()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var seq uint64
	mapper := NewClaudeStreamEventMapper(s.Task.Session.ID)
	EmitPermissionEvent(events, &seq, s.Task.Session.ID, mapper.EffectiveTurnID(), s.Agent, EmitEvent)

	var wg sync.WaitGroup
	var stdoutErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		dec := json.NewDecoder(stdoutPipe)
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, fs.ErrClosed) {
					stdoutErr = err
				}
				return
			}
			stdout.Write(raw)
			stdout.WriteByte('\n')
			if handle := s.Task.SessionHandle(); handle != nil {
				_ = handle.WriteStdout(append(append([]byte(nil), raw...), '\n'))
			}
			for _, evt := range mapper.Map(raw, &seq) {
				if handle := s.Task.SessionHandle(); handle != nil {
					_ = PersistToolCallEvent(handle, "claude-code", evt)
				}
				if events != nil {
					select {
					case events <- evt:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = stderr.ReadFrom(stderrPipe)
	}()

	wg.Wait()
	runErr := cmd.Wait()
	duration := time.Since(start)

	if stdoutErr != nil {
		infra := &Result{
			ExitCode: -1,
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			Duration: duration,
			Meta:     map[string]string{},
		}
		MarkInfraFailure(infra, "stdout stream read error")
		return infra, fmt.Errorf("runtime: %s: read stdout: %w", s.RuntimeName, stdoutErr)
	}

	if execCtx.Err() == context.DeadlineExceeded {
		if events != nil {
			EmitEvent(events, &seq, s.Task.Session.ID, mapper.EffectiveTurnID(), launcherevents.KindError, launcherevents.ErrorPayload{Code: "timeout", Message: execCtx.Err().Error(), Recoverable: false}, nil)
			EmitEvent(events, &seq, s.Task.Session.ID, mapper.EffectiveTurnID(), launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: mapper.EffectiveTurnID(), Status: "error"}, nil)
			mapper.TurnCompleteSaw = true
		}
		return &Result{
			ExitCode:    -1,
			Stdout:      stdout.String(),
			Stderr:      stderr.String(),
			Duration:    duration,
			Meta:        map[string]string{"timeout": "true"},
			LastMessage: mapper.LastMessageValue,
			TokenUsage:  mapper.TokenUsageValue,
			SessionRef:  mapper.SessionRefValue,
		}, fmt.Errorf("runtime: %s: timeout after %s: %w", s.RuntimeName, timeout, execCtx.Err())
	}

	if ctx.Err() != nil {
		if events != nil {
			EmitEvent(events, &seq, s.Task.Session.ID, mapper.EffectiveTurnID(), launcherevents.KindError, launcherevents.ErrorPayload{Code: "cancelled", Message: ctx.Err().Error(), Recoverable: false}, nil)
			EmitEvent(events, &seq, s.Task.Session.ID, mapper.EffectiveTurnID(), launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: mapper.EffectiveTurnID(), Status: "interrupted"}, nil)
			mapper.TurnCompleteSaw = true
		}
		return &Result{
			ExitCode:    -1,
			Stdout:      stdout.String(),
			Stderr:      stderr.String(),
			Duration:    duration,
			LastMessage: mapper.LastMessageValue,
			TokenUsage:  mapper.TokenUsageValue,
			SessionRef:  mapper.SessionRefValue,
		}, fmt.Errorf("runtime: %s: cancelled: %w", s.RuntimeName, ctx.Err())
	}

	exitCode := 0
	if runErr != nil {
		exitErr, ok := runErr.(*exec.ExitError)
		if !ok {
			infra := &Result{
				ExitCode:    -1,
				Stdout:      stdout.String(),
				Stderr:      stderr.String(),
				Duration:    duration,
				LastMessage: mapper.LastMessageValue,
				TokenUsage:  mapper.TokenUsageValue,
				SessionRef:  mapper.SessionRefValue,
				Meta:        map[string]string{},
			}
			MarkInfraFailure(infra, "claude wait error (non-exit)")
			return infra, fmt.Errorf("runtime: %s: wait: %w", s.RuntimeName, runErr)
		}
		exitCode = exitErr.ExitCode()
	}

	if events != nil && !mapper.TurnCompleteSaw {
		status := "ok"
		if exitCode != 0 {
			status = "error"
			EmitEvent(events, &seq, s.Task.Session.ID, mapper.EffectiveTurnID(), launcherevents.KindError, launcherevents.ErrorPayload{
				Code:        "claude_exit",
				Message:     fmt.Sprintf("claude exited %d without assistant.message_stop", exitCode),
				Recoverable: false,
			}, nil)
		}
		EmitEvent(events, &seq, s.Task.Session.ID, mapper.EffectiveTurnID(), launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: mapper.EffectiveTurnID(), Status: status}, nil)
	}

	sessionPath := ""
	if s.FindSession != nil {
		sessionPath = s.FindSession(s.SessionLookupPath())
	}
	if events != nil && stderr.Len() > 0 {
		if handle := s.Task.SessionHandle(); handle != nil {
			_ = handle.WriteStderr(stderr.Bytes())
		}
		for _, line := range splitLines(stderr.String()) {
			EmitEvent(events, &seq, s.Task.Session.ID, mapper.EffectiveTurnID(), launcherevents.KindLog, launcherevents.LogPayload{Stream: "stderr", Line: line}, nil)
		}
	}

	result := &Result{
		ExitCode:    exitCode,
		Stdout:      stdout.String(),
		Stderr:      stderr.String(),
		Duration:    duration,
		SessionPath: sessionPath,
		LastMessage: mapper.LastMessageValue,
		TokenUsage:  mapper.TokenUsageValue,
		SessionRef:  mapper.SessionRefValue,
	}
	if exitCode != 0 && !mapper.TurnCompleteSaw && mapper.LastMessageValue == "" && StderrLooksLikeRuntimePanic(stderr.String()) {
		MarkInfraFailure(result, "claude runtime panic/abort before agent output")
	}
	return result, nil
}

func ClaudeTokenUsage(raw map[string]json.RawMessage) *launcherevents.TokenUsagePayload {
	if len(raw) == 0 {
		return nil
	}
	payload := &launcherevents.TokenUsagePayload{
		Input:  rawIntValue(raw, "input_tokens"),
		Output: rawIntValue(raw, "output_tokens"),
		Cached: firstInt(raw, "cached_input_tokens", "cache_read_input_tokens"),
	}
	payload.Total = payload.Input + payload.Output
	if payload.Input == 0 && payload.Output == 0 && payload.Cached == 0 {
		return nil
	}
	return payload
}

func firstInt(raw map[string]json.RawMessage, keys ...string) int {
	for _, key := range keys {
		if value, ok := rawInt(raw, key); ok {
			return value
		}
	}
	return 0
}

func isClaudeCommandTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "bash":
		return true
	default:
		return false
	}
}

func ClaudeCommandExecPayload(name, callID string, rawArgs json.RawMessage) *launcherevents.CommandExecPayload {
	if len(rawArgs) == 0 {
		return nil
	}
	if !isClaudeCommandTool(name) {
		return nil
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil
	}
	command := firstNonEmpty(rawString(args, "command"), rawString(args, "cmd"))
	cmd := rawStringSlice(args, "argv")
	if len(cmd) == 0 && len(rawObject(args, "command")) == 0 && command != "" {
		cmd = strings.Fields(command)
	}
	if len(cmd) == 0 && command != "" {
		cmd = []string{command}
	}
	if len(cmd) == 0 {
		return nil
	}
	return &launcherevents.CommandExecPayload{
		Cmd:    cmd,
		CWD:    firstNonEmpty(rawString(args, "cwd"), rawString(args, "workdir")),
		CallID: callID,
	}
}

func ClaudeFileChanges(name string, rawArgs json.RawMessage) []launcherevents.FileChangePayload {
	tool := strings.ToLower(strings.TrimSpace(name))
	if len(rawArgs) == 0 {
		return nil
	}
	if !(strings.Contains(tool, "edit") || strings.Contains(tool, "write")) {
		return nil
	}

	var args map[string]json.RawMessage
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil
	}

	path := firstNonEmpty(rawString(args, "path"), rawString(args, "file_path"), rawString(args, "filename"), rawString(args, "target_file"))
	if path == "" {
		return nil
	}

	changeKind := "modify"
	if strings.Contains(tool, "write") {
		changeKind = "create"
	}

	diff := firstNonEmpty(rawString(args, "diff"), rawString(args, "patch"))
	if diff == "" {
		oldStr := rawString(args, "old_string")
		newStr := rawString(args, "new_string")
		if oldStr != "" || newStr != "" {
			diff = fmt.Sprintf("--- %s\n+++ %s\n- %s\n+ %s\n", path, path, oldStr, newStr)
		}
	}

	return []launcherevents.FileChangePayload{{
		Path:       path,
		ChangeKind: changeKind,
		Diff:       diff,
	}}
}

func ClaudeToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		typeName, _ := block["type"].(string)
		if typeName != "text" {
			continue
		}
		if value, _ := block["text"].(string); strings.TrimSpace(value) != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "\n")
}
