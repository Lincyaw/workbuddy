package diagnose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

func TestAnalyzeHeartbeatOnlyZombieSignals(t *testing.T) {
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	cfg := diagnoseConfig{
		AgentTimeouts: map[string]time.Duration{"dev-agent": time.Hour},
		IdleThreshold: 5 * time.Minute,
	}

	t.Run("mtime stale but heartbeat fresh is flagged", func(t *testing.T) {
		restore := stubTaskProcesses(func() ([]taskProcess, error) { return nil, nil })
		defer restore()

		st, dbPath := newDiagnoseStore(t)
		defer func() { _ = st.Close() }()
		worktreeRoot := filepath.Join(filepath.Dir(dbPath), "issue-200")
		seedRunningTask(t, st, now, runningTaskFixture{
			repo:         "owner/repo",
			issueNum:     200,
			taskID:       "task-stale-log",
			state:        "developing",
			labels:       `["status:developing"]`,
			worktreeRoot: worktreeRoot,
			sessionAge:   11 * time.Minute,
			startedAt:    now.Add(-15 * time.Minute),
		})

		findings, err := analyzeWithConfig(st, "owner/repo", now, cfg)
		if err != nil {
			t.Fatalf("analyzeWithConfig: %v", err)
		}
		if len(findings) != 1 {
			t.Fatalf("findings=%d, want 1: %+v", len(findings), findings)
		}
		if got := findings[0].Diagnosis; !strings.Contains(got, "heartbeat-only zombie (session log static for") {
			t.Fatalf("diagnosis=%q", got)
		}
	})

	t.Run("no matching child process but heartbeat fresh is flagged", func(t *testing.T) {
		restore := stubTaskProcesses(func() ([]taskProcess, error) { return nil, nil })
		defer restore()

		st, dbPath := newDiagnoseStore(t)
		defer func() { _ = st.Close() }()
		worktreeRoot := filepath.Join(filepath.Dir(dbPath), "issue-201")
		seedRunningTask(t, st, now, runningTaskFixture{
			repo:         "owner/repo",
			issueNum:     201,
			taskID:       "task-no-child",
			state:        "developing",
			labels:       `["status:developing"]`,
			worktreeRoot: worktreeRoot,
			sessionAge:   2 * time.Minute,
			startedAt:    now.Add(-10 * time.Minute),
		})

		findings, err := analyzeWithConfig(st, "owner/repo", now, cfg)
		if err != nil {
			t.Fatalf("analyzeWithConfig: %v", err)
		}
		if len(findings) != 1 {
			t.Fatalf("findings=%d, want 1: %+v", len(findings), findings)
		}
		if got := findings[0].Diagnosis; got != "heartbeat-only zombie (no child process)" {
			t.Fatalf("diagnosis=%q", got)
		}
	})

	t.Run("fresh session log plus live child is not flagged", func(t *testing.T) {
		st, dbPath := newDiagnoseStore(t)
		defer func() { _ = st.Close() }()
		worktreeRoot := filepath.Join(filepath.Dir(dbPath), "issue-202")
		seedRunningTask(t, st, now, runningTaskFixture{
			repo:         "owner/repo",
			issueNum:     202,
			taskID:       "task-healthy",
			state:        "developing",
			labels:       `["status:developing"]`,
			worktreeRoot: worktreeRoot,
			sessionAge:   time.Minute,
			startedAt:    now.Add(-10 * time.Minute),
		})

		restore := stubTaskProcesses(func() ([]taskProcess, error) {
			return []taskProcess{{
				PID:  4242,
				Base: "codex",
				CWD:  worktreeRoot,
			}}, nil
		})
		defer restore()

		findings, err := analyzeWithConfig(st, "owner/repo", now, cfg)
		if err != nil {
			t.Fatalf("analyzeWithConfig: %v", err)
		}
		if len(findings) != 0 {
			t.Fatalf("findings=%+v, want none", findings)
		}
	})
}

func TestAnalyzePipelineHazard_MalformedDependencyRefIncludesLineHint(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	restore := stubTaskProcesses(func() ([]taskProcess, error) { return nil, nil })
	defer restore()

	st, _ := newDiagnoseStore(t)
	defer func() { _ = st.Close() }()

	const (
		repo     = "Lincyaw/workbuddy"
		issueNum = 293
	)

	body := "Prelude\n\n```yaml\nworkbuddy:\n  depends_on:\n    - \\\"#292\\\"\n    - garbage\n```\n"
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     repo,
		IssueNum: issueNum,
		Labels:   `["workbuddy","status:developing"]`,
		Body:     body,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	if _, err := st.UpsertIssuePipelineHazard(store.PipelineHazard{
		Repo:        repo,
		IssueNum:    issueNum,
		Kind:        store.HazardKindMalformedDependencyRef,
		Fingerprint: "fp",
	}); err != nil {
		t.Fatalf("UpsertIssuePipelineHazard: %v", err)
	}

	findings, err := analyzeWithConfig(st, repo, now, diagnoseConfig{})
	if err != nil {
		t.Fatalf("analyzeWithConfig: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings=%d, want 1: %+v", len(findings), findings)
	}
	if findings[0].Kind != KindPipelineHazard {
		t.Fatalf("kind=%q, want %q", findings[0].Kind, KindPipelineHazard)
	}
	if !strings.Contains(findings[0].Diagnosis, `line 7`) || !strings.Contains(findings[0].Diagnosis, `"garbage"`) {
		t.Fatalf("diagnosis=%q, want line-specific malformed ref context", findings[0].Diagnosis)
	}
	if !strings.Contains(findings[0].SuggestedFix, "line 7") || !strings.Contains(findings[0].SuggestedFix, "`garbage`") {
		t.Fatalf("suggested_fix=%q, want line-specific edit hint", findings[0].SuggestedFix)
	}
}

type runningTaskFixture struct {
	repo         string
	issueNum     int
	taskID       string
	state        string
	labels       string
	worktreeRoot string
	sessionAge   time.Duration
	startedAt    time.Time
}

func newDiagnoseStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "diagnose.db")
	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return st, dbPath
}

func seedRunningTask(t *testing.T, st *store.Store, now time.Time, fx runningTaskFixture) {
	t.Helper()
	if err := st.UpsertIssueCache(store.IssueCache{
		Repo:     fx.repo,
		IssueNum: fx.issueNum,
		Labels:   fx.labels,
		State:    "open",
	}); err != nil {
		t.Fatalf("UpsertIssueCache: %v", err)
	}
	if err := st.InsertTask(store.TaskRecord{
		ID:        fx.taskID,
		Repo:      fx.repo,
		IssueNum:  fx.issueNum,
		AgentName: "dev-agent",
		Runtime:   "codex",
		State:     fx.state,
		Status:    store.TaskStatusRunning,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	if _, err := st.DB().Exec(
		`UPDATE task_queue SET created_at = ?, acked_at = ?, heartbeat_at = ?, updated_at = ? WHERE id = ?`,
		fx.startedAt.UTC().Format(time.RFC3339),
		fx.startedAt.UTC().Format(time.RFC3339),
		now.Add(-30*time.Second).UTC().Format(time.RFC3339),
		now.Add(-30*time.Second).UTC().Format(time.RFC3339),
		fx.taskID,
	); err != nil {
		t.Fatalf("seed task timestamps: %v", err)
	}

	sessionDir := filepath.Join(fx.worktreeRoot, ".workbuddy", "sessions", "session-"+fx.taskID)
	if _, err := st.CreateSession(store.SessionRecord{
		SessionID:  "session-" + fx.taskID,
		TaskID:     fx.taskID,
		Repo:       fx.repo,
		IssueNum:   fx.issueNum,
		AgentName:  "dev-agent",
		Runtime:    "codex",
		WorkerID:   "worker-1",
		Attempt:    1,
		Status:     store.TaskStatusRunning,
		Dir:        sessionDir,
		StdoutPath: filepath.Join(sessionDir, "stdout"),
		StderrPath: filepath.Join(sessionDir, "stderr"),
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	eventsPath := filepath.Join(sessionDir, "events-v1.jsonl")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(sessionDir): %v", err)
	}
	if err := os.WriteFile(eventsPath, []byte("{\"kind\":\"log\"}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(events): %v", err)
	}
	mtime := now.Add(-fx.sessionAge)
	if err := os.Chtimes(eventsPath, mtime, mtime); err != nil {
		t.Fatalf("Chtimes(events): %v", err)
	}
}

func stubTaskProcesses(fn func() ([]taskProcess, error)) func() {
	orig := listTaskProcesses
	listTaskProcesses = fn
	return func() { listTaskProcesses = orig }
}
