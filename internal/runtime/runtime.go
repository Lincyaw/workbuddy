package runtime

import (
	launcherpkg "github.com/Lincyaw/workbuddy/internal/launcher"
	"github.com/Lincyaw/workbuddy/internal/store"
)

type Runtime = launcherpkg.Runtime
type Session = launcherpkg.Session
type Result = launcherpkg.Result
type TaskContext = launcherpkg.TaskContext
type IssueContext = launcherpkg.IssueContext
type IssueComment = launcherpkg.IssueComment
type PRSummary = launcherpkg.PRSummary
type PRContext = launcherpkg.PRContext
type SessionContext = launcherpkg.SessionContext
type SessionRef = launcherpkg.SessionRef
type Approver = launcherpkg.Approver
type ApprovalRequest = launcherpkg.ApprovalRequest
type ApprovalDecision = launcherpkg.ApprovalDecision
type ApprovalKind = launcherpkg.ApprovalKind
type ApprovalScope = launcherpkg.ApprovalScope
type AlwaysAllow = launcherpkg.AlwaysAllow
type SessionManager = launcherpkg.SessionManager
type SessionCreateInput = launcherpkg.SessionCreateInput
type ManagedSession = launcherpkg.ManagedSession
type Registry = launcherpkg.Launcher

var ErrNotSupported = launcherpkg.ErrNotSupported

const (
	ApprovalExec        = launcherpkg.ApprovalExec
	ApprovalPatch       = launcherpkg.ApprovalPatch
	ApprovalPermissions = launcherpkg.ApprovalPermissions
	ApprovalToolInput   = launcherpkg.ApprovalToolInput
	ApprovalMCPElicit   = launcherpkg.ApprovalMCPElicit

	ScopeOnce    = launcherpkg.ScopeOnce
	ScopeSession = launcherpkg.ScopeSession
	ScopeForever = launcherpkg.ScopeForever

	MetaInfraFailure       = launcherpkg.MetaInfraFailure
	MetaInfraFailureReason = launcherpkg.MetaInfraFailureReason
	MetaStorageDegraded    = "storage_degraded"
	MetaStorageIssues      = "storage_issues"
)

func NewRegistry() *Registry {
	return launcherpkg.NewLauncher()
}

func NewSessionManager(baseDir string, st *store.Store) *SessionManager {
	return launcherpkg.NewSessionManager(baseDir, st)
}

func IsInfraFailure(result *Result) bool {
	return launcherpkg.IsInfraFailure(result)
}
