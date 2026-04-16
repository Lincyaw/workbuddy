package diagnose

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

func newDiagnoseStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.NewStore(filepath.Join(t.TempDir(), "diagnose.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestAnalyze(t *testing.T) {
	now := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)

	t.Run("stuck issue positive", func(t *testing.T) {
		st := newDiagnoseStore(t)
		seedDiagnoseIssue(t, st, 1, "status:developing", now.Add(-2*time.Hour))
		findings, err := Analyze(st, "owner/repo", now)
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		assertFinding(t, findings, KindStuckIssue, 1, true)
	})

	t.Run("stuck issue negative when active task exists", func(t *testing.T) {
		st := newDiagnoseStore(t)
		seedDiagnoseIssue(t, st, 2, "status:developing", now.Add(-2*time.Hour))
		if err := st.InsertTask(store.TaskRecord{ID: "task-2", Repo: "owner/repo", IssueNum: 2, AgentName: "dev-agent", Status: store.TaskStatusRunning}); err != nil {
			t.Fatalf("InsertTask: %v", err)
		}
		findings, err := Analyze(st, "owner/repo", now)
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		assertFinding(t, findings, KindStuckIssue, 2, false)
	})

	t.Run("missed redispatch positive", func(t *testing.T) {
		st := newDiagnoseStore(t)
		seedDiagnoseIssue(t, st, 3, "status:reviewing", now.Add(-5*time.Minute))
		if err := st.UpsertIssueDependencyState(store.IssueDependencyState{Repo: "owner/repo", IssueNum: 3, Verdict: store.DependencyVerdictReady}); err != nil {
			t.Fatalf("UpsertIssueDependencyState: %v", err)
		}
		findings, err := Analyze(st, "owner/repo", now)
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		assertFinding(t, findings, KindMissedRedispatch, 3, true)
	})

	t.Run("missed redispatch negative when not ready", func(t *testing.T) {
		st := newDiagnoseStore(t)
		seedDiagnoseIssue(t, st, 4, "status:reviewing", now.Add(-5*time.Minute))
		if err := st.UpsertIssueDependencyState(store.IssueDependencyState{Repo: "owner/repo", IssueNum: 4, Verdict: store.DependencyVerdictBlocked}); err != nil {
			t.Fatalf("UpsertIssueDependencyState: %v", err)
		}
		findings, err := Analyze(st, "owner/repo", now)
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		assertFinding(t, findings, KindMissedRedispatch, 4, false)
	})

	t.Run("orphaned task positive", func(t *testing.T) {
		st := newDiagnoseStore(t)
		if err := st.InsertTask(store.TaskRecord{ID: "task-5", Repo: "owner/repo", IssueNum: 5, AgentName: "dev-agent", Status: store.TaskStatusRunning}); err != nil {
			t.Fatalf("InsertTask: %v", err)
		}
		if _, err := st.DB().Exec(`UPDATE task_queue SET updated_at = ? WHERE id = ?`, now.Add(-2*time.Hour).Format("2006-01-02 15:04:05"), "task-5"); err != nil {
			t.Fatalf("update task: %v", err)
		}
		findings, err := Analyze(st, "owner/repo", now)
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		assertFinding(t, findings, KindOrphanedTask, 5, true)
	})

	t.Run("orphaned task negative when fresh", func(t *testing.T) {
		st := newDiagnoseStore(t)
		if err := st.InsertTask(store.TaskRecord{ID: "task-6", Repo: "owner/repo", IssueNum: 6, AgentName: "dev-agent", Status: store.TaskStatusRunning}); err != nil {
			t.Fatalf("InsertTask: %v", err)
		}
		if _, err := st.DB().Exec(`UPDATE task_queue SET updated_at = ? WHERE id = ?`, now.Format("2006-01-02 15:04:05"), "task-6"); err != nil {
			t.Fatalf("update task: %v", err)
		}
		findings, err := Analyze(st, "owner/repo", now)
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		assertFinding(t, findings, KindOrphanedTask, 6, false)
	})

	t.Run("repeated failure positive", func(t *testing.T) {
		st := newDiagnoseStore(t)
		for i := 0; i < 3; i++ {
			if err := st.InsertTask(store.TaskRecord{ID: "task-f-" + string(rune('a'+i)), Repo: "owner/repo", IssueNum: 7, AgentName: "dev-agent", Status: store.TaskStatusFailed}); err != nil {
				t.Fatalf("InsertTask: %v", err)
			}
		}
		findings, err := Analyze(st, "owner/repo", now)
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		assertFinding(t, findings, KindRepeatedFailure, 7, true)
	})

	t.Run("repeated failure negative when streak broken", func(t *testing.T) {
		st := newDiagnoseStore(t)
		if err := st.InsertTask(store.TaskRecord{ID: "task-8a", Repo: "owner/repo", IssueNum: 8, AgentName: "dev-agent", Status: store.TaskStatusFailed}); err != nil {
			t.Fatalf("InsertTask: %v", err)
		}
		if err := st.InsertTask(store.TaskRecord{ID: "task-8b", Repo: "owner/repo", IssueNum: 8, AgentName: "dev-agent", Status: store.TaskStatusCompleted}); err != nil {
			t.Fatalf("InsertTask: %v", err)
		}
		if err := st.InsertTask(store.TaskRecord{ID: "task-8c", Repo: "owner/repo", IssueNum: 8, AgentName: "dev-agent", Status: store.TaskStatusFailed}); err != nil {
			t.Fatalf("InsertTask: %v", err)
		}
		findings, err := Analyze(st, "owner/repo", now)
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		assertFinding(t, findings, KindRepeatedFailure, 8, false)
	})
}

func seedDiagnoseIssue(t *testing.T, st *store.Store, issueNum int, status string, eventAt time.Time) {
	t.Helper()
	if err := st.UpsertIssueCache(store.IssueCache{Repo: "owner/repo", IssueNum: issueNum, Labels: `["` + status + `"]`, State: "open"}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	id, err := st.InsertEvent(store.Event{Type: "state_entry", Repo: "owner/repo", IssueNum: issueNum, Payload: `{"state":"` + status + `"}`})
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if _, err := st.DB().Exec(`UPDATE events SET ts = ? WHERE id = ?`, eventAt.Format("2006-01-02 15:04:05"), id); err != nil {
		t.Fatalf("update event ts: %v", err)
	}
}

func assertFinding(t *testing.T, findings []Finding, kind string, issueNum int, want bool) {
	t.Helper()
	found := false
	for _, finding := range findings {
		if finding.Kind == kind && finding.IssueNum == issueNum {
			found = true
			break
		}
	}
	if found != want {
		t.Fatalf("finding %s for issue #%d = %t, want %t; findings=%+v", kind, issueNum, found, want, findings)
	}
}
