package runtime

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
	Issue          IssueContext
	PR             PRContext
	Repo           string
	RepoRoot       string
	WorkDir        string
	Session        SessionContext
	RelatedPRs     []PRSummary
	RelatedPRsText string
	Rollout        RolloutContext
	sessionHandle  *ManagedSession

	// stateName / state carry the workflow state metadata used to synthesize
	// the transition footer that is appended to the agent prompt at dispatch
	// (issue #204 batch 3). They are intentionally unexported so they do not
	// appear in the TaskContext schema and cannot be referenced directly from
	// agent prompt templates as `{{.State…}}` — the footer is the only way
	// state metadata leaks into the rendered prompt.
	stateName string
	state     *stateMetadata
}

// stateMetadata mirrors the subset of *config.State that the footer renderer
// needs. Storing only the values (rather than the pointer to config.State)
// keeps the runtime package free of a circular dependency on internal/config
// for TaskContext, while still letting the Preparer attach the data once.
type stateMetadata struct {
	EnterLabel  string
	Transitions map[string]string
}

type IssueContext struct {
	Number       int
	Title        string
	Body         string
	Labels       []string
	Comments     []IssueComment
	CommentsText string
}

type IssueComment struct {
	Author    string
	Body      string
	CreatedAt string
}

type PRSummary struct {
	Number      int
	State       string
	Title       string
	HeadRefName string
	BaseRefName string
	URL         string
	IsDraft     bool
}

type PRContext struct {
	URL    string
	Branch string
}

type SessionContext struct {
	ID         string
	TaskID     string
	WorkerID   string
	Attempt    int
	PreLabels  []string
	PostLabels []string
}

type RolloutContext struct {
	Index   int
	Total   int
	GroupID string
}

func (t *TaskContext) SessionHandle() *ManagedSession {
	if t == nil {
		return nil
	}
	return t.sessionHandle
}

func (t *TaskContext) SetSessionHandle(handle *ManagedSession) {
	if t == nil {
		return
	}
	t.sessionHandle = handle
}

// SetWorkflowState attaches workflow-state metadata used by the runtime to
// build the transition footer appended to the agent prompt. Pass an empty
// stateName and nil transitions to clear it.
//
// The runtime package owns this seam intentionally so internal/config is not
// imported into TaskContext directly.
func (t *TaskContext) SetWorkflowState(stateName, enterLabel string, transitions map[string]string) {
	if t == nil {
		return
	}
	t.stateName = stateName
	if stateName == "" && enterLabel == "" && len(transitions) == 0 {
		t.state = nil
		return
	}
	cloned := make(map[string]string, len(transitions))
	for k, v := range transitions {
		cloned[k] = v
	}
	t.state = &stateMetadata{EnterLabel: enterLabel, Transitions: cloned}
}

// WorkflowStateName returns the workflow state attached via SetWorkflowState.
func (t *TaskContext) WorkflowStateName() string {
	if t == nil {
		return ""
	}
	return t.stateName
}

// WorkflowStateMetadata returns the enter_label and transitions attached via
// SetWorkflowState. The returned map is a defensive copy.
func (t *TaskContext) WorkflowStateMetadata() (enterLabel string, transitions map[string]string) {
	if t == nil || t.state == nil {
		return "", nil
	}
	cloned := make(map[string]string, len(t.state.Transitions))
	for k, v := range t.state.Transitions {
		cloned[k] = v
	}
	return t.state.EnterLabel, cloned
}

type SessionRef struct {
	ID   string `json:"id,omitempty"`
	Kind string `json:"kind,omitempty"`
}

type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
	Meta     map[string]string
	// SessionPath is the canonical session artifact path handed to audit
	// and reporter. When Event Schema v1 capture succeeds this points at
	// the normalized events-v1.jsonl; otherwise it falls back to the best
	// runtime artifact available.
	SessionPath string
	// RawSessionPath preserves the runtime-native artifact path (if any)
	// when SessionPath has been overridden with the normalized v1 stream.
	RawSessionPath string
	LastMessage    string
	TokenUsage     *launcherevents.TokenUsagePayload
	SessionRef     SessionRef
}
