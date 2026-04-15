package launcher

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

type sessionFinder func(repoPath string) string

type processSession struct {
	runtimeName string
	agent       *config.AgentConfig
	task        *TaskContext
	findSession sessionFinder
}

func newProcessSession(runtimeName string, agent *config.AgentConfig, task *TaskContext, findSession sessionFinder) Session {
	return &processSession{runtimeName: runtimeName, agent: agent, task: task, findSession: findSession}
}

func (s *processSession) Run(ctx context.Context, events chan<- launcherevents.Event) (*Result, error) {
	timeout := s.agent.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, err := s.buildCommand(execCtx)
	if err != nil {
		return nil, err
	}
	if s.task.WorkDir != "" {
		cmd.Dir = s.task.WorkDir
	}
	cmd.Env = append(os.Environ(), buildEnvVars(s.task)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	var seq uint64
	emitEvent(events, &seq, s.task.Session.ID, s.task.Session.ID, launcherevents.KindTurnStarted, launcherevents.TurnStartedPayload{TurnID: s.task.Session.ID}, nil)
	runErr := cmd.Run()
	duration := time.Since(start)
	exitCode := 0
	status := "ok"
	if runErr != nil {
		status = "error"
		if execCtx.Err() == context.DeadlineExceeded {
			result := &Result{ExitCode: -1, Stdout: stdout.String(), Stderr: stderr.String(), Duration: duration, Meta: map[string]string{"timeout": "true"}}
			emitEvent(events, &seq, s.task.Session.ID, s.task.Session.ID, launcherevents.KindError, launcherevents.ErrorPayload{Code: "timeout", Message: execCtx.Err().Error(), Recoverable: false}, nil)
			emitEvent(events, &seq, s.task.Session.ID, s.task.Session.ID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: s.task.Session.ID, Status: "error"}, nil)
			return result, fmt.Errorf("launcher: %s: timeout after %s: %w", s.runtimeName, timeout, execCtx.Err())
		}
		if ctx.Err() != nil {
			result := &Result{ExitCode: -1, Stdout: stdout.String(), Stderr: stderr.String(), Duration: duration}
			emitEvent(events, &seq, s.task.Session.ID, s.task.Session.ID, launcherevents.KindError, launcherevents.ErrorPayload{Code: "cancelled", Message: ctx.Err().Error(), Recoverable: false}, nil)
			emitEvent(events, &seq, s.task.Session.ID, s.task.Session.ID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: s.task.Session.ID, Status: "interrupted"}, nil)
			return result, fmt.Errorf("launcher: %s: cancelled: %w", s.runtimeName, ctx.Err())
		}
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			emitEvent(events, &seq, s.task.Session.ID, s.task.Session.ID, launcherevents.KindError, launcherevents.ErrorPayload{Code: "exec", Message: runErr.Error(), Recoverable: false}, nil)
			emitEvent(events, &seq, s.task.Session.ID, s.task.Session.ID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: s.task.Session.ID, Status: "error"}, nil)
			return nil, fmt.Errorf("launcher: %s: exec: %w", s.runtimeName, runErr)
		}
	}

	meta := parseMeta(stdout.String())
	var sessionPath string
	if s.findSession != nil {
		sessionPath = s.findSession(s.sessionLookupPath())
	}
	if stderr.Len() > 0 {
		emitEvent(events, &seq, s.task.Session.ID, s.task.Session.ID, launcherevents.KindLog, launcherevents.LogPayload{Stream: "stderr", Line: strings.TrimRight(stderr.String(), "\n")}, nil)
	}
	emitEvent(events, &seq, s.task.Session.ID, s.task.Session.ID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: s.task.Session.ID, Status: status}, nil)

	return &Result{ExitCode: exitCode, Stdout: stdout.String(), Stderr: stderr.String(), Duration: duration, Meta: meta, SessionPath: sessionPath}, nil
}

func (s *processSession) buildCommand(execCtx context.Context) (*exec.Cmd, error) {
	if isClaudeRuntime(s.runtimeName) && strings.TrimSpace(s.agent.Prompt) != "" {
		prompt, err := renderCommandRaw(s.agent.Prompt, s.task)
		if err != nil {
			return nil, err
		}
		return newClaudePromptCommand(execCtx, prompt, nil, s.agent.Policy), nil
	}

	rendered, err := renderCommand(s.agent.Command, s.task)
	if err != nil {
		return nil, err
	}
	if isClaudeRuntime(s.runtimeName) {
		if _, args, ok := extractPrompt(rendered); ok {
			rawRendered, rawErr := renderCommandRaw(s.agent.Command, s.task)
			if rawErr != nil {
				return nil, rawErr
			}
			rawPrompt, _, _ := extractPrompt(rawRendered)
			return newClaudePromptCommand(execCtx, rawPrompt, args, s.agent.Policy), nil
		}
	}
	return exec.CommandContext(execCtx, "sh", "-c", rendered), nil
}

func newClaudePromptCommand(execCtx context.Context, prompt string, extraArgs []string, policy config.PolicyConfig) *exec.Cmd {
	args := append([]string{}, extraArgs...)
	args = append(args, claudePolicyArgs(policy)...)
	args = append(args, "--print")
	cmd := exec.CommandContext(execCtx, "claude", args...)
	cmd.Stdin = strings.NewReader(prompt)
	return cmd
}

func claudePolicyArgs(policy config.PolicyConfig) []string {
	args := []string{"--dangerously-skip-permissions"}
	if policy.Model != "" {
		args = append(args, "--model", policy.Model)
	}
	return args
}

func isClaudeRuntime(runtimeName string) bool {
	return runtimeName == config.RuntimeClaudeCode || runtimeName == config.RuntimeClaudeShot
}

func (s *processSession) sessionLookupPath() string {
	if s.task.WorkDir != "" {
		return s.task.WorkDir
	}
	return s.task.Repo
}

func (s *processSession) SetApprover(Approver) error { return ErrNotSupported }

func (s *processSession) Close() error { return nil }

func emitEvent(ch chan<- launcherevents.Event, seq *uint64, sessionID, turnID string, kind launcherevents.EventKind, payload any, raw []byte) {
	if ch == nil {
		return
	}
	*seq = *seq + 1
	var rawMsg []byte
	if len(raw) > 0 {
		rawMsg = append(rawMsg, raw...)
	}
	ch <- launcherevents.Event{Kind: kind, Timestamp: time.Now().UTC(), SessionID: sessionID, TurnID: turnID, Seq: *seq, Payload: launcherevents.MustPayload(payload), Raw: rawMsg}
}
