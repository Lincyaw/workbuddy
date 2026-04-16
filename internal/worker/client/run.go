package client

import (
	"context"
	"fmt"
	"time"

	"github.com/Lincyaw/workbuddy/internal/launcher"
)

type Executor interface {
	Execute(ctx context.Context, task *Task) (*launcher.Result, error)
}

type LauncherExecutor struct {
	Launcher *launcher.Launcher
}

func (e LauncherExecutor) Execute(ctx context.Context, task *Task) (*launcher.Result, error) {
	if e.Launcher == nil {
		return nil, fmt.Errorf("worker client: launcher is required")
	}
	agent := task.Agent
	taskCtx := task.Context
	return e.Launcher.Launch(ctx, &agent, &taskCtx)
}

func (c *Client) Run(ctx context.Context, exec Executor) error {
	if exec == nil {
		return fmt.Errorf("worker client: executor is required")
	}

	backoff := newBackoff(c.backoffInitial, c.backoffMax)
	registered := false

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		if !registered {
			if err := c.Register(ctx); err != nil {
				if sleepErr := sleepContext(ctx, backoff.Next()); sleepErr != nil {
					return sleepErr
				}
				continue
			}
			registered = true
			backoff.Reset()
		}

		task, err := c.PollTask(ctx)
		if err != nil {
			registered = false
			if sleepErr := sleepContext(ctx, backoff.Next()); sleepErr != nil {
				return sleepErr
			}
			continue
		}
		backoff.Reset()
		if task == nil {
			continue
		}

		if err := c.retryTaskOperation(ctx, func(opCtx context.Context) error { return c.Ack(opCtx, task.ID) }); err != nil {
			return err
		}

		result, execErr := c.executeTask(ctx, exec, task)
		if err := c.retryTaskOperation(ctx, func(opCtx context.Context) error {
			return c.SubmitResult(opCtx, task.ID, resultFromLauncher(result, execErr))
		}); err != nil {
			return err
		}
	}
}

func (c *Client) executeTask(ctx context.Context, exec Executor, task *Task) (*launcher.Result, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(c.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				_ = c.Heartbeat(runCtx, task.ID)
			}
		}
	}()

	result, err := exec.Execute(runCtx, task)
	cancel()
	<-done
	return result, err
}

func (c *Client) retryTaskOperation(ctx context.Context, fn func(context.Context) error) error {
	backoff := newBackoff(c.backoffInitial, c.backoffMax)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(ctx); err == nil {
			return nil
		}
		if err := sleepContext(ctx, backoff.Next()); err != nil {
			return err
		}
	}
}

func resultFromLauncher(result *launcher.Result, execErr error) ExecutionResult {
	out := ExecutionResult{}
	if result != nil {
		out.ExitCode = result.ExitCode
		out.Stdout = result.Stdout
		out.Stderr = result.Stderr
		out.DurationMS = result.Duration.Milliseconds()
		out.Meta = cloneStringMap(result.Meta)
		out.SessionPath = result.SessionPath
		out.RawSessionPath = result.RawSessionPath
		out.LastMessage = result.LastMessage
		out.SessionRef = result.SessionRef
		out.TokenUsage = result.TokenUsage
	}
	if execErr != nil {
		out.Error = execErr.Error()
		if result == nil {
			out.ExitCode = 1
		}
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

type expBackoff struct {
	current time.Duration
	initial time.Duration
	max     time.Duration
}

func newBackoff(initial, max time.Duration) *expBackoff {
	return &expBackoff{initial: initial, max: max}
}

func (b *expBackoff) Next() time.Duration {
	if b.current <= 0 {
		b.current = b.initial
		return b.current
	}
	next := b.current * 2
	if next > b.max {
		next = b.max
	}
	b.current = next
	return b.current
}

func (b *expBackoff) Reset() {
	b.current = 0
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
