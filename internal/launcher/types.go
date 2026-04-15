package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
)

var ErrNotSupported = errors.New("launcher: not supported")

type Runtime interface {
	Name() string
	Start(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (Session, error)
	Launch(ctx context.Context, agent *config.AgentConfig, task *TaskContext) (*Result, error)
}

type Session interface {
	Run(ctx context.Context, events chan<- launcherevents.Event) (*Result, error)
	SetApprover(Approver) error
	Close() error
}

type Approver interface {
	Approve(ctx context.Context, req ApprovalRequest) ApprovalDecision
}

type ApprovalRequest struct {
	Kind   ApprovalKind    `json:"kind"`
	Detail json.RawMessage `json:"detail,omitempty"`
	Source SessionRef      `json:"source"`
}

type ApprovalKind string

const (
	ApprovalExec        ApprovalKind = "exec"
	ApprovalPatch       ApprovalKind = "patch"
	ApprovalPermissions ApprovalKind = "permissions"
	ApprovalToolInput   ApprovalKind = "tool_input"
	ApprovalMCPElicit   ApprovalKind = "mcp_elicit"
)

type ApprovalDecision struct {
	Allow  bool          `json:"allow"`
	Scope  ApprovalScope `json:"scope"`
	Reason string        `json:"reason,omitempty"`
}

type ApprovalScope string

const (
	ScopeOnce    ApprovalScope = "once"
	ScopeSession ApprovalScope = "session"
	ScopeForever ApprovalScope = "forever"
)

type AlwaysAllow struct{}

func (AlwaysAllow) Approve(context.Context, ApprovalRequest) ApprovalDecision {
	return ApprovalDecision{Allow: true, Scope: ScopeSession, Reason: "always allow"}
}

// TaskContext provides the context for template rendering and agent execution.
type TaskContext struct {
	Issue   IssueContext
	PR      PRContext
	Repo    string
	WorkDir string
	Session SessionContext
}

type IssueContext struct {
	Number int
	Title  string
	Body   string
	Labels []string
}

type PRContext struct {
	URL    string
	Branch string
}

type SessionContext struct {
	ID string
}

type SessionRef struct {
	ID   string `json:"id,omitempty"`
	Kind string `json:"kind,omitempty"`
}

type Result struct {
	ExitCode    int
	Stdout      string
	Stderr      string
	Duration    time.Duration
	Meta        map[string]string
	// SessionPath is the canonical session artifact path handed to audit
	// and reporter. When Event Schema v1 capture succeeds this points at
	// the normalized events-v1.jsonl; otherwise it falls back to whatever
	// the runtime produced natively (e.g. codex-exec.jsonl).
	SessionPath    string
	// RawSessionPath preserves the runtime-native artifact path (if any)
	// when SessionPath has been overridden with the normalized v1 stream.
	RawSessionPath string
	LastMessage    string
	TokenUsage     *launcherevents.TokenUsagePayload
	SessionRef     SessionRef
}
