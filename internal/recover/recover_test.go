package recover

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test User"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, strings.TrimSpace(string(out)), err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "README.md")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %s: %v", strings.TrimSpace(string(out)), err)
	}
	cmd = exec.Command("git", "commit", "-m", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %s: %v", strings.TrimSpace(string(out)), err)
	}
	return dir
}

func TestParseProcessList(t *testing.T) {
	raw := `
  101 1 45 codex exec --model gpt-5
  202 1 120 /tmp/workbuddy serve --port 8080
  303 1 5 bash -lc echo nope
`
	processes, err := ParseProcessList(raw)
	if err != nil {
		t.Fatalf("ParseProcessList: %v", err)
	}
	if len(processes) != 3 {
		t.Fatalf("len(processes) = %d, want 3", len(processes))
	}
	if !processes[0].matchesTarget() {
		t.Fatalf("codex process should match target: %+v", processes[0])
	}
	if !processes[1].matchesTarget() {
		t.Fatalf("workbuddy serve process should match target: %+v", processes[1])
	}
	if processes[2].matchesTarget() {
		t.Fatalf("bash process should not match target: %+v", processes[2])
	}
}

func TestResetDBClearsRuntimeTablesAndKeepsTransitionCounts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "workbuddy.db")
	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	if _, err := st.InsertEvent(store.Event{Type: "tick", Repo: "owner/repo", IssueNum: 7, Payload: "{}"}); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if err := st.InsertTask(store.TaskRecord{ID: "task-1", Repo: "owner/repo", IssueNum: 7, AgentName: "dev-agent", Status: store.TaskStatusPending}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	if _, err := st.DB().Exec(`INSERT INTO issue_cache (repo, issue_num, labels, body, state) VALUES (?, ?, ?, ?, ?)`, "owner/repo", 7, "status:developing", "body", "OPEN"); err != nil {
		t.Fatalf("insert issue_cache: %v", err)
	}
	if _, err := st.DB().Exec(`INSERT INTO issue_dependency_state (repo, issue_num, verdict, resume_label, blocked_reason_hash, override_active, graph_version, last_reaction_blocked) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, "owner/repo", 7, "blocked", "status:blocked", "hash", 0, 1, 1); err != nil {
		t.Fatalf("insert issue_dependency_state: %v", err)
	}
	if _, err := st.DB().Exec(`INSERT INTO transition_counts (repo, issue_num, from_state, to_state, count) VALUES (?, ?, ?, ?, ?)`, "owner/repo", 7, "status:triage", "status:developing", 2); err != nil {
		t.Fatalf("insert transition_counts: %v", err)
	}
	_ = st.Close()

	var out bytes.Buffer
	if err := resetDB(dbPath, Options{Stdout: &out}); err != nil {
		t.Fatalf("resetDB: %v", err)
	}
	for _, table := range runtimeTables {
		if !strings.Contains(out.String(), table) {
			t.Fatalf("reset output missing table %q: %s", table, out.String())
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, table := range runtimeTables {
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s row count = %d, want 0", table, count)
		}
	}

	var transitions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM transition_counts`).Scan(&transitions); err != nil {
		t.Fatalf("count transition_counts: %v", err)
	}
	if transitions != 1 {
		t.Fatalf("transition_counts row count = %d, want 1", transitions)
	}

	var schemaCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='issue_dependency_state'`).Scan(&schemaCount); err != nil {
		t.Fatalf("check schema: %v", err)
	}
	if schemaCount != 1 {
		t.Fatalf("issue_dependency_state schema missing after reset")
	}
}

func TestRunDryRunPrintsActions(t *testing.T) {
	repo := initGitRepo(t)
	worktreeDir := filepath.Join(repo, ".workbuddy", "worktrees", "issue-1")
	if err := os.MkdirAll(worktreeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(repo, ".workbuddy", "workbuddy.db")
	st, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	_ = st.Close()

	var out bytes.Buffer
	err = Run(context.Background(), Options{
		RepoRoot:       repo,
		CommonRoot:     repo,
		KillZombies:    true,
		PruneWorktrees: true,
		ResetDB:        true,
		DryRun:         true,
		Stdout:         &out,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"No matching zombie processes found.",
		"Would remove worktree path",
		"Would run git worktree prune",
		"Would clear sqlite table events",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, got)
		}
	}
}

func TestRunKillZombiesTerminatesDummyProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("kill-zombies test requires /proc cwd lookup on linux")
	}

	repo := initGitRepo(t)
	codexPath := filepath.Join(t.TempDir(), "codex")
	if err := os.Symlink("/bin/sleep", codexPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	cmd := exec.Command(codexPath, "30")
	cmd.Dir = repo
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dummy codex: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	time.Sleep(150 * time.Millisecond)

	var out bytes.Buffer
	if err := Run(context.Background(), Options{
		RepoRoot:    repo,
		CommonRoot:  repo,
		KillZombies: true,
		Stdout:      &out,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "Terminating process") {
		t.Fatalf("kill-zombies output missing termination message: %s", out.String())
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-time.After(5 * time.Second):
		t.Fatal("dummy codex process was not terminated")
	case err := <-done:
		if err == nil {
			return
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == -1 || exitErr.ExitCode() == 143 || exitErr.ExitCode() == 137 {
				return
			}
		}
		t.Fatalf("unexpected wait error after termination: %v", err)
	}
}

func TestPruneWorktreesRemovesEntries(t *testing.T) {
	repo := initGitRepo(t)
	for _, name := range []string{"issue-1", "issue-2"} {
		if err := os.MkdirAll(filepath.Join(repo, ".workbuddy", "worktrees", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	if err := pruneWorktrees(context.Background(), repo, Options{Stdout: &out}); err != nil {
		t.Fatalf("pruneWorktrees: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(repo, ".workbuddy", "worktrees"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("worktree entries remain after prune: %d", len(entries))
	}
	if !strings.Contains(out.String(), "Running git worktree prune") {
		t.Fatalf("output missing git worktree prune: %s", out.String())
	}
}
