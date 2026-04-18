package launcher

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

type claudeToolCall struct {
	Name string
	Args json.RawMessage
}

type claudeStreamEventMapper struct {
	sessionID       string
	sessionRef      SessionRef
	turnID          string
	tokenUsage      *launcherevents.TokenUsagePayload
	toolCalls       map[string]claudeToolCall
	currentMessage  strings.Builder
	lastMessage     string
	turnCompleteSaw bool
}

func newClaudeStreamEventMapper(sessionID string) *claudeStreamEventMapper {
	return &claudeStreamEventMapper{
		sessionID: sessionID,
		toolCalls: map[string]claudeToolCall{},
	}
}

func (m *claudeStreamEventMapper) effectiveTurnID() string {
	if m.turnID != "" {
		return m.turnID
	}
	return m.sessionID
}

func (m *claudeStreamEventMapper) Map(line []byte, seq *uint64) []launcherevents.Event {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindLog, launcherevents.LogPayload{Stream: "stdout", Line: string(line)}, line)}
	}

	msgType := rawString(raw, "type")
	switch msgType {
	case "system.init":
		turnID := firstNonEmpty(rawString(raw, "session_id"), rawString(raw, "sessionId"), m.effectiveTurnID())
		m.turnID = turnID
		if sessionID := firstNonEmpty(rawString(raw, "session_id"), rawString(raw, "sessionId")); sessionID != "" {
			m.sessionRef = SessionRef{ID: sessionID, Kind: "claude-session"}
		}
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindTurnStarted, launcherevents.TurnStartedPayload{TurnID: turnID}, line)}

	case "assistant.message_start", "assistant.content_block_stop", "assistant.message_delta":
		return nil

	case "assistant.content_block_delta":
		delta := rawObject(raw, "delta")
		switch rawString(delta, "type") {
		case "text_delta":
			text := rawString(delta, "text")
			m.currentMessage.WriteString(text)
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
		m.toolCalls[callID] = claudeToolCall{Name: name, Args: args}

		out := []launcherevents.Event{m.makeEvent(seq, launcherevents.KindToolCall, launcherevents.ToolCallPayload{
			Name:   name,
			CallID: callID,
			Args:   args,
		}, line)}
		if execPayload := claudeCommandExecPayload(name, callID, args); execPayload != nil {
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

		call := m.toolCalls[callID]
		if output := claudeToolResultText(raw["content"]); output != "" && isClaudeCommandTool(call.Name) {
			out = append(out, m.makeEvent(seq, launcherevents.KindCommandOutput, launcherevents.CommandOutputPayload{
				CallID: callID,
				Stream: "stdout",
				Data:   output,
			}, line))
		}
		if ok {
			for _, change := range claudeFileChanges(call.Name, call.Args) {
				out = append(out, m.makeEvent(seq, launcherevents.KindFileChange, change, line))
			}
		}
		delete(m.toolCalls, callID)
		return out

	case "assistant.message_stop":
		if text := strings.TrimSpace(m.currentMessage.String()); text != "" {
			m.lastMessage = text
		}
		m.currentMessage.Reset()

		var out []launcherevents.Event
		if usage := claudeTokenUsage(rawObject(raw, "usage")); usage != nil {
			m.tokenUsage = usage
			out = append(out, m.makeEvent(seq, launcherevents.KindTokenUsage, *usage, line))
		}
		m.turnCompleteSaw = true
		out = append(out, m.makeEvent(seq, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{
			TurnID: m.effectiveTurnID(),
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

func (m *claudeStreamEventMapper) makeEvent(seq *uint64, kind launcherevents.EventKind, payload any, raw []byte) launcherevents.Event {
	*seq = *seq + 1
	return launcherevents.Event{
		Kind:      kind,
		Timestamp: time.Now().UTC(),
		SessionID: m.sessionID,
		TurnID:    m.effectiveTurnID(),
		Seq:       *seq,
		Payload:   launcherevents.MustPayload(payload),
		Raw:       cloneRaw(raw),
	}
}

func (s *processSession) wantsClaudeStreamJSON() bool {
	if !isClaudeRuntime(s.runtimeName) {
		return false
	}
	if strings.TrimSpace(s.agent.Prompt) != "" {
		return true
	}
	rendered, err := renderCommand(s.agent.Command, s.task)
	if err != nil {
		return false
	}
	_, _, ok := extractPrompt(rendered)
	return ok
}

func (s *processSession) runClaudeStream(ctx context.Context, timeout time.Duration, events chan<- launcherevents.Event) (*Result, error) {
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, err := s.buildCommand(execCtx)
	if err != nil {
		infra := &Result{ExitCode: -1, Meta: map[string]string{}}
		markInfraFailure(infra, "build command failed (template render)")
		return infra, err
	}
	if s.task.WorkDir != "" {
		cmd.Dir = s.task.WorkDir
	}
	cmd.Env = buildScopedEnv(s.agent, s.task)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		infra := &Result{ExitCode: -1, Meta: map[string]string{}}
		markInfraFailure(infra, "stdout pipe setup failed")
		return infra, fmt.Errorf("launcher: %s: stdout pipe: %w", s.runtimeName, err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		infra := &Result{ExitCode: -1, Meta: map[string]string{}}
		markInfraFailure(infra, "stderr pipe setup failed")
		return infra, fmt.Errorf("launcher: %s: stderr pipe: %w", s.runtimeName, err)
	}
	if err := cmd.Start(); err != nil {
		infra := &Result{ExitCode: -1, Meta: map[string]string{}}
		reason := "claude process start failed"
		if isExecStartError(err) {
			reason = "claude binary not runnable (exec start error)"
		}
		markInfraFailure(infra, reason)
		return infra, fmt.Errorf("launcher: %s: start: %w", s.runtimeName, err)
	}

	start := time.Now()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var seq uint64
	mapper := newClaudeStreamEventMapper(s.task.Session.ID)
	emitPermissionEvent(events, &seq, s.task.Session.ID, mapper.effectiveTurnID(), s.agent)

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
			if handle := s.task.SessionHandle(); handle != nil {
				_ = handle.WriteStdout(append(append([]byte(nil), raw...), '\n'))
			}
			for _, evt := range mapper.Map(raw, &seq) {
				if handle := s.task.SessionHandle(); handle != nil {
					_ = persistToolCallEvent(handle, "claude-code", evt)
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
		markInfraFailure(infra, "stdout stream read error")
		return infra, fmt.Errorf("launcher: %s: read stdout: %w", s.runtimeName, stdoutErr)
	}

	if execCtx.Err() == context.DeadlineExceeded {
		if events != nil {
			emitEvent(events, &seq, s.task.Session.ID, mapper.effectiveTurnID(), launcherevents.KindError, launcherevents.ErrorPayload{Code: "timeout", Message: execCtx.Err().Error(), Recoverable: false}, nil)
			emitEvent(events, &seq, s.task.Session.ID, mapper.effectiveTurnID(), launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: mapper.effectiveTurnID(), Status: "error"}, nil)
			mapper.turnCompleteSaw = true
		}
		return &Result{
			ExitCode:    -1,
			Stdout:      stdout.String(),
			Stderr:      stderr.String(),
			Duration:    duration,
			Meta:        map[string]string{"timeout": "true"},
			LastMessage: mapper.lastMessage,
			TokenUsage:  mapper.tokenUsage,
			SessionRef:  mapper.sessionRef,
		}, fmt.Errorf("launcher: %s: timeout after %s: %w", s.runtimeName, timeout, execCtx.Err())
	}

	if ctx.Err() != nil {
		if events != nil {
			emitEvent(events, &seq, s.task.Session.ID, mapper.effectiveTurnID(), launcherevents.KindError, launcherevents.ErrorPayload{Code: "cancelled", Message: ctx.Err().Error(), Recoverable: false}, nil)
			emitEvent(events, &seq, s.task.Session.ID, mapper.effectiveTurnID(), launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: mapper.effectiveTurnID(), Status: "interrupted"}, nil)
			mapper.turnCompleteSaw = true
		}
		return &Result{
			ExitCode:    -1,
			Stdout:      stdout.String(),
			Stderr:      stderr.String(),
			Duration:    duration,
			LastMessage: mapper.lastMessage,
			TokenUsage:  mapper.tokenUsage,
			SessionRef:  mapper.sessionRef,
		}, fmt.Errorf("launcher: %s: cancelled: %w", s.runtimeName, ctx.Err())
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
				LastMessage: mapper.lastMessage,
				TokenUsage:  mapper.tokenUsage,
				SessionRef:  mapper.sessionRef,
				Meta:        map[string]string{},
			}
			markInfraFailure(infra, "claude wait error (non-exit)")
			return infra, fmt.Errorf("launcher: %s: wait: %w", s.runtimeName, runErr)
		}
		exitCode = exitErr.ExitCode()
	}

	if events != nil && !mapper.turnCompleteSaw {
		status := "ok"
		if exitCode != 0 {
			status = "error"
			emitEvent(events, &seq, s.task.Session.ID, mapper.effectiveTurnID(), launcherevents.KindError, launcherevents.ErrorPayload{
				Code:        "claude_exit",
				Message:     fmt.Sprintf("claude exited %d without assistant.message_stop", exitCode),
				Recoverable: false,
			}, nil)
		}
		emitEvent(events, &seq, s.task.Session.ID, mapper.effectiveTurnID(), launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: mapper.effectiveTurnID(), Status: status}, nil)
	}

	sessionPath := ""
	if s.findSession != nil {
		sessionPath = s.findSession(s.sessionLookupPath())
	}
	if events != nil && stderr.Len() > 0 {
		if handle := s.task.SessionHandle(); handle != nil {
			_ = handle.WriteStderr(stderr.Bytes())
		}
		for _, line := range splitLines(stderr.String()) {
			emitEvent(events, &seq, s.task.Session.ID, mapper.effectiveTurnID(), launcherevents.KindLog, launcherevents.LogPayload{Stream: "stderr", Line: line}, nil)
		}
	}

	result := &Result{
		ExitCode:    exitCode,
		Stdout:      stdout.String(),
		Stderr:      stderr.String(),
		Duration:    duration,
		SessionPath: sessionPath,
		LastMessage: mapper.lastMessage,
		TokenUsage:  mapper.tokenUsage,
		SessionRef:  mapper.sessionRef,
	}
	// Claude exited non-zero but we never observed an agent turn completion
	// and never saw any assistant message. If the stderr looks like a Rust
	// plugin-cache panic or similar runtime abort, this is an infra failure,
	// not an agent verdict. We gate on (!turnCompleteSaw AND empty
	// lastMessage) to avoid mis-classifying an agent that emitted tool calls
	// but crashed partway through; in that case we still have agent output
	// to attribute the failure to.
	if exitCode != 0 && !mapper.turnCompleteSaw && mapper.lastMessage == "" && stderrLooksLikeRuntimePanic(stderr.String()) {
		markInfraFailure(result, "claude runtime panic/abort before agent output")
	}
	return result, nil
}

func claudeTokenUsage(raw map[string]json.RawMessage) *launcherevents.TokenUsagePayload {
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

func claudeCommandExecPayload(name, callID string, rawArgs json.RawMessage) *launcherevents.CommandExecPayload {
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

func claudeFileChanges(name string, rawArgs json.RawMessage) []launcherevents.FileChangePayload {
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

func claudeToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, block := range blocks {
			if value, ok := block["text"].(string); ok && value != "" {
				parts = append(parts, value)
				continue
			}
			if value, ok := block["content"].(string); ok && value != "" {
				parts = append(parts, value)
			}
		}
		return strings.Join(parts, "\n")
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		if value, ok := obj["text"].(string); ok {
			return value
		}
		if value, ok := obj["output"].(string); ok {
			return value
		}
	}
	return ""
}
