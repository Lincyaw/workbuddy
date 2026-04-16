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
// The worktree is created at .workbuddy/worktrees/<issue>/ branching from
// the current HEAD (or from origin/workbuddy/issue-<issue> if it exists).
// Branch names are deterministic per issue so work persists across cycles.
func (m *Manager) Create(issueNum int, taskID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	branchName := fmt.Sprintf("workbuddy/issue-%d", issueNum)
	wtPath := filepath.Join(m.baseDir, worktreeDir, fmt.Sprintf("issue-%d", issueNum))

	// Remove any existing worktree for this issue to avoid conflicts.
	if existing := m.findWorktreePath(branchName); existing != "" {
		_ = exec.Command("git", "worktree", "remove", "--force", existing).Run()
		_ = exec.Command("git", "worktree", "prune").Run()
	}
	_ = os.RemoveAll(wtPath)

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(wtPath), 0755); err != nil {
		return "", fmt.Errorf("workspace: mkdir: %w", err)
	}

	// If the local branch already exists, create worktree from it directly.
	// Otherwise branch from origin/branch (if it exists) or HEAD.
	if m.localBranchExists(branchName) {
		cmd := exec.Command("git", "worktree", "add", wtPath, branchName)
		cmd.Dir = m.baseDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("workspace: git worktree add from existing branch: %s: %w", strings.TrimSpace(string(out)), err)
		}
		log.Printf("[workspace] created worktree %s (branch %s from local) for issue #%d",
			wtPath, branchName, issueNum)
	} else {
		baseRef := "HEAD"
		if m.remoteBranchExists(branchName) {
			baseRef = "origin/" + branchName
		}
		cmd := exec.Command("git", "worktree", "add", "-b", branchName, wtPath, baseRef)
		cmd.Dir = m.baseDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("workspace: git worktree add: %s: %w", strings.TrimSpace(string(out)), err)
		}
		log.Printf("[workspace] created worktree %s (branch %s from %s) for issue #%d",
			wtPath, branchName, baseRef, issueNum)
	}
	return wtPath, nil
}

// Remove cleans up a worktree and its associated branch.
// Best-effort: if worktree removal fails but prune succeeds, no error is returned.
// Returns a combined error only when cleanup truly fails (both remove+prune fail,
// or branch deletion fails).
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

	combined := errors.Join(errs...)
	if combined != nil {
		log.Printf("[workspace] partial cleanup for worktree %s: %v", wtPath, combined)
	} else {
		log.Printf("[workspace] removed worktree %s", wtPath)
	}
	return combined
}

// findWorktreePath returns the filesystem path of an existing worktree for the
// given branch, or empty string if none exists.
func (m *Manager) findWorktreePath(branchName string) string {
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = m.baseDir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	lines := strings.Split(string(out), "\n")
	var currentPath string
	for _, line := range lines {
		if strings.HasPrefix(line, "worktree ") {
			currentPath = strings.TrimPrefix(line, "worktree ")
		}
		if strings.HasPrefix(line, "branch ") {
			ref := strings.TrimPrefix(line, "branch ")
			localBranch := strings.TrimPrefix(ref, "refs/heads/")
			if localBranch == branchName && currentPath != "" {
				return currentPath
			}
		}
		if line == "" {
			currentPath = ""
		}
	}
	return ""
}

// localBranchExists reports whether a local branch <branchName> exists.
func (m *Manager) localBranchExists(branchName string) bool {
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+branchName)
	cmd.Dir = m.baseDir
	return cmd.Run() == nil
}

// remoteBranchExists reports whether origin/<branchName> exists.
func (m *Manager) remoteBranchExists(branchName string) bool {
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/remotes/origin/"+branchName)
	cmd.Dir = m.baseDir
	return cmd.Run() == nil
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
// It only removes directories that are NOT still registered as valid git worktrees.
func (m *Manager) Prune() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Let git clean up stale worktree references.
	cmd := exec.Command("git", "worktree", "prune")
	cmd.Dir = m.baseDir
	_ = cmd.Run()

	// Build a set of valid worktree paths.
	validWorktrees, err := m.listWorktreePaths()
	if err != nil {
		log.Printf("[workspace] prune: failed to list worktrees, skipping deletion: %v", err)
		return fmt.Errorf("workspace: prune: list worktrees: %w", err)
	}

	// Remove any leftover directories under worktreeDir that are no longer valid worktrees.
	wtDir := filepath.Join(m.baseDir, worktreeDir)
	entries, err := os.ReadDir(wtDir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() {
			path := filepath.Join(wtDir, entry.Name())
			absPath, _ := filepath.Abs(path)
			if validWorktrees[absPath] {
				log.Printf("[workspace] prune: keeping active worktree directory %s", path)
				continue
			}
			log.Printf("[workspace] prune: removing orphaned worktree directory %s", path)
			_ = os.RemoveAll(path)
		}
	}
	return nil
}

// listWorktreePaths returns the set of filesystem paths currently registered
// as git worktrees (including the main repo directory).
func (m *Manager) listWorktreePaths() (map[string]bool, error) {
	paths := make(map[string]bool)
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = m.baseDir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "worktree ") {
			path := strings.TrimPrefix(line, "worktree ")
			absPath, _ := filepath.Abs(path)
			paths[absPath] = true
		}
	}
	return paths, nil
}

// shortID returns first 8 chars of a UUID/task ID for readable directory names.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
