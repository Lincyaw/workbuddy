package launcher

import (
	"context"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
)

// Runtime is the interface for agent execution backends.
type Runtime interface {
	// Name returns the runtime identifier (e.g., "claude-code", "codex").
	Name() string
	// Launch executes an agent and returns the result.
	Launch(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (*Result, error)
}

// TaskContext provides the context for template rendering and agent execution.
type TaskContext struct {
	Issue   IssueContext
	PR      PRContext
	Repo    string
	Session SessionContext
}

// IssueContext holds GitHub issue information.
type IssueContext struct {
	Number int
	Title  string
	Body   string
	Labels []string
}

// PRContext holds GitHub PR information.
type PRContext struct {
	URL    string
	Branch string
}

// SessionContext holds session metadata.
type SessionContext struct {
	ID string
}

// Result holds the outcome of an agent execution.
type Result struct {
	ExitCode    int
	Stdout      string
	Stderr      string
	Duration    time.Duration
	Meta        map[string]string // parsed from WORKBUDDY_META block
	SessionPath string            // path to session artifacts
}
