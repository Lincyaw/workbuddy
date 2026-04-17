package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initTestRepo creates a bare-minimum git repo in a temp directory.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}

	// Need at least one commit for worktree to reference HEAD.
	marker := filepath.Join(dir, "README.md")
	if err := os.WriteFile(marker, []byte("test repo"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", ".")
	cmd.Dir = dir
	_ = cmd.Run()
	cmd = exec.Command("git", "commit", "-m", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %s: %v", out, err)
	}

	return dir
}

func TestCreateAndRemove(t *testing.T) {
	repoDir := initTestRepo(t)
	mgr := NewManager(repoDir)

	wtPath, err := mgr.Create(42, "task-abc12345-def")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Worktree directory should exist.
	if _, err := os.Stat(wtPath); os.IsNotExist(err) {
		t.Fatalf("worktree path does not exist: %s", wtPath)
	}

	// Should contain the repo files.
	readme := filepath.Join(wtPath, "README.md")
	if _, err := os.Stat(readme); os.IsNotExist(err) {
		t.Error("README.md not found in worktree")
	}

	// Verify the branch was created.
	cmd := exec.Command("git", "branch", "--list", "workbuddy/issue-42")
	cmd.Dir = repoDir
	out, _ := cmd.Output()
	if !strings.Contains(string(out), "workbuddy/issue-42") {
		t.Errorf("expected workbuddy branch, got: %s", out)
	}

	// Remove the worktree.
	if err := mgr.Remove(wtPath); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Directory should be gone.
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Error("worktree directory still exists after Remove")
	}

	// Branch should be gone.
	cmd = exec.Command("git", "branch", "--list", "workbuddy/issue-42")
	cmd.Dir = repoDir
	out, _ = cmd.Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("branch still exists after Remove: %s", out)
	}
}

func TestMultipleWorktrees(t *testing.T) {
	repoDir := initTestRepo(t)
	mgr := NewManager(repoDir)

	wt1, err := mgr.Create(1, "task-aaaa1111")
	if err != nil {
		t.Fatalf("Create wt1: %v", err)
	}
	wt2, err := mgr.Create(2, "task-bbbb2222")
	if err != nil {
		t.Fatalf("Create wt2: %v", err)
	}

	// Both should exist and be different paths.
	if wt1 == wt2 {
		t.Error("worktree paths should be different")
	}
	for _, wt := range []string{wt1, wt2} {
		if _, err := os.Stat(wt); os.IsNotExist(err) {
			t.Errorf("worktree does not exist: %s", wt)
		}
	}

	// Changes in one worktree should not affect the other.
	if err := os.WriteFile(filepath.Join(wt1, "file1.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("write file in wt1: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt2, "file1.txt")); !os.IsNotExist(err) {
		t.Error("file created in wt1 should not appear in wt2")
	}

	// Cleanup.
	_ = mgr.Remove(wt1)
	_ = mgr.Remove(wt2)
}

func TestCreateUsesTaskScopedPathForSameIssue(t *testing.T) {
	repoDir := initTestRepo(t)
	mgr := NewManager(repoDir)

	wt1, err := mgr.Create(7, "task-alpha")
	if err != nil {
		t.Fatalf("Create wt1: %v", err)
	}
	wt2, err := mgr.Create(7, "task-beta")
	if err != nil {
		t.Fatalf("Create wt2: %v", err)
	}

	if wt1 == wt2 {
		t.Fatalf("expected task-scoped worktree paths, got identical path %q", wt1)
	}
	if !strings.HasSuffix(wt1, filepath.Join(".workbuddy", "worktrees", "issue-7-task-alpha")) {
		t.Fatalf("wt1 path = %q", wt1)
	}
	if !strings.HasSuffix(wt2, filepath.Join(".workbuddy", "worktrees", "issue-7-task-beta")) {
		t.Fatalf("wt2 path = %q", wt2)
	}
	if _, err := os.Stat(wt1); !os.IsNotExist(err) {
		t.Fatalf("expected previous worktree %q to be removed, stat err = %v", wt1, err)
	}
	if _, err := os.Stat(wt2); err != nil {
		t.Fatalf("expected replacement worktree %q to exist: %v", wt2, err)
	}

	_ = mgr.Remove(wt2)
}
