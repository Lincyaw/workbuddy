package launcher

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

func newCodexSession(agent *config.AgentConfig, task *TaskContext, prompt string) *codexSession {
	baseDir := task.WorkDir
	if baseDir == "" {
		baseDir = "."
	}
	artifactDir := filepath.Join(baseDir, ".workbuddy", "sessions", task.Session.ID)
	return &codexSession{
		agent:       agent,
		task:        task,
		prompt:      prompt,
		lastMsgPath: filepath.Join(artifactDir, "codex-last-message.txt"),
		stdoutPath:  filepath.Join(artifactDir, "codex-events.jsonl"),
	}
}

func (s *codexSession) Run(ctx context.Context, events chan<- launcherevents.Event) (*Result, error) {
	if s.cachedResult != nil {
		return s.cachedResult, nil
	}
	if err := os.MkdirAll(filepath.Dir(s.stdoutPath), 0o755); err != nil {
		return nil, fmt.Errorf("launcher: codex-exec: create artifact dir: %w", err)
	}

	timeout := s.agent.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := s.buildArgs()
	cmd := exec.CommandContext(execCtx, "codex", args...)
	if s.task.WorkDir != "" {
		cmd.Dir = s.task.WorkDir
	}
	cmd.Env = append(os.Environ(), buildEnvVars(s.task)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("launcher: codex-exec: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("launcher: codex-exec: stderr pipe: %w", err)
	}

	stdoutFile, err := os.Create(s.stdoutPath)
	if err != nil {
		return nil, fmt.Errorf("launcher: codex-exec: create stdout artifact: %w", err)
	}
	defer func() { _ = stdoutFile.Close() }()

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launcher: codex-exec: start: %w", err)
	}
	start := time.Now()

	var stdoutBuf, stderrBuf bytes.Buffer
	var seq uint64
	mapper := newCodexEventMapper(s.task.Session.ID)
	var wg sync.WaitGroup
	var scanErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdoutPipe)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			stdoutBuf.WriteString(line)
			stdoutBuf.WriteByte('\n')
			_, _ = stdoutFile.WriteString(line + "\n")
			for _, evt := range mapper.Map([]byte(line), &seq) {
				if events != nil {
					events <- evt
				}
			}
		}
		if err := scanner.Err(); err != nil {
			scanErr = err
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = stderrBuf.ReadFrom(stderrPipe)
	}()

	runErr := cmd.Wait()
	wg.Wait()
	duration := time.Since(start)
	if scanErr != nil && !strings.Contains(scanErr.Error(), "file already closed") {
		return nil, fmt.Errorf("launcher: codex-exec: read stdout: %w", scanErr)
	}

	exitCode := 0
	if runErr != nil {
		if execCtx.Err() == context.DeadlineExceeded {
			result := &Result{ExitCode: -1, Stdout: stdoutBuf.String(), Stderr: stderrBuf.String(), Duration: duration, Meta: map[string]string{"timeout": "true"}, SessionPath: s.stdoutPath, SessionRef: mapper.sessionRef, TokenUsage: mapper.tokenUsage}
			if events != nil {
				emitEvent(events, &seq, s.task.Session.ID, mapper.turnID, launcherevents.KindError, launcherevents.ErrorPayload{Code: "timeout", Message: execCtx.Err().Error(), Recoverable: false}, nil)
				emitEvent(events, &seq, s.task.Session.ID, mapper.turnID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: mapper.effectiveTurnID(), Status: "error"}, nil)
			}
			return result, fmt.Errorf("launcher: codex-exec: timeout after %s: %w", timeout, execCtx.Err())
		}
		if ctx.Err() != nil {
			result := &Result{ExitCode: -1, Stdout: stdoutBuf.String(), Stderr: stderrBuf.String(), Duration: duration, SessionPath: s.stdoutPath, SessionRef: mapper.sessionRef, TokenUsage: mapper.tokenUsage}
			if events != nil {
				emitEvent(events, &seq, s.task.Session.ID, mapper.turnID, launcherevents.KindError, launcherevents.ErrorPayload{Code: "cancelled", Message: ctx.Err().Error(), Recoverable: false}, nil)
				emitEvent(events, &seq, s.task.Session.ID, mapper.turnID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: mapper.effectiveTurnID(), Status: "interrupted"}, nil)
			}
			return result, fmt.Errorf("launcher: codex-exec: cancelled: %w", ctx.Err())
		}
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("launcher: codex-exec: wait: %w", runErr)
		}
	}

	lastMessage := readOptionalFile(s.lastMsgPath)
	result := &Result{ExitCode: exitCode, Stdout: stdoutBuf.String(), Stderr: stderrBuf.String(), Duration: duration, SessionPath: s.stdoutPath, LastMessage: lastMessage, SessionRef: mapper.sessionRef, TokenUsage: mapper.tokenUsage}
	s.cachedResult = result
	if events != nil && stderrBuf.Len() > 0 {
		emitEvent(events, &seq, s.task.Session.ID, mapper.turnID, launcherevents.KindLog, launcherevents.LogPayload{Stream: "stderr", Line: strings.TrimRight(stderrBuf.String(), "\n")}, nil)
	}
	return result, nil
}

func (s *codexSession) SetApprover(Approver) error { return ErrNotSupported }
func (s *codexSession) Close() error               { return nil }

func (s *codexSession) buildArgs() []string {
	args := []string{"exec", "--skip-git-repo-check", "--json", "--output-last-message", s.lastMsgPath}
	if s.task.WorkDir != "" {
		args = append(args, "--cd", s.task.WorkDir)
	}
	if sandbox := s.agent.Policy.Sandbox; sandbox != "" {
		args = append(args, "--sandbox", sandbox)
	}
	for _, cfg := range s.approvalConfigArgs() {
		args = append(args, "-c", cfg)
	}
	if model := strings.TrimSpace(s.agent.Policy.Model); model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, s.prompt)
	return args
}

func (s *codexSession) approvalConfigArgs() []string {
	switch s.agent.Policy.Approval {
	case "never":
		return []string{"approval_policy=\"never\""}
	case "on-failure":
		return []string{"approval_policy=\"on-failure\""}
	case "on-request":
		return []string{"approval_policy=\"on-request\""}
	default:
		return nil
	}
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

type codexNativeEvent struct {
	Type   string          `json:"type"`
	Thread string          `json:"thread_id"`
	Usage  *codexUsage     `json:"usage,omitempty"`
	Item   *codexEventItem `json:"item,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type codexUsage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
}

type codexEventItem struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Text             string `json:"text,omitempty"`
	Command          string `json:"command,omitempty"`
	AggregatedOutput string `json:"aggregated_output,omitempty"`
	ExitCode         *int   `json:"exit_code,omitempty"`
	Status           string `json:"status,omitempty"`
	Title            string `json:"title,omitempty"`
	Args             any    `json:"arguments,omitempty"`
}

type codexEventMapper struct {
	sessionID  string
	sessionRef SessionRef
	turnID     string
	turnCount  int
	tokenUsage *launcherevents.TokenUsagePayload
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
	var msg codexNativeEvent
	if err := json.Unmarshal(line, &msg); err != nil {
		return []launcherevents.Event{m.makeEvent(seq, m.effectiveTurnID(), launcherevents.KindLog, launcherevents.LogPayload{Stream: "stdout", Line: string(line)}, line)}
	}

	switch msg.Type {
	case "thread.started":
		m.sessionRef = SessionRef{ID: msg.Thread, Kind: "codex-thread"}
		return nil
	case "turn.started":
		m.turnCount++
		m.turnID = fmt.Sprintf("%s-turn-%d", m.sessionID, m.turnCount)
		return []launcherevents.Event{m.makeEvent(seq, m.turnID, launcherevents.KindTurnStarted, launcherevents.TurnStartedPayload{TurnID: m.turnID}, line)}
	case "turn.completed":
		var out []launcherevents.Event
		if msg.Usage != nil {
			payload := launcherevents.TokenUsagePayload{Input: msg.Usage.InputTokens, Output: msg.Usage.OutputTokens, Cached: msg.Usage.CachedInputTokens, Total: msg.Usage.InputTokens + msg.Usage.OutputTokens}
			m.tokenUsage = &payload
			out = append(out, m.makeEvent(seq, m.effectiveTurnID(), launcherevents.KindTokenUsage, payload, line))
		}
		out = append(out, m.makeEvent(seq, m.effectiveTurnID(), launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: m.effectiveTurnID(), Status: "ok"}, line))
		return out
	case "item.started":
		return m.mapItemStarted(msg.Item, line, seq)
	case "item.completed":
		return m.mapItemCompleted(msg.Item, line, seq)
	case "error":
		message := msg.Error
		if message == "" {
			message = string(line)
		}
		return []launcherevents.Event{m.makeEvent(seq, m.effectiveTurnID(), launcherevents.KindError, launcherevents.ErrorPayload{Code: "unknown", Message: message, Recoverable: false}, line)}
	default:
		return []launcherevents.Event{m.makeEvent(seq, m.effectiveTurnID(), launcherevents.KindLog, launcherevents.LogPayload{Stream: "stdout", Line: string(line)}, line)}
	}
}

func (m *codexEventMapper) mapItemStarted(item *codexEventItem, raw []byte, seq *uint64) []launcherevents.Event {
	if item == nil {
		return []launcherevents.Event{m.makeEvent(seq, m.effectiveTurnID(), launcherevents.KindLog, launcherevents.LogPayload{Stream: "stdout", Line: string(raw)}, raw)}
	}
	switch item.Type {
	case "command_execution":
		return []launcherevents.Event{m.makeEvent(seq, m.effectiveTurnID(), launcherevents.KindCommandExec, launcherevents.CommandExecPayload{Cmd: []string{item.Command}, CallID: item.ID}, raw)}
	case "mcp_tool_call":
		args, _ := json.Marshal(item.Args)
		return []launcherevents.Event{m.makeEvent(seq, m.effectiveTurnID(), launcherevents.KindToolCall, launcherevents.ToolCallPayload{Name: item.Title, CallID: item.ID, Args: args}, raw)}
	default:
		return []launcherevents.Event{m.makeEvent(seq, m.effectiveTurnID(), launcherevents.KindLog, launcherevents.LogPayload{Stream: "stdout", Line: string(raw)}, raw)}
	}
}

func (m *codexEventMapper) mapItemCompleted(item *codexEventItem, raw []byte, seq *uint64) []launcherevents.Event {
	if item == nil {
		return []launcherevents.Event{m.makeEvent(seq, m.effectiveTurnID(), launcherevents.KindLog, launcherevents.LogPayload{Stream: "stdout", Line: string(raw)}, raw)}
	}
	switch item.Type {
	case "agent_message":
		return []launcherevents.Event{m.makeEvent(seq, m.effectiveTurnID(), launcherevents.KindAgentMessage, launcherevents.AgentMessagePayload{Text: item.Text, Delta: false, Final: true}, raw)}
	case "reasoning":
		return []launcherevents.Event{m.makeEvent(seq, m.effectiveTurnID(), launcherevents.KindReasoning, launcherevents.ReasoningPayload{Text: item.Text, Delta: false}, raw)}
	case "command_execution":
		var out []launcherevents.Event
		if item.AggregatedOutput != "" {
			out = append(out, m.makeEvent(seq, m.effectiveTurnID(), launcherevents.KindCommandOutput, launcherevents.CommandOutputPayload{CallID: item.ID, Stream: "stdout", Data: item.AggregatedOutput}, raw))
		}
		itemResult, _ := json.Marshal(item)
		ok := item.ExitCode != nil && *item.ExitCode == 0
		out = append(out, m.makeEvent(seq, m.effectiveTurnID(), launcherevents.KindToolResult, launcherevents.ToolResultPayload{CallID: item.ID, OK: ok, Result: itemResult}, raw))
		return out
	case "mcp_tool_call":
		itemResult, _ := json.Marshal(item)
		return []launcherevents.Event{m.makeEvent(seq, m.effectiveTurnID(), launcherevents.KindToolResult, launcherevents.ToolResultPayload{CallID: item.ID, OK: item.Status == "completed", Result: itemResult}, raw)}
	default:
		return []launcherevents.Event{m.makeEvent(seq, m.effectiveTurnID(), launcherevents.KindLog, launcherevents.LogPayload{Stream: "stdout", Line: string(raw)}, raw)}
	}
}

func (m *codexEventMapper) makeEvent(seq *uint64, turnID string, kind launcherevents.EventKind, payload any, raw []byte) launcherevents.Event {
	*seq = *seq + 1
	var rawMsg []byte
	if len(raw) > 0 {
		rawMsg = append(rawMsg, raw...)
	}
	return launcherevents.Event{Kind: kind, Timestamp: time.Now().UTC(), SessionID: m.sessionID, TurnID: turnID, Seq: *seq, Payload: launcherevents.MustPayload(payload), Raw: rawMsg}
}
