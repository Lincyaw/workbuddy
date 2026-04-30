package hooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// commandKillGrace mirrors internal/supervisor/agent.go: SIGTERM, 2s grace,
// then SIGKILL. Implemented via exec.Cmd.WaitDelay so the kernel-side
// enforcement matches the supervisor's cancel path even if the child traps
// SIGTERM.
const commandKillGrace = 2 * time.Second

// CommandAction runs an argv (no shell) with the v1 payload on stdin.
//
// Cancellation discipline: ctx cancellation triggers cmd.Cancel which sends
// SIGTERM. After WaitDelay (commandKillGrace) os/exec force-kills the process.
// Any non-zero exit (including kill-by-signal) is reported as failure.
type CommandAction struct {
	name string
	argv []string
	cwd  string
}

// Type implements Action.
func (c *CommandAction) Type() string { return ActionTypeCommand }

// Execute runs the command and discards stdout/stderr. For captured runs
// (e.g. `workbuddy hooks test`), use Run.
func (c *CommandAction) Execute(ctx context.Context, ev Event, payload []byte) error {
	return c.run(ctx, ev, payload, io.Discard, io.Discard).Err
}

// CommandResult is the outcome of a single Run invocation, used by the
// `hooks test` CLI to render stdout / stderr / exit / duration.
type CommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Duration time.Duration
	// Err is non-nil for any failure (non-zero exit, signal, exec error,
	// timeout). nil only on a clean exit code 0.
	Err error
}

// Run executes the command with stdout/stderr buffered for inspection.
func (c *CommandAction) Run(ctx context.Context, ev Event, payload []byte) CommandResult {
	var stdout, stderr bytes.Buffer
	res := c.run(ctx, ev, payload, &stdout, &stderr)
	res.Stdout = stdout.Bytes()
	res.Stderr = stderr.Bytes()
	return res
}

// Capture implements CapturingAction so the dispatcher can record stdout /
// stderr previews in the per-hook invocation timeline.
func (c *CommandAction) Capture(ctx context.Context, ev Event, payload []byte) ActionCapture {
	res := c.Run(ctx, ev, payload)
	return ActionCapture{
		Stdout: res.Stdout,
		Stderr: res.Stderr,
		Err:    res.Err,
	}
}

func (c *CommandAction) run(ctx context.Context, ev Event, payload []byte, stdout, stderr io.Writer) CommandResult {
	start := time.Now()
	if len(c.argv) == 0 {
		return CommandResult{Err: errors.New("hooks: command argv is empty")}
	}

	cmd := exec.CommandContext(ctx, c.argv[0], c.argv[1:]...)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if c.cwd != "" {
		cmd.Dir = c.cwd
	}
	cmd.Env = commandEnv(c.name, ev.Type, ev.Repo, ev.IssueNum)
	// Custom cancel: SIGTERM first; WaitDelay forces SIGKILL after grace.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = commandKillGrace

	err := cmd.Run()
	res := CommandResult{
		Duration: time.Since(start),
		Err:      err,
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	return res
}

// BuildCommandAction is exported for use by `hooks test` (which needs to
// reconstruct an action from a Hook entry).
func BuildCommandAction(h *Hook) (*CommandAction, error) {
	if len(h.Action.Cmd) == 0 {
		return nil, fmt.Errorf("hooks: hook %q: command.cmd must be a non-empty argv", h.Name)
	}
	if strings.TrimSpace(h.Action.Cmd[0]) == "" {
		return nil, fmt.Errorf("hooks: hook %q: command.cmd[0] (program) is empty", h.Name)
	}
	return &CommandAction{
		name: h.Name,
		argv: append([]string(nil), h.Action.Cmd...),
		cwd:  h.Action.Cwd,
	}, nil
}

func buildCommandAction(h *Hook) (Action, []string, error) {
	a, err := BuildCommandAction(h)
	if err != nil {
		return nil, nil, err
	}
	return a, nil, nil
}

func finalizeCommandAction(h *Hook) ([]string, error) {
	if _, err := BuildCommandAction(h); err != nil {
		return nil, err
	}
	return nil, nil
}

// commandEnv builds the child env: the coordinator process env plus the four
// WORKBUDDY_* introspection variables documented in the design doc.
func commandEnv(hookName, eventType, repo string, issueNum int) []string {
	base := os.Environ()
	base = append(base,
		"WORKBUDDY_EVENT_TYPE="+eventType,
		"WORKBUDDY_REPO="+repo,
		"WORKBUDDY_ISSUE_NUM="+strconv.Itoa(issueNum),
		"WORKBUDDY_HOOK_NAME="+hookName,
	)
	return base
}
