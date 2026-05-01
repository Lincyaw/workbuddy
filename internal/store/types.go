package store

import "time"

// Event represents a recorded event in the events table.
type Event struct {
	ID       int64
	TS       time.Time
	Type     string
	Repo     string
	IssueNum int
	Payload  string // JSON
}

// TaskRecord represents a task in the task_queue table.
type TaskRecord struct {
	ID             string
	Repo           string
	IssueNum       int
	AgentName      string
	Labels         string
	Role           string
	Runtime        string
	Workflow       string
	State          string
	WorkerID       string
	ClaimToken     string
	Status         string // pending, running, completed, failed, timeout
	LeaseExpiresAt time.Time
	AckedAt        time.Time
	HeartbeatAt    time.Time
	CompletedAt    time.Time
	ExitCode       int
	SessionRefs    string // JSON
	RolloutIndex   int
	RolloutsTotal  int
	RolloutGroupID string
	// SupervisorAgentID links the task to a subprocess managed by the local
	// supervisor IPC service (issue #234). Empty string means the task does
	// not (yet) have a supervisor-tracked agent.
	SupervisorAgentID string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// WorkerRecord represents a registered worker.
type WorkerRecord struct {
	ID            string
	Repo          string
	ReposJSON     string
	Roles         string // JSON array
	Runtime       string
	Hostname      string
	MgmtBaseURL   string
	// AuditURL is the worker-advertised base URL of its audit HTTP server
	// (Phase 1 of the session-data ownership refactor). Empty when the
	// worker did not start an audit listener (i.e. --audit-listen=disabled
	// or older worker builds). Phase 2 will use this to proxy session
	// reads from the coordinator to the worker that owns the data; Phase 1
	// only persists it.
	AuditURL      string
	Status        string // online, offline
	LastHeartbeat time.Time
	RegisteredAt  time.Time
}

// RepoRegistrationRecord stores the coordinator-side configuration pushed by a repo.
type RepoRegistrationRecord struct {
	Repo         string
	Environment  string
	Status       string
	ConfigJSON   string
	RegisteredAt time.Time
	UpdatedAt    time.Time
}

// TransitionCount tracks retry counts for back-edges.
type TransitionCount struct {
	Repo      string
	IssueNum  int
	FromState string
	ToState   string
	Count     int
}

// IssueCache stores the last known state of an issue for change detection.
type IssueCache struct {
	Repo      string
	IssueNum  int
	Labels    string // JSON array
	Body      string
	State     string
	UpdatedAt time.Time
}

// SessionRoute is the coordinator-side index that maps a session_id to the
// worker that owns the underlying SessionRecord and on-disk artefacts. It is
// the only session-shaped data the coordinator stores after the worker-owned
// refactor (REQ-122 follow-up): the rest lives on the worker that produced
// it. Resolver uses (worker_id, repo, issue_num) to dispatch session reads to
// the owning worker via workers.audit_url.
type SessionRoute struct {
	SessionID string
	WorkerID  string
	Repo      string
	IssueNum  int
	CreatedAt time.Time
}

// SessionRecord stores the durable execution session index row.
type SessionRecord struct {
	ID            int64
	SessionID     string
	TaskID        string
	Repo          string
	IssueNum      int
	AgentName     string
	Runtime       string
	WorkerID      string
	Attempt       int
	Status        string
	Dir           string
	StdoutPath    string
	StderrPath    string
	ToolCallsPath string
	MetadataPath  string
	Summary       string
	RawPath       string
	CreatedAt     time.Time
	ClosedAt      time.Time
}

const (
	DependencyStatusActive               = "active"
	DependencyStatusUnsupportedCrossRepo = "unsupported_cross_repo"
	DependencyStatusInvalid              = "invalid"
	DependencyStatusRemoved              = "removed"

	DependencyVerdictReady      = "ready"
	DependencyVerdictBlocked    = "blocked"
	DependencyVerdictOverride   = "override"
	DependencyVerdictNeedsHuman = "needs_human"
)

// IssueCycleState tracks per-issue dev↔review cycle counts and timing
// for the orchestrator-level cycle-cap and long-flight stuck detector.
type IssueCycleState struct {
	Repo                string
	IssueNum            int
	DevReviewCycleCount int
	SynthCycleCount     int
	FirstDispatchAt     time.Time
	CapHitAt            time.Time
	SynthCapHitAt       time.Time
	UpdatedAt           time.Time
}

type IssueDependency struct {
	Repo              string
	IssueNum          int
	DependsOnRepo     string
	DependsOnIssueNum int
	SourceHash        string
	Status            string
}

// IssueDependencyState is the cached per-issue dep verdict used by the
// dispatch gate and for change detection (reaction add/remove).
//
// LastReactionBlocked records the last on-GitHub reaction state we applied,
// so the Coordinator only issues a reaction-write when it actually flips.
type IssueDependencyState struct {
	Repo                string
	IssueNum            int
	Verdict             string
	ResumeLabel         string
	BlockedReasonHash   string
	OverrideActive      bool
	GraphVersion        int64
	LastReactionBlocked bool
	LastEvaluatedAt     time.Time
}
