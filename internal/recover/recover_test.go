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

func TestIsRecoverableProcess(t *testing.T) {
	t.Run("skips current session child", func(t *testing.T) {
		processes := []Process{
			{PID: 7, PPID: 1000, ElapsedSeconds: 10, Args: []string{"bash"}},
			{PID: 42, PPID: 7, ElapsedSeconds: 3, Args: []string{"codex", "exec"}},
		}
		if isRecoverableProcess(processes, processes[1], 100, 7, 10, true) {
			t.Fatalf("expected current-session child to be skipped")
		}
	})

	t.Run("matches orphaned target", func(t *testing.T) {
		proc := Process{PID: 42, PPID: 1, ElapsedSeconds: 1, Args: []string{"codex", "exec"}}
		if !isRecoverableProcess([]Process{proc}, proc, 100, 7, 10, true) {
			t.Fatalf("expected orphaned target to be recoverable")
		}
	})

	t.Run("matches process outside current shell session", func(t *testing.T) {
		processes := []Process{
			{PID: 7, PPID: 1000, ElapsedSeconds: 10, Args: []string{"bash"}},
			{PID: 42, PPID: 88, ElapsedSeconds: 3, Args: []string{"codex", "exec"}},
			{PID: 88, PPID: 2000, ElapsedSeconds: 4, Args: []string{"sh"}},
		}
		if !isRecoverableProcess(processes, processes[1], 100, 7, 10, true) {
			t.Fatalf("expected process outside current shell session to be recoverable")
		}
	})

	t.Run("matches process older than shell session", func(t *testing.T) {
		processes := []Process{
			{PID: 7, PPID: 1000, ElapsedSeconds: 10, Args: []string{"bash"}},
			{PID: 8, PPID: 7, ElapsedSeconds: 25, Args: []string{"workbuddy", "serve"}},
		}
		if !isRecoverableProcess(processes, processes[1], 100, 7, 10, true) {
			t.Fatalf("expected older target to be recoverable")
		}
	})
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

func TestRunKillZombiesTerminatesProcessOutsideCurrentSession(t *testing.T) {
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
	pid := cmd.Process.Pid
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
		CurrentPID:  os.Getpid(),
		ShellPID:    999999,
		Stdout:      &out,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "Terminating process") {
		t.Fatalf("kill-zombies output missing termination message: %s", out.String())
	}
	waitForProcessExit(t, pid)
}

func TestRunKillZombiesSkipsActiveCurrentSessionProcess(t *testing.T) {
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
		CurrentPID:  os.Getpid(),
		ShellPID:    os.Getpid(),
		Stdout:      &out,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "No matching zombie processes found.") {
		t.Fatalf("expected active process to be skipped, got: %s", out.String())
	}
	if !processExists(cmd.Process.Pid) {
		t.Fatalf("active current-session process was terminated")
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

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processExists(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process %d was not terminated", pid)
}
