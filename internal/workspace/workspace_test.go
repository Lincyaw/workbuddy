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

func TestCreate_StaleMetadataPruneRescue(t *testing.T) {
	repoDir := initTestRepo(t)
	mgr := NewManager(repoDir)

	// Create a worktree.
	wtPath, err := mgr.Create(1, "task-1")
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}

	// Simulate unclean shutdown: delete the worktree directory but leave git metadata.
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatalf("RemoveAll worktree dir: %v", err)
	}

	// Re-create should succeed because prune cleans stale metadata.
	wtPath2, err := mgr.Create(1, "task-2")
	if err != nil {
		t.Fatalf("Create after stale metadata: %v", err)
	}
	if wtPath != wtPath2 {
		t.Fatalf("worktree path changed: %s vs %s", wtPath, wtPath2)
	}
	if _, err := os.Stat(wtPath2); os.IsNotExist(err) {
		t.Fatalf("worktree path does not exist after re-create: %s", wtPath2)
	}

	_ = mgr.Remove(wtPath2)
}

func TestCreate_ExistingCleanReuse(t *testing.T) {
	repoDir := initTestRepo(t)
	mgr := NewManager(repoDir)

	wtPath, err := mgr.Create(1, "task-1")
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}

	// Re-create for the same issue should reuse the existing worktree.
	wtPath2, err := mgr.Create(1, "task-2")
	if err != nil {
		t.Fatalf("Create reuse: %v", err)
	}
	if wtPath != wtPath2 {
		t.Fatalf("worktree path changed on reuse: %s vs %s", wtPath, wtPath2)
	}

	_ = mgr.Remove(wtPath)
}

func TestCreate_ExistingDirtyRefuse(t *testing.T) {
	repoDir := initTestRepo(t)
	mgr := NewManager(repoDir)

	wtPath, err := mgr.Create(1, "task-1")
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}

	// Dirty the worktree.
	if err := os.WriteFile(filepath.Join(wtPath, "dirty.txt"), []byte("x"), 0644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	// Re-create should fail because the worktree has uncommitted changes.
	_, err = mgr.Create(1, "task-2")
	if err == nil {
		t.Fatal("expected error for dirty worktree, got nil")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("expected 'uncommitted changes' in error, got: %v", err)
	}

	_ = mgr.Remove(wtPath)
}

func TestCreate_ExistingWrongBranchRefuse(t *testing.T) {
	repoDir := initTestRepo(t)
	mgr := NewManager(repoDir)

	wtPath, err := mgr.Create(1, "task-1")
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}

	cmd := exec.Command("git", "checkout", "-b", "workbuddy/issue-999")
	cmd.Dir = wtPath
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b: %s: %v", out, err)
	}

	_, err = mgr.Create(1, "task-2")
	if err == nil {
		t.Fatal("expected error for wrong-branch worktree, got nil")
	}
	if !strings.Contains(err.Error(), `expected "workbuddy/issue-1"`) {
		t.Fatalf("expected branch mismatch error, got: %v", err)
	}

	_ = mgr.Remove(wtPath)
}

func TestCreate_StaleAddFailure(t *testing.T) {
	repoDir := initTestRepo(t)
	mgr := NewManager(repoDir)

	wtPath := filepath.Join(repoDir, ".workbuddy", "worktrees", "issue-1")
	// Create a file (not directory) at the worktree path.
	// os.Stat sees it exists, but git commands fail because it's not a directory.
	if err := os.MkdirAll(filepath.Dir(wtPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(wtPath, []byte("stale"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := mgr.Create(1, "task-1")
	if err == nil {
		t.Fatal("expected error for invalid existing path, got nil")
	}
	if !strings.Contains(err.Error(), "not a valid worktree") {
		t.Fatalf("expected 'not a valid worktree' in error, got: %v", err)
	}
}
