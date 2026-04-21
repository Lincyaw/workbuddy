package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

type SessionFinder func(repoPath string) string

type ProcessSession struct {
	RuntimeName string
	Agent       *config.AgentConfig
	Task        *TaskContext
	FindSession SessionFinder
}

func NewProcessSession(runtimeName string, agent *config.AgentConfig, task *TaskContext, findSession SessionFinder) Session {
	return &ProcessSession{RuntimeName: runtimeName, Agent: agent, Task: task, FindSession: findSession}
}

func (s *ProcessSession) Run(ctx context.Context, events chan<- launcherevents.Event) (*Result, error) {
	timeout := s.Agent.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	if s.WantsClaudeStreamJSON() {
		return s.runClaudeStream(ctx, timeout, events)
	}
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

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	var seq uint64
	EmitPermissionEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, s.Agent, EmitEvent)
	EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindTurnStarted, launcherevents.TurnStartedPayload{TurnID: s.Task.Session.ID}, nil)
	runErr := cmd.Run()
	duration := time.Since(start)
	exitCode := 0
	status := "ok"
	if runErr != nil {
		status = "error"
		if execCtx.Err() == context.DeadlineExceeded {
			result := &Result{ExitCode: -1, Stdout: stdout.String(), Stderr: stderr.String(), Duration: duration, Meta: map[string]string{"timeout": "true"}}
			EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindError, launcherevents.ErrorPayload{Code: "timeout", Message: execCtx.Err().Error(), Recoverable: false}, nil)
			EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: s.Task.Session.ID, Status: "error"}, nil)
			return result, fmt.Errorf("runtime: %s: timeout after %s: %w", s.RuntimeName, timeout, execCtx.Err())
		}
		if ctx.Err() != nil {
			result := &Result{ExitCode: -1, Stdout: stdout.String(), Stderr: stderr.String(), Duration: duration}
			EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindError, launcherevents.ErrorPayload{Code: "cancelled", Message: ctx.Err().Error(), Recoverable: false}, nil)
			EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: s.Task.Session.ID, Status: "interrupted"}, nil)
			return result, fmt.Errorf("runtime: %s: cancelled: %w", s.RuntimeName, ctx.Err())
		}
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindError, launcherevents.ErrorPayload{Code: "exec", Message: runErr.Error(), Recoverable: false}, nil)
			EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: s.Task.Session.ID, Status: "error"}, nil)
			infra := &Result{
				ExitCode: -1,
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
				Duration: duration,
				Meta:     map[string]string{},
			}
			reason := "launcher exec error"
			if IsExecStartError(runErr) {
				reason = "exec start error (binary not runnable)"
			}
			MarkInfraFailure(infra, reason)
			return infra, fmt.Errorf("runtime: %s: exec: %w", s.RuntimeName, runErr)
		}
	}

	meta := ParseMeta(stdout.String())
	var sessionPath string
	if s.FindSession != nil {
		sessionPath = s.FindSession(s.SessionLookupPath())
	}
	if handle := s.Task.SessionHandle(); handle != nil {
		_ = handle.WriteStdout(stdout.Bytes())
		if stderr.Len() > 0 {
			_ = handle.WriteStderr(stderr.Bytes())
		}
	}
	if stderr.Len() > 0 {
		EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindLog, launcherevents.LogPayload{Stream: "stderr", Line: strings.TrimRight(stderr.String(), "\n")}, nil)
	}

	result := &Result{ExitCode: exitCode, Stdout: stdout.String(), Stderr: stderr.String(), Duration: duration, Meta: meta, SessionPath: sessionPath}
	if exitCode != 0 && strings.TrimSpace(stdout.String()) == "" && StderrLooksLikeRuntimePanic(stderr.String()) {
		MarkInfraFailure(result, "runtime panic/abort before agent output")
	}
	if err := ValidateOutputContract(s.Agent, result); err != nil {
		EmitOutputContractFailure(events, &seq, s.Task.Session.ID, s.Task.Session.ID, err, EmitEvent)
		return result, err
	}
	EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: s.Task.Session.ID, Status: status}, nil)
	return result, nil
}

func (s *ProcessSession) BuildCommand(execCtx context.Context) (*exec.Cmd, error) {
	if IsClaudeRuntime(s.RuntimeName) && strings.TrimSpace(s.Agent.Prompt) != "" {
		prompt, err := RenderCommandRaw(s.Agent.Prompt, s.Task)
		if err != nil {
			return nil, err
		}
		return NewClaudePromptCommand(execCtx, prompt, nil, s.Agent.Policy), nil
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
			return NewClaudePromptCommand(execCtx, rawPrompt, args, s.Agent.Policy), nil
		}
	}
	return exec.CommandContext(execCtx, "sh", "-c", rendered), nil
}

func NewClaudePromptCommand(execCtx context.Context, prompt string, extraArgs []string, policy config.PolicyConfig) *exec.Cmd {
	args := append([]string{}, extraArgs...)
	args = append(args, ClaudePolicyArgs(policy)...)
	if !HasPrintFlag(args) {
		args = append(args, "--print")
	}
	args = append(args, "--output-format", "stream-json")
	if !HasVerboseFlag(args) {
		args = append(args, "--verbose")
	}
	cmd := exec.CommandContext(execCtx, "claude", args...)
	cmd.Stdin = strings.NewReader(prompt)
	return cmd
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
