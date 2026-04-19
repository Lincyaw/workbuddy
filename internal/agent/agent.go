// Package agent defines a unified interface for agent execution backends.
package agent

import (
	"context"
	"encoding/json"
	"time"
)

// Spec describes what to execute.
type Spec struct {
	Backend string            `json:"backend"`
	Workdir string            `json:"workdir"`
	Prompt  string            `json:"prompt"`
	Model   string            `json:"model,omitempty"`
	Sandbox string            `json:"sandbox,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Tags    map[string]string `json:"tags,omitempty"`
}

// Event is a single streaming event from the agent.
type Event struct {
	Kind string          `json:"kind"`
	Body json.RawMessage `json:"body"`
}

// Result is the outcome of a completed session.
type Result struct {
	ExitCode     int           `json:"exit_code"`
	FinalMsg     string        `json:"final_msg,omitempty"`
	FilesChanged []string      `json:"files_changed,omitempty"`
	Duration     time.Duration `json:"duration"`
}

// Session represents a running agent execution.
type Session interface {
	Events() <-chan Event
	Wait(ctx context.Context) (Result, error)
	Interrupt(ctx context.Context) error
	Close() error
	ID() string
}

// Backend creates and manages agent sessions.
type Backend interface {
	NewSession(ctx context.Context, spec Spec) (Session, error)
	Shutdown(ctx context.Context) error
}
