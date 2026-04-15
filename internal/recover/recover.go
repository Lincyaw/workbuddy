// Package recover implements the workbuddy recover workflow.
package recover

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite" // sqlite driver
)

const (
	defaultDBPath = ".workbuddy/workbuddy.db"
	worktreesDir  = ".workbuddy/worktrees"
)

var runtimeTables = []string{"events", "task_queue", "issue_cache", "issue_dependency_state"}

// Options controls a recover run.
type Options struct {
	RepoRoot            string
	CommonRoot          string
	DBPath              string
	CurrentPID          int
	ShellPID            int
	KillZombies         bool
	ResetDB             bool
	PruneWorktrees      bool
	PruneRemoteBranches bool
	Force               bool
	DryRun              bool
	Interactive         bool
	Stdin               io.Reader
	Stdout              io.Writer
	Stderr              io.Writer
}

// Process describes one candidate row from `ps`.
type Process struct {
	PID            int
	PPID           int
	ElapsedSeconds int
	Command        string
	Args           []string
}

// Run executes the requested recovery actions.
func Run(ctx context.Context, opts Options) error {
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	if opts.Stdin == nil {
		opts.Stdin = strings.NewReader("")
	}

	commonRoot, repoRoot, err := resolveRoots(ctx, opts)
	if err != nil {
		return err
	}
	dbPath := opts.DBPath
	if strings.TrimSpace(dbPath) == "" {
		dbPath = filepath.Join(commonRoot, defaultDBPath)
	} else if !filepath.IsAbs(dbPath) {
		dbPath = filepath.Join(commonRoot, dbPath)
	}

	if opts.KillZombies {
		if err := killZombies(ctx, commonRoot, opts); err != nil {
			return err
		}
	}
	if opts.PruneWorktrees {
		if err := pruneWorktrees(ctx, commonRoot, opts); err != nil {
			return err
		}
	}
	if opts.ResetDB {
		if err := resetDB(dbPath, opts); err != nil {
			return err
		}
	}
	if opts.PruneRemoteBranches {
		if err := pruneRemoteBranches(ctx, repoRoot, commonRoot, opts); err != nil {
			return err
		}
	}
	return nil
}

func resolveRoots(ctx context.Context, opts Options) (commonRoot string, repoRoot string, err error) {
	if opts.CommonRoot != "" {
		commonRoot = opts.CommonRoot
	} else {
		commonDir, cmdErr := gitOutput(ctx, opts.RepoRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if cmdErr != nil {
			return "", "", fmt.Errorf("recover: resolve git common dir: %w", cmdErr)
		}
		commonRoot = filepath.Dir(strings.TrimSpace(commonDir))
	}
	if opts.RepoRoot != "" {
		repoRoot = opts.RepoRoot
	} else {
		top, cmdErr := gitOutput(ctx, commonRoot, "rev-parse", "--path-format=absolute", "--show-toplevel")
		if cmdErr != nil {
			return "", "", fmt.Errorf("recover: resolve repo root: %w", cmdErr)
		}
		repoRoot = strings.TrimSpace(top)
	}
	return commonRoot, repoRoot, nil
}

func killZombies(ctx context.Context, commonRoot string, opts Options) error {
	processes, err := listProcesses(ctx)
	if err != nil {
		return fmt.Errorf("recover: list processes: %w", err)
	}
	currentPID := opts.CurrentPID
	if currentPID == 0 {
		currentPID = os.Getpid()
	}
	shellPID := opts.ShellPID
	if shellPID == 0 {
		shellPID = os.Getppid()
	}
	shellAge, hasShellAge := processElapsedSeconds(processes, shellPID)

	var victims []Process
	for _, proc := range processes {
		if !isRecoverableProcess(processes, proc, currentPID, shellPID, shellAge, hasShellAge) {
			continue
		}
		cwd, cwdErr := processCWD(proc.PID)
		if cwdErr != nil || !isWithinRoot(commonRoot, cwd) {
			continue
		}
		victims = append(victims, proc)
	}
	if len(victims) == 0 {
		fmt.Fprintln(opts.Stdout, "No matching zombie processes found.")
		return nil
	}

	for _, proc := range victims {
		fmt.Fprintf(opts.Stdout, "%s process %d: %s\n", actionText(opts.DryRun, "Would terminate", "Terminating"), proc.PID, proc.Command)
		if opts.DryRun {
			continue
		}
		if err := terminateProcess(proc.PID); err != nil {
			return fmt.Errorf("recover: terminate pid %d: %w", proc.PID, err)
		}
	}
	return nil
}

func processElapsedSeconds(processes []Process, pid int) (int, bool) {
	for _, proc := range processes {
		if proc.PID == pid {
			return proc.ElapsedSeconds, true
		}
	}
	return 0, false
}

func isRecoverableProcess(processes []Process, proc Process, currentPID int, shellPID int, shellAge int, hasShellAge bool) bool {
	if proc.PID == currentPID || !proc.matchesTarget() {
		return false
	}
	if proc.PPID == 1 {
		return true
	}
	if shellPID > 0 && !isDescendantOf(processes, proc.PID, shellPID) {
		return true
	}
	return hasShellAge && proc.ElapsedSeconds > shellAge
}

func isDescendantOf(processes []Process, pid int, ancestorPID int) bool {
	if pid <= 0 || ancestorPID <= 0 {
		return false
	}
	current := pid
	seen := map[int]struct{}{}
	for current > 0 {
		if current == ancestorPID {
			return true
		}
		if _, ok := seen[current]; ok {
			return false
		}
		seen[current] = struct{}{}

		parent, ok := processParentPID(processes, current)
		if !ok || parent <= 0 || parent == current {
			return false
		}
		current = parent
	}
	return false
}

func processParentPID(processes []Process, pid int) (int, bool) {
	for _, proc := range processes {
		if proc.PID == pid {
			return proc.PPID, true
		}
	}
	return 0, false
}

func pruneWorktrees(ctx context.Context, commonRoot string, opts Options) error {
	wtRoot := filepath.Join(commonRoot, worktreesDir)
	entries, err := os.ReadDir(wtRoot)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("recover: read worktrees dir: %w", err)
	}
	for _, entry := range entries {
		target := filepath.Join(wtRoot, entry.Name())
		fmt.Fprintf(opts.Stdout, "%s worktree path %s\n", actionText(opts.DryRun, "Would remove", "Removing"), target)
		if opts.DryRun {
			continue
		}
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("recover: remove worktree %s: %w", target, err)
		}
	}
	fmt.Fprintf(opts.Stdout, "%s git worktree prune\n", actionText(opts.DryRun, "Would run", "Running"))
	if opts.DryRun {
		return nil
	}
	if _, err := gitOutput(ctx, commonRoot, "worktree", "prune"); err != nil {
		return fmt.Errorf("recover: git worktree prune: %w", err)
	}
	return nil
}

func resetDB(dbPath string, opts Options) error {
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(opts.Stdout, "Database not found, skipping reset: %s\n", dbPath)
			return nil
		}
		return fmt.Errorf("recover: stat db: %w", err)
	}

	for _, table := range runtimeTables {
		fmt.Fprintf(opts.Stdout, "%s sqlite table %s\n", actionText(opts.DryRun, "Would clear", "Clearing"), table)
	}
	if opts.DryRun {
		return nil
	}

	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("recover: open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("recover: begin reset tx: %w", err)
	}
	for _, table := range runtimeTables {
		if _, err := tx.Exec("DELETE FROM " + table); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("recover: clear %s: %w", table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("recover: commit reset tx: %w", err)
	}
	return nil
}

func pruneRemoteBranches(ctx context.Context, repoRoot, commonRoot string, opts Options) error {
	branches, err := listRemoteIssueBranches(ctx, commonRoot)
	if err != nil {
		return fmt.Errorf("recover: list remote branches: %w", err)
	}
	if len(branches) == 0 {
		fmt.Fprintln(opts.Stdout, "No remote workbuddy issue branches found.")
		return nil
	}

	repoSlug, err := gitHubRepoSlug(ctx, repoRoot)
	if err != nil {
		return err
	}
	openPRBranches, err := listOpenPRBranches(ctx, repoRoot, repoSlug)
	if err != nil {
		return err
	}

	var orphans []string
	for _, branch := range branches {
		if _, ok := openPRBranches[branch]; !ok {
			orphans = append(orphans, branch)
		}
	}
	if len(orphans) == 0 {
		fmt.Fprintln(opts.Stdout, "No orphaned remote branches found.")
		return nil
	}

	if opts.DryRun {
		for _, branch := range orphans {
			fmt.Fprintf(opts.Stdout, "%s remote branch %s\n", actionText(true, "Would delete", "Deleting"), branch)
		}
		return nil
	}
	if !opts.Force && !opts.Interactive {
		fmt.Fprintln(opts.Stdout, "Skipping remote branch deletion in non-interactive mode. Re-run with --force to delete:")
		for _, branch := range orphans {
			fmt.Fprintln(opts.Stdout, branch)
		}
		return nil
	}
	if !opts.Force {
		ok, err := confirmDeletion(opts.Stdin, opts.Stdout, orphans)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(opts.Stdout, "Remote branch deletion canceled.")
			return nil
		}
	}

	for _, branch := range orphans {
		fmt.Fprintf(opts.Stdout, "%s remote branch %s\n", actionText(false, "Would delete", "Deleting"), branch)
		if _, err := gitOutput(ctx, commonRoot, "push", "origin", "--delete", branch); err != nil {
			return fmt.Errorf("recover: delete remote branch %s: %w", branch, err)
		}
	}
	return nil
}

func confirmDeletion(stdin io.Reader, stdout io.Writer, branches []string) (bool, error) {
	fmt.Fprintln(stdout, "Delete orphaned remote branches?")
	for _, branch := range branches {
		fmt.Fprintf(stdout, " - %s\n", branch)
	}
	fmt.Fprint(stdout, "Type 'yes' to continue: ")
	reader := bufio.NewReader(stdin)
	answer, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("recover: read confirmation: %w", err)
	}
	return strings.EqualFold(strings.TrimSpace(answer), "yes"), nil
}

func listRemoteIssueBranches(ctx context.Context, dir string) ([]string, error) {
	out, err := gitOutput(ctx, dir, "for-each-ref", "--format=%(refname:lstrip=3)", "refs/remotes/origin/workbuddy/issue-*")
	if err != nil {
		return nil, err
	}
	var branches []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			branches = append(branches, line)
		}
	}
	return branches, nil
}

func listOpenPRBranches(ctx context.Context, dir, repoSlug string) (map[string]struct{}, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "list", "--repo", repoSlug, "--state", "open", "--limit", "200", "--json", "headRefName")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("recover: gh pr list: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var rows []struct {
		HeadRefName string `json:"headRefName"`
	}
	if err := jsonUnmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("recover: parse gh pr list: %w", err)
	}
	branches := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if row.HeadRefName != "" {
			branches[row.HeadRefName] = struct{}{}
		}
	}
	return branches, nil
}

func gitHubRepoSlug(ctx context.Context, dir string) (string, error) {
	remote, err := gitOutput(ctx, dir, "config", "--get", "remote.origin.url")
	if err != nil {
		return "", fmt.Errorf("recover: read origin remote: %w", err)
	}
	slug := parseGitHubRepoSlug(strings.TrimSpace(remote))
	if slug == "" {
		return "", fmt.Errorf("recover: unsupported origin remote %q", strings.TrimSpace(remote))
	}
	return slug, nil
}

func parseGitHubRepoSlug(remote string) string {
	remote = strings.TrimSpace(remote)
	remote = strings.TrimSuffix(remote, ".git")
	if strings.HasPrefix(remote, "git@github.com:") {
		return strings.TrimPrefix(remote, "git@github.com:")
	}
	if strings.Contains(remote, "github.com/") {
		parts := strings.SplitN(remote, "github.com/", 2)
		return strings.TrimPrefix(parts[1], "/")
	}
	return ""
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func listProcesses(ctx context.Context) ([]Process, error) {
	cmd := exec.CommandContext(ctx, "ps", "-eo", "pid=,ppid=,etimes=,args=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ps: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return ParseProcessList(string(out))
}

// ParseProcessList parses `ps -eo pid=,ppid=,etimes=,args=` output.
func ParseProcessList(raw string) ([]Process, error) {
	var processes []Process
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			return nil, fmt.Errorf("recover: malformed ps line %q", line)
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, fmt.Errorf("recover: parse pid from %q: %w", line, err)
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("recover: parse ppid from %q: %w", line, err)
		}
		elapsed, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("recover: parse etimes from %q: %w", line, err)
		}
		args := fields[3:]
		processes = append(processes, Process{
			PID:            pid,
			PPID:           ppid,
			ElapsedSeconds: elapsed,
			Command:        strings.Join(args, " "),
			Args:           args,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("recover: scan ps output: %w", err)
	}
	return processes, nil
}

func (p Process) matchesTarget() bool {
	if len(p.Args) == 0 {
		return false
	}
	base := filepath.Base(p.Args[0])
	switch base {
	case "codex":
		return true
	case "workbuddy":
		return len(p.Args) > 1 && p.Args[1] == "serve"
	default:
		return false
	}
}

func processCWD(pid int) (string, error) {
	if runtime.GOOS != "linux" {
		return "", errors.New("cwd lookup unsupported on this platform")
	}
	return os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
}

func isWithinRoot(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func terminateProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processExists(pid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processExists(pid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("process %d still running after SIGKILL", pid)
}

func processExists(pid int) bool {
	if runtime.GOOS == "linux" {
		if status, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status")); err == nil {
			for _, line := range strings.Split(string(status), "\n") {
				if strings.HasPrefix(line, "State:") && strings.Contains(line, "\tZ") {
					return false
				}
			}
		}
	}
	err := syscall.Kill(pid, 0)
	return err == nil
}

func actionText(dryRun bool, preview string, live string) string {
	if dryRun {
		return preview
	}
	return live
}

var jsonUnmarshal = func(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
