package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

func TestRunDiagnoseWithOpts(t *testing.T) {
	now := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)

	t.Run("healthy pipeline", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "healthy.db")
		st, err := store.NewStore(dbPath)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		_ = st.Close()

		var out bytes.Buffer
		err = runDiagnoseWithOpts(context.Background(), &diagnoseOpts{
			dbPath: dbPath,
			now:    func() time.Time { return now },
		}, &out)
		if err != nil {
			t.Fatalf("runDiagnoseWithOpts: %v", err)
		}
		if strings.TrimSpace(out.String()) != "Pipeline healthy: no issues detected" {
			t.Fatalf("unexpected output: %q", out.String())
		}
	})

	t.Run("fix applies cache invalidate and auto_fix event", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "fix.db")
		st, err := store.NewStore(dbPath)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		if err := st.UpsertIssueCache(store.IssueCache{Repo: "owner/repo", IssueNum: 47, Labels: `["status:developing"]`, State: "open"}); err != nil {
			t.Fatalf("UpsertIssueCache: %v", err)
		}
		if err := st.UpsertIssueDependencyState(store.IssueDependencyState{Repo: "owner/repo", IssueNum: 47, Verdict: store.DependencyVerdictReady}); err != nil {
			t.Fatalf("UpsertIssueDependencyState: %v", err)
		}
		if _, err := st.InsertEvent(store.Event{Type: "state_entry", Repo: "owner/repo", IssueNum: 47, Payload: `{"state":"status:developing"}`}); err != nil {
			t.Fatalf("InsertEvent: %v", err)
		}
		_ = st.Close()

		var out bytes.Buffer
		err = runDiagnoseWithOpts(context.Background(), &diagnoseOpts{
			repo:    "owner/repo",
			dbPath:  dbPath,
			fix:     true,
			jsonOut: true,
			now:     func() time.Time { return now },
		}, &out)
		var exitErr *cliExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("expected exit code 1, got %v", err)
		}

		var rows []diagnoseResult
		if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(rows) == 0 || !rows[0].FixApplied {
			t.Fatalf("expected applied fix, got %+v", rows)
		}

		st2, err := store.NewStore(dbPath)
		if err != nil {
			t.Fatalf("reopen store: %v", err)
		}
		defer func() { _ = st2.Close() }()
		cache, err := st2.QueryIssueCache("owner/repo", 47)
		if err != nil {
			t.Fatalf("QueryIssueCache: %v", err)
		}
		if cache != nil {
			t.Fatalf("cache still present: %+v", cache)
		}
		events, err := st2.QueryEvents("owner/repo")
		if err != nil {
			t.Fatalf("QueryEvents: %v", err)
		}
		gotAutoFix := false
		for _, event := range events {
			if event.Type == "auto_fix" {
				gotAutoFix = true
			}
		}
		if !gotAutoFix {
			t.Fatalf("expected auto_fix event, got %+v", events)
		}
	})

	t.Run("fix marks heartbeat zombie completed when label advanced", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "complete.db")
		st, err := store.NewStore(dbPath)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		sessionDir := filepath.Join(filepath.Dir(dbPath), "issue-142", ".workbuddy", "sessions", "session-task-complete")
		if err := seedHeartbeatZombieTask(st, sessionDir, "task-complete", 142, `["status:reviewing"]`, now, 15*time.Minute); err != nil {
			t.Fatalf("seedHeartbeatZombieTask: %v", err)
		}
		_ = st.Close()

		var out bytes.Buffer
		err = runDiagnoseWithOpts(context.Background(), &diagnoseOpts{
			repo:    "owner/repo",
			dbPath:  dbPath,
			fix:     true,
			jsonOut: true,
			now:     func() time.Time { return now },
		}, &out)
		var exitErr *cliExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("expected exit code 1, got %v", err)
		}

		st2, err := store.NewStore(dbPath)
		if err != nil {
			t.Fatalf("reopen store: %v", err)
		}
		defer func() { _ = st2.Close() }()
		task, err := st2.GetTask("task-complete")
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if task.Status != store.TaskStatusCompleted {
			t.Fatalf("status=%q, want completed", task.Status)
		}
		if task.LeaseExpiresAt != (time.Time{}) {
			t.Fatalf("lease_expires_at=%s, want zero", task.LeaseExpiresAt)
		}
	})

	t.Run("fix marks heartbeat zombie failed when work did not land", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "failed.db")
		st, err := store.NewStore(dbPath)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		sessionDir := filepath.Join(filepath.Dir(dbPath), "issue-143", ".workbuddy", "sessions", "session-task-fail")
		if err := seedHeartbeatZombieTask(st, sessionDir, "task-fail", 143, `["status:developing"]`, now, 15*time.Minute); err != nil {
			t.Fatalf("seedHeartbeatZombieTask: %v", err)
		}
		_ = st.Close()

		var out bytes.Buffer
		err = runDiagnoseWithOpts(context.Background(), &diagnoseOpts{
			repo:    "owner/repo",
			dbPath:  dbPath,
			fix:     true,
			jsonOut: true,
			now:     func() time.Time { return now },
		}, &out)
		var exitErr *cliExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("expected exit code 1, got %v", err)
		}

		st2, err := store.NewStore(dbPath)
		if err != nil {
			t.Fatalf("reopen store: %v", err)
		}
		defer func() { _ = st2.Close() }()
		task, err := st2.GetTask("task-fail")
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if task.Status != store.TaskStatusFailed {
			t.Fatalf("status=%q, want failed", task.Status)
		}
		if task.LeaseExpiresAt != (time.Time{}) {
			t.Fatalf("lease_expires_at=%s, want zero", task.LeaseExpiresAt)
		}
	})
}

func TestDiagnoseHelpTextUsesFormatJSONCanonically(t *testing.T) {
	if strings.Contains(diagnoseCmd.Long, "Use --json when piping into another tool.") {
		t.Fatalf("diagnose help still advertises --json as canonical: %q", diagnoseCmd.Long)
	}
	if !strings.Contains(diagnoseCmd.Long, "Use --format json when piping into another tool.") {
		t.Fatalf("diagnose help missing canonical --format json guidance: %q", diagnoseCmd.Long)
	}
}

func seedHeartbeatZombieTask(st *store.Store, sessionDir, taskID string, issueNum int, labels string, now time.Time, sessionAge time.Duration) error {
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     "owner/repo",
		IssueNum: issueNum,
		Labels:   labels,
		State:    "open",
	}); err != nil {
		return err
	}
	if err := st.InsertTask(store.TaskRecord{
		ID:        taskID,
		Repo:      "owner/repo",
		IssueNum:  issueNum,
		AgentName: "dev-agent",
		Runtime:   "codex",
		State:     "developing",
		Status:    store.TaskStatusRunning,
	}); err != nil {
		return err
	}
	if _, err := st.DB().Exec(
		`UPDATE task_queue
		 SET created_at = ?, acked_at = ?, heartbeat_at = ?, updated_at = ?, lease_expires_at = ?
		 WHERE id = ?`,
		now.Add(-20*time.Minute).UTC().Format(time.RFC3339),
		now.Add(-20*time.Minute).UTC().Format(time.RFC3339),
		now.Add(-30*time.Second).UTC().Format(time.RFC3339),
		now.Add(-30*time.Second).UTC().Format(time.RFC3339),
		now.Add(5*time.Minute).UTC().Format(time.RFC3339),
		taskID,
	); err != nil {
		return err
	}
	if _, err := st.CreateSession(store.SessionRecord{
		SessionID:  "session-" + taskID,
		TaskID:     taskID,
		Repo:       "owner/repo",
		IssueNum:   issueNum,
		AgentName:  "dev-agent",
		Runtime:    "codex",
		WorkerID:   "worker-1",
		Attempt:    1,
		Status:     store.TaskStatusRunning,
		Dir:        sessionDir,
		StdoutPath: filepath.Join(sessionDir, "stdout"),
		StderrPath: filepath.Join(sessionDir, "stderr"),
	}); err != nil {
		return err
	}
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return err
	}
	eventsPath := filepath.Join(sessionDir, "events-v1.jsonl")
	if err := os.WriteFile(eventsPath, []byte("{\"kind\":\"turn.completed\"}\n"), 0o644); err != nil {
		return err
	}
	mtime := now.Add(-sessionAge)
	return os.Chtimes(eventsPath, mtime, mtime)
}
