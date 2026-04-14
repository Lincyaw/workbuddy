// Package workspace provides per-issue workspace isolation using git worktrees.
// Each agent task gets its own worktree so multiple agents can work on different
// issues concurrently without git conflicts.
package workspace

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

const worktreeDir = ".workbuddy/worktrees"

// Manager handles creation and cleanup of git worktrees for agent tasks.
type Manager struct {
	baseDir string // root of the main repo (where .git lives)
	mu      sync.Mutex
}

// NewManager creates a Manager rooted at the given repo directory.
func NewManager(baseDir string) *Manager {
	return &Manager{baseDir: baseDir}
}

// Create creates a new git worktree for the given task and returns its path.
// The worktree is created at .workbuddy/worktrees/<issue>-<taskID>/ branching
// from the current HEAD of the main worktree.
func (m *Manager) Create(issueNum int, taskID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	name := fmt.Sprintf("issue-%d-%s", issueNum, shortID(taskID))
	wtPath := filepath.Join(m.baseDir, worktreeDir, name)

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(wtPath), 0755); err != nil {
		return "", fmt.Errorf("workspace: mkdir: %w", err)
	}

	// Create a new branch for isolation. Branch name includes issue number
	// so multiple tasks for the same issue get separate branches.
	branchName := fmt.Sprintf("workbuddy/issue-%d/%s", issueNum, shortID(taskID))

	cmd := exec.Command("git", "worktree", "add", "-b", branchName, wtPath, "HEAD")
	cmd.Dir = m.baseDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("workspace: git worktree add: %s: %w", strings.TrimSpace(string(out)), err)
	}

	log.Printf("[workspace] created worktree %s (branch %s) for issue #%d task %s",
		wtPath, branchName, issueNum, shortID(taskID))
	return wtPath, nil
}

// Remove cleans up a worktree and its associated branch.
// Returns a combined error if worktree removal or branch deletion fails.
func (m *Manager) Remove(wtPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error

	// Get the branch name before removing the worktree.
	branchName := worktreeBranch(m.baseDir, wtPath)

	// Remove the worktree.
	cmd := exec.Command("git", "worktree", "remove", "--force", wtPath)
	cmd.Dir = m.baseDir
	if out, err := cmd.CombinedOutput(); err != nil {
		// If already removed (e.g. directory deleted), just prune.
		log.Printf("[workspace] worktree remove warning: %s: %v", strings.TrimSpace(string(out)), err)
		pruneCmd := exec.Command("git", "worktree", "prune")
		pruneCmd.Dir = m.baseDir
		if pruneErr := pruneCmd.Run(); pruneErr != nil {
			errs = append(errs, fmt.Errorf("workspace: worktree remove %s: %s: %w; prune also failed: %v",
				wtPath, strings.TrimSpace(string(out)), err, pruneErr))
		} else {
			// Prune succeeded, so the worktree is cleaned up; not a hard error.
			log.Printf("[workspace] worktree pruned successfully after remove failure")
		}
	}

	// Delete the temporary branch.
	if branchName != "" {
		delCmd := exec.Command("git", "branch", "-D", branchName)
		delCmd.Dir = m.baseDir
		if out, err := delCmd.CombinedOutput(); err != nil {
			errs = append(errs, fmt.Errorf("workspace: branch delete %s: %s: %w",
				branchName, strings.TrimSpace(string(out)), err))
		}
	}

	log.Printf("[workspace] removed worktree %s", wtPath)
	return errors.Join(errs...)
}

// worktreeBranch returns the branch checked out in the given worktree path.
func worktreeBranch(repoDir, wtPath string) string {
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	absWT, _ := filepath.Abs(wtPath)
	lines := strings.Split(string(out), "\n")
	found := false
	for _, line := range lines {
		if strings.HasPrefix(line, "worktree ") {
			path := strings.TrimPrefix(line, "worktree ")
			absPath, _ := filepath.Abs(path)
			found = (absPath == absWT)
		}
		if found && strings.HasPrefix(line, "branch ") {
			ref := strings.TrimPrefix(line, "branch ")
			// ref is like "refs/heads/workbuddy/issue-5/abc123"
			return strings.TrimPrefix(ref, "refs/heads/")
		}
		if line == "" {
			found = false
		}
	}
	return ""
}

// Prune cleans up orphaned worktrees left behind by crashes or unclean shutdowns.
func (m *Manager) Prune() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Let git clean up stale worktree references.
	cmd := exec.Command("git", "worktree", "prune")
	cmd.Dir = m.baseDir
	_ = cmd.Run()

	// Remove any leftover directories under worktreeDir.
	wtDir := filepath.Join(m.baseDir, worktreeDir)
	entries, err := os.ReadDir(wtDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			path := filepath.Join(wtDir, entry.Name())
			log.Printf("[workspace] prune: removing orphaned worktree directory %s", path)
			_ = os.RemoveAll(path)
		}
	}
}

// shortID returns first 8 chars of a UUID/task ID for readable directory names.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
