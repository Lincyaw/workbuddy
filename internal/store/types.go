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
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// WorkerRecord represents a registered worker.
type WorkerRecord struct {
	ID            string
	Repo          string
	Roles         string // JSON array
	Hostname      string
	Status        string // online, offline
	LastHeartbeat time.Time
	RegisteredAt  time.Time
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
