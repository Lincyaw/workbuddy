// Package store: Store interface.
//
// The Store interface is the abstract persistence boundary for workbuddy.
// Every operation that crosses out of the internal/store package goes
// through this interface so callers are not coupled to a particular SQL
// engine.
//
// Today the only implementation is *dbStore, returned by New /
// NewStore / NewCoordinatorStore. A MySQL implementation is planned (see
// docs/decisions/2026-05-13-k8s-agentm-otel.md, Block 3 § Storage, and
// follow-up issue #316) and will plug in behind this same interface
// without touching call sites.
//
// The method surface is intentionally a faithful transcription of the
// methods previously defined directly on the concrete Store struct —
// this PR is a pure refactor, not a redesign. The few raw-SQL escape
// hatches (Exec/Query/QueryRow) are retained so existing test setup and
// migration helpers keep working; production code should prefer the
// typed methods.
package store

import (
	"database/sql"
	"time"
)

// Store is the abstract persistence boundary. See package doc.
type Store interface {
	// Lifecycle.
	Close() error
	SetNowFunc(nowFunc func() time.Time)

	// Raw SQL escape hatches. Retained for tests, migration tools, and
	// the small number of legacy call sites that have not yet been
	// expressed as typed repository methods. New production code should
	// prefer the typed methods on this interface.
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row

	// Events.
	InsertEvent(e Event) (int64, error)
	QueryEvents(repo string) ([]Event, error)
	QueryEventsFiltered(filter EventQueryFilter) ([]Event, error)
	LatestEventAt(repo string, issueNum int) (*time.Time, error)
	LatestIssueEvent(repo string, issueNum int) (*IssueEventMeta, error)
	LatestIssueTransition(repo string, issueNum int) (*IssueTransitionEvent, error)
	QueryIssueTransitions(repo string, issueNum int) ([]IssueTransitionEvent, error)
	CountEventsByRepoType() ([]EventCountByRepoType, error)
	LastEventTimestampByType(eventType string) (*time.Time, error)
	TokenUsageEvents(eventType string) ([]TokenUsagePayload, error)

	// Tasks.
	InsertTask(t TaskRecord) error
	QueryTasks(status string) ([]TaskRecord, error)
	QueryTasksFiltered(filter TaskFilter) ([]TaskRecord, error)
	GetTask(taskID string) (*TaskRecord, error)
	ListIssueTasks(repo string, issueNum int) ([]TaskRecord, error)
	ListTasksForIssue(repo string, issueNum int) ([]TaskRecord, error)
	UpdateTaskSupervisorAgentID(taskID, agentID string) error
	UpdateTaskStatus(taskID, status string) error
	TransitionTaskStatusIfRunning(taskID, status string) error
	FinalizeTaskForOperator(taskID, status string, exitCode int) error
	ClaimTask(taskID, workerID string) (bool, error)
	ReleaseTask(taskID, workerID string) (bool, error)
	ClaimNextTask(workerID string, roles []string, repos []string, runtime string, claimToken string, lease time.Duration) (*TaskRecord, error)
	AckTask(taskID, workerID string, lease time.Duration) error
	HeartbeatTask(taskID, workerID string, lease time.Duration) error
	CompleteTask(taskID, workerID string, exitCode int, sessionRefs string) error
	HasActiveTask(repo string, issueNum int, agentName string) (bool, error)
	HasAnyActiveTask(repo string, issueNum int) (bool, error)
	FailPendingTasksForRepo(repo string) error
	InFlightTasksForWorker(workerID string) ([]InFlightTaskForWorker, error)
	WorkerCurrentTaskID(workerID string) (string, error)
	WorkerHasRunningTask(workerID string) (bool, error)
	CountTasksByRepoStatus() ([]TaskCountByRepoStatus, error)

	// Workers.
	InsertWorker(w WorkerRecord) error
	QueryWorkers(repo string) ([]WorkerRecord, error)
	GetWorker(workerID string) (*WorkerRecord, error)
	UpdateWorkerHeartbeat(workerID string) error
	UpdateWorkerStatus(workerID, status string) error
	DeleteWorker(workerID string) (bool, error)
	CountWorkers() (int, error)
	CountWorkersByRepo() ([]WorkerCountByRepo, error)

	// Worker tokens.
	IssueWorkerToken(workerID, repo string, roles []string, hostname string) (*IssuedWorkerToken, error)
	ListWorkerTokens(repo string) ([]WorkerTokenRecord, error)
	RevokeWorkerToken(workerID, kid string) error
	AuthenticateWorkerToken(token string) (*WorkerAuthRecord, error)

	// Repo registrations.
	UpsertRepoRegistration(rec RepoRegistrationRecord) error
	GetRepoRegistration(repo string) (*RepoRegistrationRecord, error)
	ListRepoRegistrations() ([]RepoRegistrationRecord, error)
	DeleteRepoRegistration(repo string) error

	// Issue cache.
	UpsertIssueCache(ic IssueCache) error
	ListCachedIssueNums(repo string) ([]int, error)
	DeleteIssueCache(repo string, issueNum int) error
	QueryIssueCache(repo string, issueNum int) (*IssueCache, error)
	ListIssueCaches(repo string) ([]IssueCache, error)

	// Root trace IDs (REQ-137 / #317). On first ingest of an issue or PR
	// row into issue_cache, the store assigns a freshly minted OTel
	// trace_id (32-hex). Subsequent operations on that entity look up the
	// stored trace_id so all child spans correlate under the same trace.
	//
	// The PR/issue cross-linkage ("PR inherits parent issue's trace_id")
	// is not yet implemented at the schema level — PRs and issues share
	// the issue_cache table but there is no parent_issue_num column, so
	// each row gets its own freshly minted trace_id. Cross-linking will
	// land alongside the span-correlation work in #320.
	GetIssueRootTraceID(repo string, issueNum int) (string, error)
	GetPRRootTraceID(repo string, prNum int) (string, error)

	// Transitions / cycle state.
	IncrementTransition(repo string, issueNum int, fromState, toState string) (int, error)
	CountConsecutiveAgentFailures(repo string, issueNum int, agentName string) (int, error)
	QueryTransitionCounts(repo string, issueNum int) ([]TransitionCount, error)
	MaxTransitionCounts() ([]TransitionMaxCount, error)
	IncrementDevReviewCycleCount(repo string, issueNum int) (int, error)
	IncrementSynthCycleCount(repo string, issueNum int) (int, error)
	TouchIssueFirstDispatch(repo string, issueNum int) error
	MarkIssueCycleCapHit(repo string, issueNum int) error
	MarkIssueSynthCycleCapHit(repo string, issueNum int) error
	QueryIssueCycleState(repo string, issueNum int) (*IssueCycleState, error)
	ResetIssueCycleState(repo string, issueNum int) error

	// Sessions.
	CreateSession(sess SessionRecord) (int64, error)
	UpdateSession(record SessionRecord) error
	GetSession(sessionID string) (*SessionRecord, error)
	ListSessions(f SessionFilter) ([]SessionRecord, error)
	ListSessionsForAPI(filter SessionListFilter) ([]SessionRecord, error)
	LatestSessionForIssue(repo string, issueNum int) (*SessionRecord, error)
	CountActiveSessions() (int, error)
	CountTerminalSessionsSince(status string, since time.Time) (int, error)
	AggregateSessionMetrics() (SessionAggregateMetrics, error)
	CountSessionsByAgent() ([]SessionCountByAgent, error)

	// Agent sessions (legacy worker-side index).
	InsertAgentSession(sess AgentSession) (int64, error)
	QueryAgentSessions(repo string, issueNum int) ([]AgentSession, error)
	ListAgentSessions(f SessionFilter) ([]AgentSession, error)
	UpdateAgentSession(sessionID, summary, rawPath string) error
	GetAgentSession(sessionID string) (*AgentSession, error)

	// Session routes (coordinator-side index).
	UpsertSessionRoute(route SessionRoute) error
	BulkUpsertSessionRoutes(routes []SessionRoute) error
	GetSessionRoute(sessionID string) (*SessionRoute, error)

	// Issue dependencies.
	ReplaceIssueDependencies(repo string, issueNum int, deps []IssueDependency) error
	ListIssueDependencies(repo string, issueNum int) ([]IssueDependency, error)
	UpsertIssueDependencyState(state IssueDependencyState) error
	MarkDependencyReactionApplied(repo string, issueNum int, blocked bool) error
	QueryIssueDependencyState(repo string, issueNum int) (*IssueDependencyState, error)
	DeleteIssueDependencyState(repo string, issueNum int) (bool, error)

	// Issue claims (coordinator + worker fence).
	AcquireIssueClaim(repo string, issueNum int, workerID string, lease time.Duration) (AcquireIssueClaimResult, error)
	ReleaseIssueClaim(repo string, issueNum int, workerID, claimToken string) (bool, error)
	DeleteIssueClaim(repo string, issueNum int) (bool, error)
	RefreshIssueClaim(repo string, issueNum int, workerID, claimToken string, lease time.Duration) (bool, error)
	QueryIssueClaim(repo string, issueNum int) (*IssueClaimRecord, error)
	DeleteStaleCoordinatorIssueClaims(currentPID int) ([]IssueClaimRecord, error)

	// Pipeline hazards.
	UpsertIssuePipelineHazard(h PipelineHazard) (changed bool, err error)
	QueryIssuePipelineHazard(repo string, issueNum int) (*PipelineHazard, error)
	ListIssuePipelineHazards(repo string) ([]PipelineHazard, error)
	ClearIssuePipelineHazard(repo string, issueNum int) error

	// Workflow instances.
	CreateWorkflowInstanceIfMissing(id, workflowName, repo string, issueNum int, currentState string) error
	AdvanceWorkflowInstance(id, fromState, toState, triggerAgent string, at time.Time) error
	QueryWorkflowInstancesByRepoIssue(repo string, issueNum int) ([]WorkflowInstanceRow, error)
	GetWorkflowInstanceByID(id string) (*WorkflowInstanceRow, error)
	QueryWorkflowTransitions(instanceID string) ([]WorkflowTransitionRow, error)

	// Rollout groups.
	LatestRolloutGroupSummaryForIssueState(repo string, issueNum int, workflow, state string) (*RolloutGroupSummary, error)
	ListTasksByRolloutGroup(groupID string) ([]TaskRecord, error)
	SummarizeRolloutGroup(groupID string) (*RolloutGroupSummary, error)

	// Cross-cutting aggregates.
	ListOpenIssueActivity(pendingStatus, runningStatus string) ([]IssueActivityRow, error)

	// Typed maintenance helpers (added by the v0.5 abstraction step so
	// callers like internal/recover and internal/operator no longer need
	// to crack open the *sql.DB handle).
	ResetTables(tables []string) error
	CountTasksByIssue(repo string, issueNum int) (int, error)
	RecentAlertPayloads(eventType string, limit int) ([]string, error)
	MarkStaleWorkersOffline(threshold time.Duration) error
}

// compile-time assertion that the concrete type satisfies the interface.
var _ Store = (*dbStore)(nil)
