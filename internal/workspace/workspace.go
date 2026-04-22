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
//
// When multiple agents share the same per-issue worktree (e.g. dev-agent
// finishes and review-agent starts while dev-agent's cleanup is still
// pending), the manager tracks an active-session reference count per
// worktree path. Remove only performs the actual git removal when the last
// active reference releases; earlier Remove calls decrement the count and
// return nil so a late defer from a finished agent cannot pull the
// filesystem out from under a still-running one.
type Manager struct {
	baseDir string // root of the main repo (where .git lives)
	mu      sync.Mutex
	refs    map[string]int // absolute worktree path -> active session count
}

// NewManager creates a Manager rooted at the given repo directory.
func NewManager(baseDir string) *Manager {
	return &Manager{baseDir: baseDir, refs: make(map[string]int)}
}

// refKey returns the canonical refcount key for a worktree path.
func refKey(wtPath string) string {
	abs, err := filepath.Abs(wtPath)
	if err != nil {
		return wtPath
	}
	return abs
}

// Create creates a new git worktree for the given task and returns its path.
// The worktree is created at .workbuddy/worktrees/<issue>/ branching from
// the current HEAD (or from origin/workbuddy/issue-<issue> if it exists).
// Branch names are deterministic per issue so work persists across cycles.
//
// Before every add, it runs `git worktree prune` to clean stale metadata.
// If the worktree path already exists and is on the expected branch with
// no uncommitted changes, it is reused. Otherwise, the call fails loudly
// and never falls back to the main working tree.
func (m *Manager) Create(issueNum int, taskID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	branchName := fmt.Sprintf("workbuddy/issue-%d", issueNum)
	wtPath := filepath.Join(m.baseDir, worktreeDir, fmt.Sprintf("issue-%d", issueNum))

	// 1. Prune stale worktree metadata (cheap, idempotent).
	pruneCmd := exec.Command("git", "worktree", "prune")
	pruneCmd.Dir = m.baseDir
	if out, err := pruneCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("workspace: git worktree prune: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// 2. Path exists — validate branch and cleanliness, then reuse or fail.
	if _, err := os.Stat(wtPath); err == nil {
		if !m.isRegisteredWorktree(wtPath) {
			return "", fmt.Errorf("workspace: path %s exists but is not a registered git worktree", wtPath)
		}
		curBranch, err := m.gitCurrentBranch(wtPath)
		if err != nil {
			return "", fmt.Errorf("workspace: path %s exists but is not a valid worktree: %w", wtPath, err)
		}
		if curBranch != branchName {
			return "", fmt.Errorf("workspace: worktree %s is on branch %q, expected %q — refusing to reuse", wtPath, curBranch, branchName)
		}
		if !m.gitIsClean(wtPath) {
			return "", fmt.Errorf("workspace: worktree %s has uncommitted changes — refusing to reuse", wtPath)
		}
		if err := m.syncExistingWorktree(wtPath, branchName); err != nil {
			return "", err
		}
		m.refs[refKey(wtPath)]++
		log.Printf("[workspace] reused worktree %s (branch %s) for issue #%d (active=%d)",
			wtPath, branchName, issueNum, m.refs[refKey(wtPath)])
		return wtPath, nil
	}

	// 3. Path does not exist — try add.
	if err := os.MkdirAll(filepath.Dir(wtPath), 0755); err != nil {
		return "", fmt.Errorf("workspace: mkdir: %w", err)
	}

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
	m.refs[refKey(wtPath)]++
	return wtPath, nil
}

func (m *Manager) isRegisteredWorktree(wtPath string) bool {
	foundPath := m.findWorktreePathForPath(wtPath)
	return foundPath != ""
}

func (m *Manager) syncExistingWorktree(wtPath, branchName string) error {
	checkoutCmd := exec.Command("git", "checkout", branchName)
	checkoutCmd.Dir = wtPath
	if out, err := checkoutCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("workspace: checkout %s in %s: %s: %w", branchName, wtPath, strings.TrimSpace(string(out)), err)
	}

	if !m.remoteBranchExists(branchName) {
		return nil
	}

	pullCmd := exec.Command("git", "pull", "--ff-only", "origin", branchName)
	pullCmd.Dir = wtPath
	if out, err := pullCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("workspace: sync reused worktree %s on %s: %s: %w", wtPath, branchName, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// gitCurrentBranch returns the current branch in the given worktree directory.
func (m *Manager) gitCurrentBranch(wtPath string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitIsClean reports whether the given worktree directory has no uncommitted changes.
func (m *Manager) gitIsClean(wtPath string) bool {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == ""
}

// Remove releases one active reference to the worktree and, when the last
// reference is released, performs the actual git worktree removal and branch
// deletion.
//
// Sharing: the same per-issue worktree is reused across a dev-agent → review-agent
// handoff. Each Create/Remove pair is a reference, not necessarily a full
// lifecycle; the underlying worktree must survive until every active agent
// has released its reference. A Remove on a path whose refcount is > 1
// decrements the count and returns nil (no-op) to preserve the filesystem for
// the still-running sessions.
//
// A Remove on a path with no tracked refcount (e.g. recovery paths, Prune,
// callers from before this change) proceeds with the actual removal, matching
// prior behaviour.
//
// Best-effort on the actual removal: if worktree removal fails but prune
// succeeds, no error is returned. Returns a combined error only when cleanup
// truly fails (both remove+prune fail, or branch deletion fails).
func (m *Manager) Remove(wtPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := refKey(wtPath)
	if count, tracked := m.refs[key]; tracked {
		if count > 1 {
			m.refs[key] = count - 1
			log.Printf("[workspace] retaining worktree %s (still in use by %d session(s))",
				wtPath, count-1)
			return nil
		}
		delete(m.refs, key)
	}

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
	return m.findWorktreePathForPathAndBranch("", branchName)
}

func (m *Manager) findWorktreePathForPath(wtPath string) string {
	return m.findWorktreePathForPathAndBranch(wtPath, "")
}

func (m *Manager) findWorktreePathForPathAndBranch(wtPath, branchName string) string {
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = m.baseDir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	absTargetPath := ""
	if wtPath != "" {
		absTargetPath, _ = filepath.Abs(wtPath)
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
			branchMatches := branchName == "" || localBranch == branchName
			pathMatches := false
			if currentPath != "" {
				if absTargetPath == "" {
					pathMatches = true
				} else {
					absCurrentPath, _ := filepath.Abs(currentPath)
					pathMatches = absCurrentPath == absTargetPath
				}
			}
			if branchMatches && pathMatches && currentPath != "" {
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
		return nil, fmt.Errorf("git worktree list: %w", err)
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
