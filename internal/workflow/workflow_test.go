package workflow

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newInMemoryManager(t *testing.T) *Manager {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS workflow_instances (
			id TEXT PRIMARY KEY,
			workflow_name TEXT NOT NULL,
			repo TEXT NOT NULL,
			issue_num INTEGER NOT NULL,
			current_state TEXT NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE (repo, issue_num, workflow_name)
		)`)
	if err != nil {
		t.Fatalf("initialize sqlite schema: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS workflow_transitions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			workflow_instance_id TEXT NOT NULL,
			from_state TEXT NOT NULL,
			to_state TEXT NOT NULL,
			trigger_agent TEXT NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (workflow_instance_id) REFERENCES workflow_instances(id) ON DELETE CASCADE
		)`)
	if err != nil {
		t.Fatalf("initialize sqlite schema: %v", err)
	}
	return NewManager(db)
}

func TestCreateWorkflowInstance(t *testing.T) {
	manager := newInMemoryManager(t)
	inst, err := manager.CreateIfMissing("owner/repo", 42, "default", "developing")
	if err != nil {
		t.Fatalf("CreateIfMissing: %v", err)
	}
	if inst == nil {
		t.Fatal("expected instance")
	}
	if inst.Repo != "owner/repo" || inst.IssueNum != 42 || inst.WorkflowName != "default" || inst.CurrentState != "developing" {
		t.Fatalf("instance mismatch: %+v", inst)
	}
	if inst.ID == "" {
		t.Fatal("instance id should not be empty")
	}
	if len(inst.History) != 0 {
		t.Fatalf("expected no history, got %d", len(inst.History))
	}
}

func TestAdvanceState(t *testing.T) {
	manager := newInMemoryManager(t)
	if _, err := manager.CreateIfMissing("owner/repo", 7, "default", "developing"); err != nil {
		t.Fatalf("CreateIfMissing: %v", err)
	}
	inst, err := manager.Advance("owner/repo", 7, "default", "developing", "reviewing", "dev-agent")
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if inst.CurrentState != "reviewing" {
		t.Fatalf("expected current_state=reviewing, got %q", inst.CurrentState)
	}
	if len(inst.History) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(inst.History))
	}
	if inst.History[0].From != "developing" || inst.History[0].To != "reviewing" || inst.History[0].TriggerAgent != "dev-agent" {
		t.Fatalf("history entry mismatch: %+v", inst.History[0])
	}
}

func TestHistoryOrderingAndAllTransitions(t *testing.T) {
	manager := newInMemoryManager(t)
	if _, err := manager.CreateIfMissing("owner/repo", 100, "default", "developing"); err != nil {
		t.Fatalf("CreateIfMissing: %v", err)
	}

	if _, err := manager.Advance("owner/repo", 100, "default", "developing", "reviewing", "dev-agent"); err != nil {
		t.Fatalf("advance1: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := manager.Advance("owner/repo", 100, "default", "reviewing", "testing", "review-agent"); err != nil {
		t.Fatalf("advance2: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := manager.Advance("owner/repo", 100, "default", "testing", "done", "release-agent"); err != nil {
		t.Fatalf("advance3: %v", err)
	}

	instances, err := manager.QueryByRepoIssue("owner/repo", 100)
	if err != nil {
		t.Fatalf("QueryByRepoIssue: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(instances))
	}
	history := instances[0].History
	if len(history) != 3 {
		t.Fatalf("expected 3 history entries, got %d", len(history))
	}
	for i := 1; i < len(history); i++ {
		if history[i-1].Timestamp.After(history[i].Timestamp) {
			t.Fatalf("history not sorted by timestamp: %v before %v", history[i-1].Timestamp, history[i].Timestamp)
		}
	}
}
