package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	"github.com/Lincyaw/workbuddy/internal/supervisor"
	supclient "github.com/Lincyaw/workbuddy/internal/supervisor/client"
)

type SessionFinder func(repoPath string) string

// AgentStartedHook is invoked exactly once per session, immediately after the
// supervisor has accepted POST /agents and returned an agent_id. Workers wire
// this to Store.UpdateTaskSupervisorAgentID so a worker crash mid-run leaves
// the task row pointing at the live supervisor-managed subprocess.
type AgentStartedHook func(taskID, agentID string)

type ProcessSession struct {
	RuntimeName    string
	Agent          *config.AgentConfig
	Task           *TaskContext
	FindSession    SessionFinder
	Client         *supclient.Client
	OnAgentStarted AgentStartedHook
}

// NewProcessSession returns a Session that runs a workbuddy claude-style
// agent through the local supervisor IPC API. The returned session does not
// start the subprocess until Run is called; onAgentStarted (if non-nil) is
// invoked exactly once after the supervisor accepts POST /agents.
func NewProcessSession(client *supclient.Client, onAgentStarted AgentStartedHook, runtimeName string, agent *config.AgentConfig, task *TaskContext, findSession SessionFinder) Session {
	return &ProcessSession{
		RuntimeName:    runtimeName,
		Agent:          agent,
		Task:           task,
		FindSession:    findSession,
		Client:         client,
		OnAgentStarted: onAgentStarted,
	}
}

// CommandSpec describes a runnable command independent of os/exec. It is the
// IPC-shaped equivalent of the previous exec.Cmd construction: a binary plus
// argv plus optional stdin payload. The supervisor executes it on the
// caller's behalf so the worker process can exit without orphaning the
// subprocess.
type CommandSpec struct {
	Binary string
	Args   []string
	Stdin  string
}

func (s *ProcessSession) Run(ctx context.Context, events chan<- launcherevents.Event) (*Result, error) {
	timeout := s.Agent.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	if s.WantsClaudeStreamJSON() {
		return s.runClaudeStream(ctx, timeout, events)
	}
	if s.Client == nil {
		infra := &Result{ExitCode: -1, Meta: map[string]string{}}
		MarkInfraFailure(infra, "supervisor client not configured")
		return infra, fmt.Errorf("runtime: %s: supervisor client not configured", s.RuntimeName)
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	spec, err := s.BuildSpec()
	if err != nil {
		infra := &Result{ExitCode: -1, Meta: map[string]string{}}
		MarkInfraFailure(infra, "build command failed (template render)")
		return infra, err
	}

	start := time.Now()
	var seq uint64
	EmitPermissionEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, s.Agent, EmitEvent)
	EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindTurnStarted, launcherevents.TurnStartedPayload{TurnID: s.Task.Session.ID}, nil)

	startResp, err := s.Client.StartAgent(execCtx, s.startRequestFor(spec))
	if err != nil {
		infra := &Result{ExitCode: -1, Meta: map[string]string{}}
		reason := "supervisor StartAgent failed"
		if isExecStartFailure(err) {
			reason = "exec start error (binary not runnable)"
		}
		MarkInfraFailure(infra, reason)
		EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindError, launcherevents.ErrorPayload{Code: "exec", Message: err.Error(), Recoverable: false}, nil)
		EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: s.Task.Session.ID, Status: "error"}, nil)
		return infra, fmt.Errorf("runtime: %s: start: %w", s.RuntimeName, err)
	}
	agentID := startResp.AgentID
	if s.OnAgentStarted != nil && s.Task != nil && s.Task.Session.TaskID != "" {
		s.OnAgentStarted(s.Task.Session.TaskID, agentID)
	}

	// Watch ctx and the timeout, sending Cancel to the supervisor so the
	// agent process is reaped; the StreamEvents call returns naturally
	// once the supervisor observes the exit.
	cancelDone := make(chan struct{})
	go s.watchCancel(execCtx, agentID, cancelDone)
	defer close(cancelDone)

	stdoutBuf, streamErr := collectStdoutLines(execCtx, s.Client, agentID, nil)
	duration := time.Since(start)

	status, statusErr := s.Client.Status(context.Background(), agentID)
	exitCode := 0
	if statusErr != nil {
		infra := &Result{ExitCode: -1, Stdout: stdoutBuf.String(), Duration: duration, Meta: map[string]string{}}
		MarkInfraFailure(infra, "supervisor status lookup failed")
		return infra, fmt.Errorf("runtime: %s: status: %w", s.RuntimeName, statusErr)
	}
	stderrBytes := readFileIfExists(status.StderrPath)
	stdoutBytes := readFileIfExists(status.StdoutPath)
	if stdoutBytes != nil {
		stdoutBuf.Reset()
		stdoutBuf.Write(stdoutBytes)
	}

	if execCtx.Err() == context.DeadlineExceeded {
		EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindError, launcherevents.ErrorPayload{Code: "timeout", Message: execCtx.Err().Error(), Recoverable: false}, nil)
		EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: s.Task.Session.ID, Status: "error"}, nil)
		result := &Result{ExitCode: -1, Stdout: stdoutBuf.String(), Stderr: string(stderrBytes), Duration: duration, Meta: map[string]string{"timeout": "true"}}
		return result, fmt.Errorf("runtime: %s: timeout after %s: %w", s.RuntimeName, timeout, execCtx.Err())
	}
	if ctx.Err() != nil {
		EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindError, launcherevents.ErrorPayload{Code: "cancelled", Message: ctx.Err().Error(), Recoverable: false}, nil)
		EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: s.Task.Session.ID, Status: "interrupted"}, nil)
		result := &Result{ExitCode: -1, Stdout: stdoutBuf.String(), Stderr: string(stderrBytes), Duration: duration}
		return result, fmt.Errorf("runtime: %s: cancelled: %w", s.RuntimeName, ctx.Err())
	}
	if streamErr != nil {
		infra := &Result{ExitCode: -1, Stdout: stdoutBuf.String(), Stderr: string(stderrBytes), Duration: duration, Meta: map[string]string{}}
		MarkInfraFailure(infra, "supervisor stream read error")
		return infra, fmt.Errorf("runtime: %s: stream: %w", s.RuntimeName, streamErr)
	}
	if status.ExitCode != nil {
		exitCode = *status.ExitCode
	}

	statusName := "ok"
	if exitCode != 0 {
		statusName = "error"
	}

	meta := ParseMeta(stdoutBuf.String())
	var sessionPath string
	if s.FindSession != nil {
		sessionPath = s.FindSession(s.SessionLookupPath())
	}
	if handle := s.Task.SessionHandle(); handle != nil {
		_ = handle.WriteStdout(stdoutBuf.Bytes())
		if len(stderrBytes) > 0 {
			_ = handle.WriteStderr(stderrBytes)
		}
	}
	if len(stderrBytes) > 0 {
		EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindLog, launcherevents.LogPayload{Stream: "stderr", Line: strings.TrimRight(string(stderrBytes), "\n")}, nil)
	}

	result := &Result{ExitCode: exitCode, Stdout: stdoutBuf.String(), Stderr: string(stderrBytes), Duration: duration, Meta: meta, SessionPath: sessionPath}
	if exitCode != 0 && strings.TrimSpace(stdoutBuf.String()) == "" && StderrLooksLikeRuntimePanic(string(stderrBytes)) {
		MarkInfraFailure(result, "runtime panic/abort before agent output")
	}
	if err := ValidateResolvedOutputContract(s.Agent, s.Task, result); err != nil {
		EmitOutputContractFailure(events, &seq, s.Task.Session.ID, s.Task.Session.ID, err, EmitEvent)
		return result, err
	}
	EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: s.Task.Session.ID, Status: statusName}, nil)
	return result, nil
}

// watchCancel observes execCtx and, on cancellation/deadline, sends Cancel to
// the supervisor so the subprocess is signalled; doneCh closes when the
// caller is finished and we should stop watching.
func (s *ProcessSession) watchCancel(execCtx context.Context, agentID string, doneCh <-chan struct{}) {
	select {
	case <-execCtx.Done():
		// Use a fresh context for the cancel call so the cancel itself
		// isn't aborted by the very ctx we're reacting to.
		cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.Client.Cancel(cancelCtx, agentID)
		cancel()
	case <-doneCh:
	}
}

// startRequestFor packages the spec + task env/workdir into an IPC request.
func (s *ProcessSession) startRequestFor(spec *CommandSpec) supervisor.StartAgentRequest {
	envSlice := BuildScopedEnv(s.Agent, s.Task)
	envMap := make(map[string]string, len(envSlice))
	for _, kv := range envSlice {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		envMap[kv[:eq]] = kv[eq+1:]
	}
	workdir := ""
	if s.Task != nil && s.Task.WorkDir != "" {
		workdir = s.Task.WorkDir
	}
	sessionID := ""
	if s.Task != nil {
		sessionID = s.Task.Session.ID
	}
	return supervisor.StartAgentRequest{
		Runtime:   spec.Binary,
		Args:      append([]string{}, spec.Args...),
		Workdir:   workdir,
		Env:       envMap,
		SessionID: sessionID,
		Stdin:     spec.Stdin,
	}
}

// BuildSpec is the IPC-shaped replacement for the previous BuildCommand:
// it returns the binary, argv, and optional stdin to hand to the supervisor.
func (s *ProcessSession) BuildSpec() (*CommandSpec, error) {
	rolloutArgs := rolloutInvocationArgs(s.Task)
	if promptBody := ResolvePromptBody(s.Agent, s.Task); IsClaudeRuntime(s.RuntimeName) && strings.TrimSpace(promptBody) != "" {
		prompt, err := RenderAgentPrompt(promptBody, s.Task)
		if err != nil {
			return nil, err
		}
		return claudePromptSpec(prompt, rolloutArgs, s.Agent.Policy), nil
	}

	rendered, err := RenderCommand(s.Agent.Command, s.Task)
	if err != nil {
		return nil, err
	}
	if IsClaudeRuntime(s.RuntimeName) {
		if _, args, ok := ExtractPrompt(rendered); ok {
			rawRendered, rawErr := RenderCommandRaw(s.Agent.Command, s.Task)
			if rawErr != nil {
				return nil, rawErr
			}
			rawPrompt, _, _ := ExtractPrompt(rawRendered)
			mergedArgs := append(append([]string{}, rolloutArgs...), args...)
			return claudePromptSpec(rawPrompt, mergedArgs, s.Agent.Policy), nil
		}
	}
	return &CommandSpec{Binary: "sh", Args: []string{"-c", rendered}}, nil
}

// claudePromptSpec mirrors the argument list the previous exec.Cmd path built
// for `claude --print --output-format stream-json --verbose` invocations,
// reading the prompt from stdin.
func claudePromptSpec(prompt string, extraArgs []string, policy config.PolicyConfig) *CommandSpec {
	args := append([]string{}, extraArgs...)
	args = append(args, ClaudePolicyArgs(policy)...)
	if !HasPrintFlag(args) {
		args = append(args, "--print")
	}
	args = append(args, "--output-format", "stream-json")
	if !HasVerboseFlag(args) {
		args = append(args, "--verbose")
	}
	return &CommandSpec{Binary: "claude", Args: args, Stdin: prompt}
}

func HasPrintFlag(args []string) bool {
	for _, a := range args {
		if a == "-p" || a == "--print" {
			return true
		}
	}
	return false
}

func HasVerboseFlag(args []string) bool {
	for _, a := range args {
		if a == "--verbose" {
			return true
		}
	}
	return false
}

func ClaudePolicyArgs(policy config.PolicyConfig) []string {
	var args []string
	if policy.Sandbox == "danger-full-access" {
		args = append(args, "--dangerously-skip-permissions")
	}
	return args
}

func IsClaudeRuntime(runtimeName string) bool {
	return runtimeName == config.RuntimeClaudeCode || runtimeName == config.RuntimeClaudeShot
}

func (s *ProcessSession) WantsClaudeStreamJSON() bool {
	if !IsClaudeRuntime(s.RuntimeName) {
		return false
	}
	if strings.TrimSpace(s.Agent.Prompt) != "" {
		return true
	}
	rendered, err := RenderCommand(s.Agent.Command, s.Task)
	if err != nil {
		return false
	}
	_, _, ok := ExtractPrompt(rendered)
	return ok
}

func (s *ProcessSession) SessionLookupPath() string {
	if strings.TrimSpace(s.Task.Repo) != "" {
		return s.Task.Repo
	}
	return s.Task.WorkDir
}

func (s *ProcessSession) SetApprover(Approver) error { return ErrNotSupported }

func (s *ProcessSession) Close() error { return nil }

// collectStdoutLines streams the supervisor SSE feed and accumulates each
// line, separated by '\n', into the returned buffer. The optional onLine
// callback receives each line for callers (claude_stream) that need to map
// events on the fly. Returns whatever was collected even on error.
func collectStdoutLines(ctx context.Context, client *supclient.Client, agentID string, onLine func(supclient.StreamEvent) error) (bytes.Buffer, error) {
	var buf bytes.Buffer
	err := client.StreamEvents(ctx, agentID, 0, func(ev supclient.StreamEvent) error {
		buf.WriteString(ev.Line)
		buf.WriteByte('\n')
		if onLine != nil {
			return onLine(ev)
		}
		return nil
	})
	return buf, err
}

func readFileIfExists(path string) []byte {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

func isExecStartFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, supclient.ErrAgentNotFound) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "exec format error") ||
		strings.Contains(msg, "no such file") ||
		strings.Contains(msg, "executable file not found") ||
		strings.Contains(msg, "permission denied")
}
