package launcher

import runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"

type Runtime = runtimepkg.Runtime
type Session = runtimepkg.Session
type Result = runtimepkg.Result
type TaskContext = runtimepkg.TaskContext
type IssueContext = runtimepkg.IssueContext
type IssueComment = runtimepkg.IssueComment
type PRSummary = runtimepkg.PRSummary
type PRContext = runtimepkg.PRContext
type SessionContext = runtimepkg.SessionContext
type SessionRef = runtimepkg.SessionRef
type Approver = runtimepkg.Approver
type ApprovalRequest = runtimepkg.ApprovalRequest
type ApprovalDecision = runtimepkg.ApprovalDecision
type ApprovalKind = runtimepkg.ApprovalKind
type ApprovalScope = runtimepkg.ApprovalScope
type AlwaysAllow = runtimepkg.AlwaysAllow
type SessionManager = runtimepkg.SessionManager
type SessionCreateInput = runtimepkg.SessionCreateInput
type ManagedSession = runtimepkg.ManagedSession

var ErrNotSupported = runtimepkg.ErrNotSupported

const (
	ApprovalExec        = runtimepkg.ApprovalExec
	ApprovalPatch       = runtimepkg.ApprovalPatch
	ApprovalPermissions = runtimepkg.ApprovalPermissions
	ApprovalToolInput   = runtimepkg.ApprovalToolInput
	ApprovalMCPElicit   = runtimepkg.ApprovalMCPElicit

	ScopeOnce    = runtimepkg.ScopeOnce
	ScopeSession = runtimepkg.ScopeSession
	ScopeForever = runtimepkg.ScopeForever
)
