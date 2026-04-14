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
	ID        string
	Repo      string
	IssueNum  int
	AgentName string
	WorkerID  string
	Status    string // pending, running, completed, failed, timeout
	CreatedAt time.Time
	UpdatedAt time.Time
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
	State     string
	UpdatedAt time.Time
}
