package cmd

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	defaultWatchInterval        = 5 * time.Minute
	defaultUpdaterBinaryPath    = "/usr/local/bin/workbuddy"
	defaultUpdaterSystemctlMode = "user"
	updaterCurrentVersionFile   = "current-version"
	updaterPreviousBinaryFile   = "previous-binary"
	updaterChecksumName         = "checksums.txt"
)

// deployWatchSleep blocks for d or until ctx is cancelled. Overridable in tests.
var deployWatchSleep = func(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type deployWatchOpts struct {
	repository     string
	interval       time.Duration
	restartUnits   []string
	stateDir       string
	binaryPath     string
	systemctlScope string
	dryRun         bool
	once           bool
}

type deployRollbackOpts struct {
	stateDir       string
	restartUnits   []string
	binaryPath     string
	systemctlScope string
	dryRun         bool
}

var deployWatchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Poll GitHub Releases and upgrade the deployed binary in place",
	Long: strings.TrimSpace(`
Poll the configured GitHub repository for the latest release on a fixed
interval. When a newer release is found, download its linux-${ARCH} archive,
verify its SHA256 against the release checksums file, atomically replace the
target binary (default /usr/local/bin/workbuddy), back up the previous
binary, and restart the listed systemd units.

The previous binary is preserved in <state-dir>/previous-binary so that
"workbuddy deploy rollback" can restore it. The current installed version
is tracked in <state-dir>/current-version. On any failure the existing
binary is left untouched and the error is logged; the loop retries on the
next interval.
`),
	Example: strings.TrimSpace(`
  # One-shot dry run:
  workbuddy deploy watch --repo Lincyaw/workbuddy --once --dry-run

  # Recurring updater (typical systemd user service ExecStart):
  workbuddy deploy watch \
    --repo Lincyaw/workbuddy \
    --interval 5m \
    --restart-units workbuddy-coordinator.service \
    --restart-units workbuddy-worker.service
`),
	RunE: runDeployWatchCmd,
}

var deployRollbackCmd = &cobra.Command{
	Use:   "rollback",
	Short: "Swap the previous binary back into place and restart units",
	Long: strings.TrimSpace(`
Swap <state-dir>/previous-binary back into the target binary path (default
/usr/local/bin/workbuddy), then restart the listed systemd units. The
displaced binary is moved into <state-dir>/previous-binary so that running
rollback twice toggles between the two versions.
`),
	RunE: runDeployRollbackCmd,
}

func init() {
	deployWatchCmd.Flags().String("repo", "", "GitHub repository in OWNER/NAME form (required)")
	deployWatchCmd.Flags().Duration("interval", defaultWatchInterval, "Poll interval between release checks")
	deployWatchCmd.Flags().StringSlice("restart-units", nil, "systemd unit name(s) to restart after a successful upgrade (repeatable)")
	deployWatchCmd.Flags().String("state-dir", "", "Directory used to track the current version and previous binary backup (default: $XDG_STATE_HOME/workbuddy/updater or ~/.local/state/workbuddy/updater)")
	deployWatchCmd.Flags().String("binary", defaultUpdaterBinaryPath, "Path to the workbuddy binary to upgrade")
	deployWatchCmd.Flags().String("systemctl-scope", defaultUpdaterSystemctlMode, "systemctl scope used to restart units: user or system")
	deployWatchCmd.Flags().Bool("dry-run", false, "Run a check but do not download, replace, or restart anything")
	deployWatchCmd.Flags().Bool("once", false, "Run a single check and exit instead of looping")

	deployRollbackCmd.Flags().String("state-dir", "", "Directory holding the previous-binary backup (default: $XDG_STATE_HOME/workbuddy/updater or ~/.local/state/workbuddy/updater)")
	deployRollbackCmd.Flags().StringSlice("restart-units", nil, "systemd unit name(s) to restart after rollback (repeatable)")
	deployRollbackCmd.Flags().String("binary", defaultUpdaterBinaryPath, "Path to the workbuddy binary to roll back")
	deployRollbackCmd.Flags().String("systemctl-scope", defaultUpdaterSystemctlMode, "systemctl scope used to restart units: user or system")
	deployRollbackCmd.Flags().Bool("dry-run", false, "Print the actions that would be taken without executing them")

	deployCmd.AddCommand(deployWatchCmd, deployRollbackCmd)
}

func runDeployWatchCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseDeployWatchFlags(cmd)
	if err != nil {
		return err
	}
	if !opts.dryRun {
		if err := requireWritable(cmd, "deploy watch"); err != nil {
			return err
		}
	}
	return runDeployWatchLoop(cmd.Context(), opts, cmdStdout(cmd))
}

func runDeployRollbackCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseDeployRollbackFlags(cmd)
	if err != nil {
		return err
	}
	if !opts.dryRun {
		if err := requireWritable(cmd, "deploy rollback"); err != nil {
			return err
		}
	}
	return runDeployRollback(cmd.Context(), opts, cmdStdout(cmd))
}

func parseDeployWatchFlags(cmd *cobra.Command) (*deployWatchOpts, error) {
	repo, _ := cmd.Flags().GetString("repo")
	interval, _ := cmd.Flags().GetDuration("interval")
	units, _ := cmd.Flags().GetStringSlice("restart-units")
	stateDir, _ := cmd.Flags().GetString("state-dir")
	binary, _ := cmd.Flags().GetString("binary")
	scope, _ := cmd.Flags().GetString("systemctl-scope")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	once, _ := cmd.Flags().GetBool("once")

	repo = strings.TrimSpace(repo)
	if repo == "" {
		return nil, fmt.Errorf("deploy watch: --repo is required (owner/name)")
	}
	if !strings.Contains(repo, "/") {
		return nil, fmt.Errorf("deploy watch: --repo must be owner/name, got %q", repo)
	}
	if interval <= 0 {
		return nil, fmt.Errorf("deploy watch: --interval must be positive")
	}
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope != "user" && scope != "system" {
		return nil, fmt.Errorf("deploy watch: --systemctl-scope must be user or system")
	}
	binary = strings.TrimSpace(binary)
	if binary == "" {
		return nil, fmt.Errorf("deploy watch: --binary is required")
	}
	resolvedStateDir, err := resolveUpdaterStateDir(stateDir)
	if err != nil {
		return nil, fmt.Errorf("deploy watch: %w", err)
	}
	return &deployWatchOpts{
		repository:     repo,
		interval:       interval,
		restartUnits:   trimStringSlice(units),
		stateDir:       resolvedStateDir,
		binaryPath:     binary,
		systemctlScope: scope,
		dryRun:         dryRun,
		once:           once,
	}, nil
}

func parseDeployRollbackFlags(cmd *cobra.Command) (*deployRollbackOpts, error) {
	stateDir, _ := cmd.Flags().GetString("state-dir")
	units, _ := cmd.Flags().GetStringSlice("restart-units")
	binary, _ := cmd.Flags().GetString("binary")
	scope, _ := cmd.Flags().GetString("systemctl-scope")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope != "user" && scope != "system" {
		return nil, fmt.Errorf("deploy rollback: --systemctl-scope must be user or system")
	}
	binary = strings.TrimSpace(binary)
	if binary == "" {
		return nil, fmt.Errorf("deploy rollback: --binary is required")
	}
	resolvedStateDir, err := resolveUpdaterStateDir(stateDir)
	if err != nil {
		return nil, fmt.Errorf("deploy rollback: %w", err)
	}
	return &deployRollbackOpts{
		stateDir:       resolvedStateDir,
		restartUnits:   trimStringSlice(units),
		binaryPath:     binary,
		systemctlScope: scope,
		dryRun:         dryRun,
	}, nil
}

func resolveUpdaterStateDir(requested string) (string, error) {
	if dir := strings.TrimSpace(requested); dir != "" {
		return filepath.Abs(dir)
	}
	if v := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); v != "" {
		return filepath.Join(v, "workbuddy", "updater"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".local", "state", "workbuddy", "updater"), nil
}

func runDeployWatchLoop(ctx context.Context, opts *deployWatchOpts, stdout io.Writer) error {
	if opts == nil {
		return fmt.Errorf("deploy watch: options are required")
	}
	for {
		if err := runDeployWatchOnce(ctx, opts, stdout); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			fmt.Fprintf(stdout, "watch: error: %v\n", err)
		}
		if opts.once {
			return nil
		}
		if err := deployWatchSleep(ctx, opts.interval); err != nil {
			return err
		}
	}
}

func runDeployWatchOnce(ctx context.Context, opts *deployWatchOpts, stdout io.Writer) error {
	current, err := readCurrentVersion(opts.stateDir)
	if err != nil {
		return fmt.Errorf("read current version: %w", err)
	}
	latest, err := resolveReleaseVersion(ctx, opts.repository, "latest")
	if err != nil {
		return fmt.Errorf("resolve latest release: %w", err)
	}
	currentLabel := current
	if currentLabel == "" {
		currentLabel = "<none>"
	}
	if !semverGreater(latest, current) {
		fmt.Fprintf(stdout, "watch: up-to-date (current=%s, latest=v%s)\n", currentLabel, latest)
		return nil
	}
	fmt.Fprintf(stdout, "watch: upgrade available (current=%s, latest=v%s)\n", currentLabel, latest)
	if opts.dryRun {
		fmt.Fprintf(stdout, "watch: dry-run, skipping download and restart\n")
		return nil
	}
	if err := performUpgrade(ctx, opts, latest, stdout); err != nil {
		return err
	}
	return nil
}

func performUpgrade(ctx context.Context, opts *deployWatchOpts, version string, stdout io.Writer) error {
	if err := os.MkdirAll(opts.stateDir, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	archiveName := releaseArchiveName(version)
	archiveBytes, err := downloadReleaseAsset(ctx, opts.repository, version, archiveName)
	if err != nil {
		return fmt.Errorf("download archive: %w", err)
	}
	checksumBytes, err := downloadReleaseAsset(ctx, opts.repository, version, updaterChecksumName)
	if err != nil {
		return fmt.Errorf("download checksums: %w", err)
	}
	expected, err := lookupChecksum(checksumBytes, archiveName)
	if err != nil {
		return fmt.Errorf("locate checksum: %w", err)
	}
	got := sha256Hex(archiveBytes)
	if got != expected {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", archiveName, expected, got)
	}
	binaryBytes, err := extractBinaryFromArchive(bytes.NewReader(archiveBytes))
	if err != nil {
		return fmt.Errorf("extract binary: %w", err)
	}

	previousBinary := filepath.Join(opts.stateDir, updaterPreviousBinaryFile)
	if err := backupBinary(opts.binaryPath, previousBinary); err != nil {
		return fmt.Errorf("backup previous binary: %w", err)
	}
	if err := writeBytesAtomic(opts.binaryPath, binaryBytes, 0o755); err != nil {
		return fmt.Errorf("install new binary: %w", err)
	}
	if err := writeCurrentVersion(opts.stateDir, "v"+version); err != nil {
		return fmt.Errorf("write current-version: %w", err)
	}
	fmt.Fprintf(stdout, "watch: installed v%s to %s (previous backed up to %s)\n", version, opts.binaryPath, previousBinary)
	if err := restartUnits(ctx, opts.systemctlScope, opts.restartUnits); err != nil {
		return fmt.Errorf("restart units: %w", err)
	}
	for _, unit := range opts.restartUnits {
		fmt.Fprintf(stdout, "watch: restarted %s\n", unit)
	}
	return nil
}

func runDeployRollback(ctx context.Context, opts *deployRollbackOpts, stdout io.Writer) error {
	if opts == nil {
		return fmt.Errorf("deploy rollback: options are required")
	}
	previousBinary := filepath.Join(opts.stateDir, updaterPreviousBinaryFile)
	info, err := os.Stat(previousBinary)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("deploy rollback: no previous binary at %s", previousBinary)
		}
		return fmt.Errorf("deploy rollback: stat previous binary: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("deploy rollback: previous binary path %s is a directory", previousBinary)
	}
	if opts.dryRun {
		fmt.Fprintf(stdout, "dry-run: would swap %s into %s\n", previousBinary, opts.binaryPath)
		for _, unit := range opts.restartUnits {
			fmt.Fprintf(stdout, "dry-run: would restart %s (scope=%s)\n", unit, opts.systemctlScope)
		}
		return nil
	}

	prevBytes, err := os.ReadFile(previousBinary)
	if err != nil {
		return fmt.Errorf("deploy rollback: read previous binary: %w", err)
	}
	tmpPrev := previousBinary + ".rollback-tmp"
	defer os.Remove(tmpPrev)
	if currentBytes, err := os.ReadFile(opts.binaryPath); err == nil {
		if err := writeBytesAtomic(tmpPrev, currentBytes, 0o755); err != nil {
			return fmt.Errorf("deploy rollback: stash current binary: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("deploy rollback: read current binary: %w", err)
	}
	if err := writeBytesAtomic(opts.binaryPath, prevBytes, 0o755); err != nil {
		return fmt.Errorf("deploy rollback: install previous binary: %w", err)
	}
	// Move tmpPrev → previousBinary so rollback toggles between versions.
	if _, err := os.Stat(tmpPrev); err == nil {
		if err := os.Rename(tmpPrev, previousBinary); err != nil {
			return fmt.Errorf("deploy rollback: rotate previous-binary: %w", err)
		}
	} else {
		_ = os.Remove(previousBinary)
	}
	fmt.Fprintf(stdout, "rollback: swapped %s into %s\n", previousBinary, opts.binaryPath)
	if err := restartUnits(ctx, opts.systemctlScope, opts.restartUnits); err != nil {
		return fmt.Errorf("deploy rollback: %w", err)
	}
	for _, unit := range opts.restartUnits {
		fmt.Fprintf(stdout, "rollback: restarted %s\n", unit)
	}
	return nil
}

func backupBinary(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			// First-time install: nothing to back up.
			_ = os.Remove(dst)
			return nil
		}
		return err
	}
	return writeBytesAtomic(dst, data, 0o755)
}

func readCurrentVersion(stateDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, updaterCurrentVersionFile))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func writeCurrentVersion(stateDir, version string) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	return writeBytesAtomic(filepath.Join(stateDir, updaterCurrentVersionFile), []byte(version+"\n"), 0o644)
}

func downloadReleaseAsset(ctx context.Context, repository, version, name string) ([]byte, error) {
	url := fmt.Sprintf("%s/%s/releases/download/v%s/%s",
		strings.TrimRight(deployGitHubDownloadBase, "/"), repository, version, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", name, err)
	}
	applyGitHubAuth(req)
	resp, err := deployHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: unexpected status %s", name, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s body: %w", name, err)
	}
	return body, nil
}

func lookupChecksum(checksumFile []byte, target string) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(checksumFile))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// goreleaser format: "<sha>  <name>" — the name may have a leading "*".
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		if name == target {
			return strings.ToLower(fields[0]), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("no checksum entry for %s", target)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func extractBinaryFromArchive(reader io.Reader) ([]byte, error) {
	gzipReader, err := gzip.NewReader(reader)
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		if header == nil || header.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(header.Name) != "workbuddy" {
			continue
		}
		return io.ReadAll(tarReader)
	}
	return nil, fmt.Errorf("archive did not contain a workbuddy binary")
}

func restartUnits(ctx context.Context, scope string, units []string) error {
	for _, unit := range units {
		if strings.TrimSpace(unit) == "" {
			continue
		}
		if err := deployRunSystemctl(ctx, scope, "restart", unit); err != nil {
			return err
		}
	}
	return nil
}

// semverGreater reports whether `latest` (e.g. "1.2.3") is strictly greater
// than `current` (e.g. "v1.2.2", "1.2.2", or "" for unset). Non-numeric
// segments are compared lexically so prerelease tags like "1.2.3-rc1" still
// produce a stable ordering.
func semverGreater(latest, current string) bool {
	if strings.TrimSpace(current) == "" {
		return true
	}
	return compareSemver(latest, current) > 0
}

func compareSemver(a, b string) int {
	aCore, aPre := splitSemver(a)
	bCore, bPre := splitSemver(b)
	max := len(aCore)
	if len(bCore) > max {
		max = len(bCore)
	}
	for i := 0; i < max; i++ {
		ax := "0"
		bx := "0"
		if i < len(aCore) {
			ax = aCore[i]
		}
		if i < len(bCore) {
			bx = bCore[i]
		}
		ai, aErr := strconv.Atoi(ax)
		bi, bErr := strconv.Atoi(bx)
		if aErr == nil && bErr == nil {
			if ai != bi {
				if ai > bi {
					return 1
				}
				return -1
			}
			continue
		}
		if ax != bx {
			if ax > bx {
				return 1
			}
			return -1
		}
	}
	// Per semver: a release version > the same version with a prerelease tag.
	if aPre == "" && bPre != "" {
		return 1
	}
	if aPre != "" && bPre == "" {
		return -1
	}
	if aPre == bPre {
		return 0
	}
	if aPre > bPre {
		return 1
	}
	return -1
}

func splitSemver(v string) (core []string, prerelease string) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return nil, ""
	}
	body := v
	if idx := strings.Index(body, "+"); idx >= 0 {
		body = body[:idx] // drop build metadata
	}
	if idx := strings.Index(body, "-"); idx >= 0 {
		prerelease = body[idx+1:]
		body = body[:idx]
	}
	core = strings.Split(body, ".")
	return core, prerelease
}

