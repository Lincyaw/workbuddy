// Package claude implements the agent.Backend interface by spawning the
// Claude CLI as a subprocess with streaming JSON output.
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Lincyaw/workbuddy/internal/agent"
	"github.com/google/uuid"
)

// Backend is a claude-code agent.Backend. It spawns one `claude` subprocess
// per session.
type Backend struct{}

// NewBackend returns a new Claude backend.
func NewBackend() *Backend { return &Backend{} }

func (b *Backend) NewSession(ctx context.Context, spec agent.Spec) (agent.Session, error) {
	id := uuid.New().String()

	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
	}
	if spec.Sandbox == "danger-full-access" {
		args = append(args, "--dangerously-skip-permissions")
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	args = append(args, "-p", spec.Prompt)

	cmd := exec.CommandContext(ctx, "claude", args...)
	if spec.Workdir != "" {
		cmd.Dir = spec.Workdir
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Build env: inherit current env, overlay spec.Env
	cmd.Env = os.Environ()
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("claude: stdout pipe: %w", err)
	}
	// Discard stderr to avoid blocking.
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claude: start: %w", err)
	}

	events := make(chan agent.Event, 64)
	s := &session{
		id:     id,
		cmd:    cmd,
		events: events,
		done:   make(chan struct{}),
		start:  time.Now(),
	}

	// Reader goroutine: parse stdout lines as JSON, emit events.
	go func() {
		defer close(events)
		defer close(s.done)

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			evt := mapClaudeEvent(line)
			select {
			case events <- evt:
			case <-ctx.Done():
				return
			}
		}

		// Process exit.
		waitErr := cmd.Wait()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.duration = time.Since(s.start)
		if waitErr != nil {
			if exitErr, ok := waitErr.(*exec.ExitError); ok {
				s.exitCode = exitErr.ExitCode()
			} else {
				s.exitCode = -1
			}
			s.waitErr = waitErr
		}
	}()

	return s, nil
}

func (b *Backend) Shutdown(_ context.Context) error { return nil }

type session struct {
	id     string
	cmd    *exec.Cmd
	events chan agent.Event
	done   chan struct{}
	start  time.Time

	mu       sync.Mutex
	exitCode int
	finalMsg string
	duration time.Duration
	waitErr  error
}

func (s *session) ID() string                  { return s.id }
func (s *session) Events() <-chan agent.Event   { return s.events }

func (s *session) Wait(ctx context.Context) (agent.Result, error) {
	select {
	case <-s.done:
	case <-ctx.Done():
		return agent.Result{}, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return agent.Result{
		ExitCode: s.exitCode,
		FinalMsg: s.finalMsg,
		Duration: s.duration,
	}, s.waitErr
}

func (s *session) Interrupt(ctx context.Context) error {
	if s.cmd.Process == nil {
		return nil
	}
	pgid := -s.cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)

	grace := 5 * time.Second
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-s.done:
		return nil
	case <-timer.C:
		_ = syscall.Kill(pgid, syscall.SIGKILL)
	case <-ctx.Done():
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		return ctx.Err()
	}

	select {
	case <-s.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (s *session) Close() error {
	return nil
}

// mapClaudeEvent converts a raw JSON line from claude --output-format stream-json
// into an agent.Event. The Kind follows the Claude event type field.
func mapClaudeEvent(line []byte) agent.Event {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return agent.Event{Kind: "log", Body: json.RawMessage(line)}
	}

	var msgType string
	if t, ok := raw["type"]; ok {
		_ = json.Unmarshal(t, &msgType)
	}

	kind := mapKind(msgType)
	body := json.RawMessage(append([]byte(nil), line...))
	return agent.Event{Kind: kind, Body: body}
}

func mapKind(claudeType string) string {
	switch claudeType {
	case "system.init":
		return "turn.started"
	case "assistant.content_block_delta":
		return "agent.message"
	case "assistant.message_stop":
		return "turn.completed"
	case "assistant.content_block_start":
		return "tool.call"
	case "user.tool_result":
		return "tool.result"
	case "system.error":
		return "error"
	case "assistant.message_start", "assistant.content_block_stop", "assistant.message_delta":
		return "internal"
	default:
		return "log"
	}
}

// extractFinalMessage attempts to read the last assistant text from a
// message_stop event body. Returns empty string if not applicable.
func extractFinalMessage(body json.RawMessage) string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	var text string
	if t, ok := raw["text"]; ok {
		_ = json.Unmarshal(t, &text)
	}
	return strings.TrimSpace(text)
}
