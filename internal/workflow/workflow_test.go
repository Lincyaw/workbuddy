package workflow

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	st, err := store.NewStore(filepath.Join(t.TempDir(), "workbuddy.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewManager(st)
}

func TestCreateWorkflowInstance(t *testing.T) {
	manager := newTestManager(t)
	if err := manager.CreateIfMissing("owner/repo", 42, "default", "developing"); err != nil {
		t.Fatalf("CreateIfMissing: %v", err)
	}
	instances, err := manager.QueryByRepoIssue("owner/repo", 42)
	if err != nil {
		t.Fatalf("QueryByRepoIssue: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(instances))
	}
	inst := instances[0]
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
	manager := newTestManager(t)
	if err := manager.CreateIfMissing("owner/repo", 7, "default", "developing"); err != nil {
		t.Fatalf("CreateIfMissing: %v", err)
	}
	if err := manager.Advance("owner/repo", 7, "default", "developing", "reviewing", "dev-agent"); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	instances, err := manager.QueryByRepoIssue("owner/repo", 7)
	if err != nil {
		t.Fatalf("QueryByRepoIssue: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(instances))
	}
	inst := instances[0]
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
	manager := newTestManager(t)
	if err := manager.CreateIfMissing("owner/repo", 100, "default", "developing"); err != nil {
		t.Fatalf("CreateIfMissing: %v", err)
	}

	if err := manager.Advance("owner/repo", 100, "default", "developing", "reviewing", "dev-agent"); err != nil {
		t.Fatalf("advance1: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := manager.Advance("owner/repo", 100, "default", "reviewing", "testing", "review-agent"); err != nil {
		t.Fatalf("advance2: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := manager.Advance("owner/repo", 100, "default", "testing", "done", "release-agent"); err != nil {
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
