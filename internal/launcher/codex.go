package launcher

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

type CodexRuntime struct{}

func (r *CodexRuntime) Name() string { return config.RuntimeCodexExec }

func (r *CodexRuntime) Start(_ context.Context, agent *config.AgentConfig, task *TaskContext) (Session, error) {
	prompt, err := codexPrompt(agent, task)
	if err != nil {
		return nil, err
	}
	return newCodexSession(agent, task, prompt), nil
}

func (r *CodexRuntime) Launch(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (*Result, error) {
	sess, err := r.Start(ctx, agent, task)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()
	return sess.Run(ctx, nil)
}

type codexSession struct {
	agent        *config.AgentConfig
	task         *TaskContext
	prompt       string
	lastMsgPath  string
	stdoutPath   string
	cachedResult *Result
}

var (
	codexTaskCompleteGracePeriod = time.Minute
	codexTaskCompleteKillDelay   = 10 * time.Second
)

func newCodexSession(agent *config.AgentConfig, task *TaskContext, prompt string) *codexSession {
	if task != nil && task.SessionHandle() != nil {
		return &codexSession{
			agent:       agent,
			task:        task,
			prompt:      prompt,
			lastMsgPath: filepath.Join(task.SessionHandle().Dir(), "codex-last-message.txt"),
			stdoutPath:  task.SessionHandle().StdoutPath(),
		}
	}
	baseDir := task.RepoRoot
	if baseDir == "" {
		baseDir = task.WorkDir
	}
	if baseDir == "" {
		baseDir = "."
	}
	artifactDir := filepath.Join(baseDir, ".workbuddy", "sessions", task.Session.ID)
	return &codexSession{
		agent:       agent,
		task:        task,
		prompt:      prompt,
		lastMsgPath: filepath.Join(artifactDir, "codex-last-message.txt"),
		stdoutPath:  filepath.Join(artifactDir, "codex-exec.jsonl"),
	}
}

func (s *codexSession) Run(ctx context.Context, events chan<- launcherevents.Event) (*Result, error) {
	if s.cachedResult != nil {
		return s.cachedResult, nil
	}
	if s.task.SessionHandle() == nil {
		if err := os.MkdirAll(filepath.Dir(s.stdoutPath), 0o755); err != nil {
			return nil, fmt.Errorf("launcher: codex-exec: create artifact dir: %w", err)
		}
	}

	timeout := s.agent.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, "codex", s.buildArgs()...)
	cmd.Stdin = strings.NewReader(s.prompt)
	if s.task.WorkDir != "" {
		cmd.Dir = s.task.WorkDir
	}
	cmd.Env = buildScopedEnv(s.agent, s.task)
	var seq uint64
	mapper := newCodexEventMapper(s.task.Session.ID)
	emitPermissionEvent(events, &seq, s.task.Session.ID, mapper.effectiveTurnID(), s.agent)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		infra := &Result{ExitCode: -1, Meta: map[string]string{}}
		markInfraFailure(infra, "stdout pipe setup failed")
		return infra, fmt.Errorf("launcher: codex-exec: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		infra := &Result{ExitCode: -1, Meta: map[string]string{}}
		markInfraFailure(infra, "stderr pipe setup failed")
		return infra, fmt.Errorf("launcher: codex-exec: stderr pipe: %w", err)
	}

	var stdoutFile *os.File
	if s.task.SessionHandle() == nil {
		stdoutFile, err = os.Create(s.stdoutPath)
		if err != nil {
			return nil, fmt.Errorf("launcher: codex-exec: create stdout artifact: %w", err)
		}
		defer func() { _ = stdoutFile.Close() }()
	}

	// Debug: log arg/env sizes to diagnose E2BIG
	argSize := 0
	for _, a := range cmd.Args {
		argSize += len(a)
	}
	envSize := 0
	for _, e := range cmd.Env {
		envSize += len(e)
	}
	log.Printf("[codex-debug] args=%d env=%d prompt=%d", argSize, envSize, len(s.prompt))

	if err := cmd.Start(); err != nil {
		infra := &Result{ExitCode: -1, Meta: map[string]string{}}
		reason := "codex process start failed"
		if isExecStartError(err) {
			reason = "codex binary not runnable (exec start error)"
		}
		markInfraFailure(infra, reason)
		return infra, fmt.Errorf("launcher: codex-exec: start: %w", err)
	}
	start := time.Now()

	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	var scanErr error
	var wg sync.WaitGroup
	postCompleteCh := make(chan struct{}, 1)

	go func() {
		select {
		case <-execCtx.Done():
			return
		case <-postCompleteCh:
		}

		timer := time.NewTimer(codexTaskCompleteGracePeriod)
		defer timer.Stop()
		select {
		case <-execCtx.Done():
			return
		case <-timer.C:
		}

		if cmd.Process == nil {
			return
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)

		killTimer := time.NewTimer(codexTaskCompleteKillDelay)
		defer killTimer.Stop()
		select {
		case <-execCtx.Done():
			return
		case <-killTimer.C:
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdoutPipe)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			stdoutBuf.WriteString(line)
			stdoutBuf.WriteByte('\n')
			if stdoutFile != nil {
				_, _ = stdoutFile.WriteString(line + "\n")
			} else if handle := s.task.SessionHandle(); handle != nil {
				_ = handle.WriteStdout([]byte(line + "\n"))
			}
			for _, evt := range mapper.Map([]byte(line), &seq) {
				if handle := s.task.SessionHandle(); handle != nil {
					_ = persistToolCallEvent(handle, "codex", evt)
				}
				if events != nil {
					select {
					case events <- evt:
					case <-ctx.Done():
						return
					}
				}
			}
			if mapper.taskCompleteObserved() {
				select {
				case postCompleteCh <- struct{}{}:
				default:
				}
			}
		}
		scanErr = scanner.Err()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = stderrBuf.ReadFrom(stderrPipe)
	}()

	runErr := cmd.Wait()
	wg.Wait()
	if handle := s.task.SessionHandle(); handle != nil && stderrBuf.Len() > 0 {
		_ = handle.WriteStderr(stderrBuf.Bytes())
	}
	duration := time.Since(start)

	// The stdout pipe may be closed when the child process exits; that is the
	// expected termination for the scanner, not a real read error.
	if scanErr != nil && !errors.Is(scanErr, os.ErrClosed) {
		infra := &Result{
			ExitCode:    -1,
			Stdout:      stdoutBuf.String(),
			Stderr:      stderrBuf.String(),
			Duration:    duration,
			SessionPath: s.stdoutPath,
			SessionRef:  mapper.sessionRef,
			TokenUsage:  mapper.tokenUsage,
			Meta:        map[string]string{},
		}
		reason := "codex stdout scanner error"
		if isScannerBufferOverflow(scanErr) {
			reason = "codex stdout scanner buffer overflow (bufio.ErrTooLong)"
		}
		markInfraFailure(infra, reason)
		return infra, fmt.Errorf("launcher: codex-exec: read stdout: %w", scanErr)
	}

	if execCtx.Err() == context.DeadlineExceeded {
		if events != nil {
			emitEvent(events, &seq, s.task.Session.ID, mapper.effectiveTurnID(), launcherevents.KindError, launcherevents.ErrorPayload{Code: "timeout", Message: execCtx.Err().Error(), Recoverable: false}, nil)
			emitEvent(events, &seq, s.task.Session.ID, mapper.effectiveTurnID(), launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: mapper.effectiveTurnID(), Status: "error"}, nil)
			mapper.turnCompleteSaw = true
		}
		return &Result{
			ExitCode:    -1,
			Stdout:      stdoutBuf.String(),
			Stderr:      stderrBuf.String(),
			Duration:    duration,
			Meta:        map[string]string{"timeout": "true"},
			SessionPath: s.stdoutPath,
			SessionRef:  mapper.sessionRef,
			TokenUsage:  mapper.tokenUsage,
		}, fmt.Errorf("launcher: codex-exec: timeout after %s: %w", timeout, execCtx.Err())
	}

	if ctx.Err() != nil {
		if events != nil {
			emitEvent(events, &seq, s.task.Session.ID, mapper.effectiveTurnID(), launcherevents.KindError, launcherevents.ErrorPayload{Code: "cancelled", Message: ctx.Err().Error(), Recoverable: false}, nil)
			emitEvent(events, &seq, s.task.Session.ID, mapper.effectiveTurnID(), launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: mapper.effectiveTurnID(), Status: "interrupted"}, nil)
			mapper.turnCompleteSaw = true
		}
		return &Result{
			ExitCode:    -1,
			Stdout:      stdoutBuf.String(),
			Stderr:      stderrBuf.String(),
			Duration:    duration,
			SessionPath: s.stdoutPath,
			SessionRef:  mapper.sessionRef,
			TokenUsage:  mapper.tokenUsage,
		}, fmt.Errorf("launcher: codex-exec: cancelled: %w", ctx.Err())
	}

	exitCode := 0
	if runErr != nil {
		exitErr, ok := runErr.(*exec.ExitError)
		if !ok {
			infra := &Result{
				ExitCode:    -1,
				Stdout:      stdoutBuf.String(),
				Stderr:      stderrBuf.String(),
				Duration:    duration,
				SessionPath: s.stdoutPath,
				SessionRef:  mapper.sessionRef,
				TokenUsage:  mapper.tokenUsage,
				Meta:        map[string]string{},
			}
			markInfraFailure(infra, "codex wait error (non-exit)")
			return infra, fmt.Errorf("launcher: codex-exec: wait: %w", runErr)
		}
		exitCode = exitErr.ExitCode()
	}

	lastMessage := readOptionalFile(s.lastMsgPath)
	if lastMessage == "" {
		lastMessage = strings.TrimSpace(mapper.lastMessage)
	}
	result := &Result{
		ExitCode:    exitCode,
		Stdout:      stdoutBuf.String(),
		Stderr:      stderrBuf.String(),
		Duration:    duration,
		SessionPath: s.stdoutPath,
		LastMessage: lastMessage,
		SessionRef:  mapper.sessionRef,
		TokenUsage:  mapper.tokenUsage,
	}
	// Codex exited non-zero without observing task_complete and without an
	// agent last-message. If stderr looks like a Rust plugin-cache panic or
	// similar runtime abort, mark this as an infra failure so the reporter
	// does not attribute a FAIL verdict to the agent. We gate on the
	// absence of both task_complete and last-message to avoid mis-classifying
	// a codex run that did speak to the LLM before crashing.
	if exitCode != 0 && !mapper.taskCompleteObserved() && strings.TrimSpace(lastMessage) == "" && stderrLooksLikeRuntimePanic(stderrBuf.String()) {
		markInfraFailure(result, "codex runtime panic/abort before agent output")
	}
	s.cachedResult = result

	if events != nil && stderrBuf.Len() > 0 {
		for _, line := range splitLines(stderrBuf.String()) {
			emitEvent(events, &seq, s.task.Session.ID, mapper.effectiveTurnID(), launcherevents.KindLog, launcherevents.LogPayload{Stream: "stderr", Line: line}, nil)
		}
	}

	if err := validateOutputContract(s.agent, result); err != nil {
		emitOutputContractFailure(events, &seq, s.task.Session.ID, mapper.effectiveTurnID(), err)
		return result, err
	}
	if events != nil {
		if mapper.turnCompleted != nil {
			emitEvent(events, &seq, s.task.Session.ID, mapper.effectiveTurnID(), launcherevents.KindTurnCompleted, *mapper.turnCompleted, mapper.turnCompletedRaw)
		} else {
			status := "ok"
			if exitCode != 0 {
				status = "error"
				emitEvent(events, &seq, s.task.Session.ID, mapper.effectiveTurnID(), launcherevents.KindError, launcherevents.ErrorPayload{Code: "codex_exit", Message: fmt.Sprintf("codex exec exited %d without task_complete", exitCode), Recoverable: false}, nil)
			}
			emitEvent(events, &seq, s.task.Session.ID, mapper.effectiveTurnID(), launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: mapper.effectiveTurnID(), Status: status}, nil)
		}
	}

	return result, nil
}

func (s *codexSession) SetApprover(Approver) error { return ErrNotSupported }

func (s *codexSession) Close() error { return nil }

func (s *codexSession) buildArgs() []string {
	args := []string{"exec", "--json", "--output-last-message", s.lastMsgPath}
	if s.task.WorkDir != "" {
		args = append(args, "--cd", s.task.WorkDir)
	}
	if sandbox := strings.TrimSpace(s.agent.Policy.Sandbox); sandbox != "" {
		args = append(args, "--sandbox", sandbox)
	}
	switch s.agent.Policy.Approval {
	case "on-failure", "on-request":
		args = append(args, "--ask-for-approval", s.agent.Policy.Approval)
	}
	if model := strings.TrimSpace(s.agent.Policy.Model); model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, "-")
	return args
}

func codexPrompt(agent *config.AgentConfig, task *TaskContext) (string, error) {
	if strings.TrimSpace(agent.Prompt) != "" {
		return renderCommandRaw(agent.Prompt, task)
	}
	if strings.TrimSpace(agent.Command) == "" {
		return "", fmt.Errorf("launcher: codex-exec: missing prompt/command")
	}
	rendered, err := renderCommandRaw(agent.Command, task)
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(rendered)
	if len(trimmed) == 0 {
		return "", fmt.Errorf("launcher: codex-exec: cannot derive prompt from command")
	}
	quote := trimmed[len(trimmed)-1]
	if quote != '\'' && quote != '"' {
		return "", fmt.Errorf("launcher: codex-exec: cannot derive prompt from command")
	}
	start := strings.LastIndexByte(trimmed[:len(trimmed)-1], quote)
	if start < 0 {
		return "", fmt.Errorf("launcher: codex-exec: cannot derive prompt from command")
	}
	return trimmed[start+1 : len(trimmed)-1], nil
}

func readOptionalFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

type codexEventMapper struct {
	sessionID        string
	sessionRef       SessionRef
	turnID           string
	lastMessage      string
	tokenUsage       *launcherevents.TokenUsagePayload
	turnCompleteSaw  bool
	turnCompleted    *launcherevents.TurnCompletedPayload
	turnCompletedRaw []byte
}

func newCodexEventMapper(sessionID string) *codexEventMapper {
	return &codexEventMapper{sessionID: sessionID}
}

func (m *codexEventMapper) effectiveTurnID() string {
	if m.turnID != "" {
		return m.turnID
	}
	return m.sessionID
}

func (m *codexEventMapper) Map(line []byte, seq *uint64) []launcherevents.Event {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindLog, launcherevents.LogPayload{Stream: "stdout", Line: string(line)}, line)}
	}

	msgType := rawString(raw, "type")
	switch msgType {
	case "thread.started":
		if threadID := rawString(raw, "thread_id"); threadID != "" {
			m.sessionRef = SessionRef{ID: threadID, Kind: "codex-thread"}
		}
		return nil

	case "turn.started":
		turnID := firstNonEmpty(rawString(raw, "turn_id"), rawString(raw, "task_id"), m.effectiveTurnID())
		m.turnID = turnID
		if refID := firstNonEmpty(rawString(raw, "thread_id"), rawString(raw, "session_id")); refID != "" {
			kind := "codex-session"
			if rawString(raw, "thread_id") != "" {
				kind = "codex-thread"
			}
			m.sessionRef = SessionRef{ID: refID, Kind: kind}
		}
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindTurnStarted, launcherevents.TurnStartedPayload{TurnID: turnID}, line)}

	case "item.started", "item.completed":
		return m.mapItemEvent(raw, msgType == "item.completed", line, seq)

	case "turn.completed":
		status := firstNonEmpty(rawString(raw, "status"), "ok")
		m.turnCompleteSaw = true
		payload := launcherevents.TurnCompletedPayload{TurnID: m.effectiveTurnID(), Status: status}
		m.turnCompleted = &payload
		m.turnCompletedRaw = cloneRaw(line)

		var out []launcherevents.Event
		if usage := codexTurnUsage(rawObject(raw, "usage")); usage != nil {
			m.tokenUsage = usage
			out = append(out, m.makeEvent(seq, launcherevents.KindTokenUsage, *usage, line))
		}
		out = append(out, m.makeEvent(seq, launcherevents.KindTaskComplete, launcherevents.TaskCompletePayload{
			TurnID: m.effectiveTurnID(),
			Status: status,
		}, line))
		return out

	case "task_started":
		turnID := rawString(raw, "task_id")
		if turnID == "" {
			turnID = m.effectiveTurnID()
		}
		m.turnID = turnID
		if refID := firstNonEmpty(rawString(raw, "thread_id"), rawString(raw, "session_id")); refID != "" {
			kind := "codex-session"
			if rawString(raw, "thread_id") != "" {
				kind = "codex-thread"
			}
			m.sessionRef = SessionRef{ID: refID, Kind: kind}
		}
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindTurnStarted, launcherevents.TurnStartedPayload{TurnID: turnID}, line)}

	case "agent_message", "agent_message_delta":
		text := firstNonEmpty(rawString(raw, "message"), rawString(raw, "text"))
		delta := msgType == "agent_message_delta"
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindAgentMessage, launcherevents.AgentMessagePayload{Text: text, Delta: delta, Final: !delta}, line)}

	case "agent_reasoning", "agent_reasoning_delta":
		text := firstNonEmpty(rawString(raw, "text"), rawString(raw, "message"))
		delta := msgType == "agent_reasoning_delta"
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindReasoning, launcherevents.ReasoningPayload{Text: text, Delta: delta}, line)}

	case "exec_command_begin":
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindCommandExec, launcherevents.CommandExecPayload{
			Cmd:    rawStringSlice(raw, "command"),
			CWD:    rawString(raw, "cwd"),
			CallID: rawString(raw, "call_id"),
		}, line)}

	case "exec_command_output_delta":
		stream := rawString(raw, "stream")
		if stream == "" {
			stream = "stdout"
		}
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindCommandOutput, launcherevents.CommandOutputPayload{
			CallID: rawString(raw, "call_id"),
			Stream: stream,
			Data:   rawString(raw, "chunk"),
		}, line)}

	case "exec_command_end":
		ok := true
		if code, hasCode := rawInt(raw, "exit_code"); hasCode {
			ok = code == 0
		}
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindToolResult, launcherevents.ToolResultPayload{
			CallID: rawString(raw, "call_id"),
			OK:     ok,
			Result: append(json.RawMessage(nil), line...),
		}, line)}

	case "mcp_tool_call_begin":
		invocation := rawObject(raw, "invocation")
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindToolCall, launcherevents.ToolCallPayload{
			Name:   rawString(invocation, "tool"),
			CallID: rawString(raw, "call_id"),
			Args:   cloneRaw(invocation["arguments"]),
		}, line)}

	case "mcp_tool_call_end":
		result := cloneRaw(raw["result"])
		if len(result) == 0 {
			result = append(json.RawMessage(nil), line...)
		}
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindToolResult, launcherevents.ToolResultPayload{
			CallID: rawString(raw, "call_id"),
			OK:     !rawBool(raw, "is_error"),
			Result: result,
		}, line)}

	case "patch_apply_begin":
		changes := rawPatchChanges(raw)
		if len(changes) == 0 {
			return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindLog, launcherevents.LogPayload{Stream: "stdout", Line: string(line)}, line)}
		}
		out := make([]launcherevents.Event, 0, len(changes))
		for _, change := range changes {
			out = append(out, m.makeEvent(seq, launcherevents.KindFileChange, change, line))
		}
		return out

	case "token_count":
		payload := launcherevents.TokenUsagePayload{
			Input:  rawIntValue(raw, "input_tokens"),
			Output: rawIntValue(raw, "output_tokens"),
			Cached: rawIntValue(raw, "cached_input_tokens"),
		}
		payload.Total = payload.Input + payload.Output
		m.tokenUsage = &payload
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindTokenUsage, payload, line)}

	case "task_complete":
		status := "ok"
		if rawBool(raw, "interrupted") {
			status = "interrupted"
		} else if rawBool(raw, "error") {
			status = "error"
		}
		m.turnCompleteSaw = true
		payload := launcherevents.TurnCompletedPayload{TurnID: m.effectiveTurnID(), Status: status}
		m.turnCompleted = &payload
		m.turnCompletedRaw = cloneRaw(line)
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindTaskComplete, launcherevents.TaskCompletePayload{
			TurnID: m.effectiveTurnID(),
			Status: status,
		}, line)}

	case "error":
		code := rawString(raw, "code")
		if code == "" {
			code = "unknown"
		}
		message := firstNonEmpty(rawString(raw, "message"), string(line))
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindError, launcherevents.ErrorPayload{Code: code, Message: message, Recoverable: false}, line)}

	default:
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindLog, launcherevents.LogPayload{Stream: "stdout", Line: string(line)}, line)}
	}
}

func (m *codexEventMapper) mapItemEvent(raw map[string]json.RawMessage, completed bool, line []byte, seq *uint64) []launcherevents.Event {
	item := rawObject(raw, "item")
	if item == nil {
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindLog, launcherevents.LogPayload{Stream: "stdout", Line: string(line)}, line)}
	}

	itemType := rawString(item, "type")
	itemID := rawString(item, "id")
	switch itemType {
	case "agent_message":
		text := strings.TrimSpace(rawString(item, "text"))
		if text == "" {
			return nil
		}
		if completed {
			m.lastMessage = text
		}
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindAgentMessage, launcherevents.AgentMessagePayload{Text: text, Delta: false, Final: completed}, line)}

	case "reasoning", "agent_reasoning":
		text := strings.TrimSpace(rawString(item, "text"))
		if text == "" {
			return nil
		}
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindReasoning, launcherevents.ReasoningPayload{Text: text, Delta: false}, line)}

	case "command_execution":
		if !completed {
			return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindCommandExec, launcherevents.CommandExecPayload{
				Cmd:    rawStringSlice(item, "command"),
				CWD:    rawString(item, "cwd"),
				CallID: itemID,
			}, line)}
		}
		ok := true
		if code, hasCode := rawInt(item, "exit_code"); hasCode {
			ok = code == 0
		}
		var out []launcherevents.Event
		if output := rawString(item, "aggregated_output"); output != "" {
			out = append(out, m.makeEvent(seq, launcherevents.KindCommandOutput, launcherevents.CommandOutputPayload{
				CallID: itemID,
				Stream: "stdout",
				Data:   output,
			}, line))
		}
		out = append(out, m.makeEvent(seq, launcherevents.KindToolResult, launcherevents.ToolResultPayload{
			CallID: itemID,
			OK:     ok,
			Result: append(json.RawMessage(nil), line...),
		}, line))
		return out

	case "mcp_tool_call":
		if !completed {
			return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindToolCall, launcherevents.ToolCallPayload{
				Name:   rawString(item, "tool"),
				CallID: itemID,
				Args:   cloneRaw(item["arguments"]),
			}, line)}
		}
		result := cloneRaw(item["result"])
		if len(result) == 0 {
			result = append(json.RawMessage(nil), line...)
		}
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindToolResult, launcherevents.ToolResultPayload{
			CallID: itemID,
			OK:     rawString(item, "status") != "failed" && !rawBool(item, "is_error"),
			Result: result,
		}, line)}

	default:
		return []launcherevents.Event{m.makeEvent(seq, launcherevents.KindLog, launcherevents.LogPayload{Stream: "stdout", Line: string(line)}, line)}
	}
}

func (m *codexEventMapper) taskCompleteObserved() bool {
	return m.turnCompleteSaw
}

func (m *codexEventMapper) makeEvent(seq *uint64, kind launcherevents.EventKind, payload any, raw []byte) launcherevents.Event {
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

func rawString(raw map[string]json.RawMessage, key string) string {
	if raw == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw[key], &s); err == nil {
		return s
	}
	return ""
}

func rawStringSlice(raw map[string]json.RawMessage, key string) []string {
	if raw == nil {
		return nil
	}
	var values []string
	if err := json.Unmarshal(raw[key], &values); err == nil {
		return values
	}
	var single string
	if err := json.Unmarshal(raw[key], &single); err == nil && single != "" {
		return []string{single}
	}
	return nil
}

func rawInt(raw map[string]json.RawMessage, key string) (int, bool) {
	if raw == nil {
		return 0, false
	}
	var value int
	if err := json.Unmarshal(raw[key], &value); err != nil {
		return 0, false
	}
	return value, true
}

func rawIntValue(raw map[string]json.RawMessage, key string) int {
	value, _ := rawInt(raw, key)
	return value
}

func rawBool(raw map[string]json.RawMessage, key string) bool {
	if raw == nil {
		return false
	}
	var value bool
	if err := json.Unmarshal(raw[key], &value); err != nil {
		return false
	}
	return value
}

func rawObject(raw map[string]json.RawMessage, key string) map[string]json.RawMessage {
	if raw == nil {
		return nil
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw[key], &value); err != nil {
		return nil
	}
	return value
}

func codexTurnUsage(raw map[string]json.RawMessage) *launcherevents.TokenUsagePayload {
	if len(raw) == 0 {
		return nil
	}
	payload := &launcherevents.TokenUsagePayload{
		Input:  rawIntValue(raw, "input_tokens"),
		Output: rawIntValue(raw, "output_tokens"),
		Cached: rawIntValue(raw, "cached_input_tokens"),
	}
	payload.Total = payload.Input + payload.Output
	return payload
}

func cloneRaw(data []byte) json.RawMessage {
	if len(data) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), data...)
}

func rawPatchChanges(raw map[string]json.RawMessage) []launcherevents.FileChangePayload {
	diff := firstNonEmpty(rawString(raw, "patch"), rawString(raw, "diff"))
	if diff != "" {
		if changes := splitPatch(diff); len(changes) > 0 {
			return changes
		}
	}

	var files []struct {
		Path       string `json:"path"`
		ChangeKind string `json:"change_kind"`
	}
	if err := json.Unmarshal(raw["files"], &files); err == nil && len(files) > 0 {
		out := make([]launcherevents.FileChangePayload, 0, len(files))
		for _, file := range files {
			changeKind := file.ChangeKind
			if changeKind == "" {
				changeKind = "modify"
			}
			out = append(out, launcherevents.FileChangePayload{Path: file.Path, ChangeKind: changeKind, Diff: diff})
		}
		return out
	}

	paths := rawStringSlice(raw, "paths")
	if len(paths) == 0 {
		if path := rawString(raw, "path"); path != "" {
			paths = []string{path}
		}
	}
	if len(paths) == 0 {
		return nil
	}
	out := make([]launcherevents.FileChangePayload, 0, len(paths))
	for _, path := range paths {
		out = append(out, launcherevents.FileChangePayload{Path: path, ChangeKind: "modify", Diff: diff})
	}
	return out
}

func splitPatch(diff string) []launcherevents.FileChangePayload {
	diff = strings.TrimSpace(diff)
	if diff == "" {
		return nil
	}

	var out []launcherevents.FileChangePayload
	sections := strings.Split(diff, "diff --git ")
	for _, section := range sections {
		section = strings.TrimSpace(section)
		if section == "" {
			continue
		}
		patch := "diff --git " + section
		lines := strings.Split(section, "\n")
		path := ""
		changeKind := "modify"
		if len(lines) > 0 {
			fields := strings.Fields(lines[0])
			if len(fields) >= 2 {
				path = strings.TrimPrefix(fields[1], "b/")
				if path == fields[1] {
					path = strings.TrimPrefix(fields[len(fields)-1], "b/")
				}
			}
		}
		for _, line := range lines {
			switch {
			case strings.HasPrefix(line, "new file mode"):
				changeKind = "create"
			case strings.HasPrefix(line, "deleted file mode"):
				changeKind = "delete"
			case strings.HasPrefix(line, "+++ b/"):
				path = strings.TrimPrefix(line, "+++ b/")
			case strings.HasPrefix(line, "--- /dev/null"):
				changeKind = "create"
			case strings.HasPrefix(line, "+++ /dev/null"):
				changeKind = "delete"
			}
		}
		if path == "" {
			continue
		}
		out = append(out, launcherevents.FileChangePayload{Path: path, ChangeKind: changeKind, Diff: patch})
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
