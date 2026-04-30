package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/supervisor"
	"github.com/spf13/cobra"
)

// Bundle install creates the three v0.5 systemd user units (supervisor +
// coordinator + worker) used by `workbuddy deploy install --bundle` and
// cleaned up by `workbuddy deploy uninstall`. The supervisor unit uses
// Type=notify + KillMode=process so it can be restarted without killing the
// agent subprocesses it owns.

const (
	bundleSupervisorName  = "workbuddy-supervisor"
	bundleCoordinatorName = "workbuddy-coordinator"
	bundleWorkerName      = "workbuddy-worker"
)

type bundleInstallOpts struct {
	scope            string
	binaryPath       string
	workingDirectory string
	envFiles         []string
	enable           bool
	start            bool
	supervisorArgs   []string
	coordinatorArgs  []string
	workerArgs       []string
	skipCoordinator  bool
	skipWorker       bool
}

type bundleUninstallOpts struct {
	scope    string
	force    bool
	dryRun   bool
	stateDir string
	socket   string
	stdin    io.Reader
}

var deployUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the bundled supervisor+coordinator+worker deployment created by `deploy install --bundle`",
	Long: `Remove the bundled v0.5 deployment created by ` + "`deploy install --bundle`" + `.

This stops, disables and deletes the three systemd user units
(workbuddy-supervisor, workbuddy-coordinator, workbuddy-worker) along with
their deployment manifests, and (unless --keep-state is set) wipes the
supervisor unix socket and on-disk state directory.

The on-disk binary is left in place — use ` + "`deploy delete`" + ` per unit if
you want to remove individual deployments by name.`,
	RunE: runDeployUninstallCmd,
}

func init() {
	// --bundle and the auxiliary tuning flags hang off `deploy install`.
	deployInstallCmd.Flags().Bool("bundle", false, "Install the v0.5 supervisor+coordinator+worker user units in one shot. Ignores --name and trailing -- args; use --supervisor-args / --coordinator-args / --worker-args (repeatable) to extend each unit's ExecStart.")
	deployInstallCmd.Flags().StringArray("supervisor-args", nil, "Extra argument to append to the supervisor ExecStart (repeatable, --bundle only)")
	deployInstallCmd.Flags().StringArray("coordinator-args", nil, "Extra argument to append to the coordinator ExecStart (repeatable, --bundle only)")
	deployInstallCmd.Flags().StringArray("worker-args", nil, "Extra argument to append to the worker ExecStart (repeatable, --bundle only)")
	deployInstallCmd.Flags().Bool("bundle-skip-coordinator", false, "When --bundle is set, do not (re)install the coordinator unit (e.g. host runs worker only)")
	deployInstallCmd.Flags().Bool("bundle-skip-worker", false, "When --bundle is set, do not (re)install the worker unit")

	deployUninstallCmd.Flags().String("scope", defaultDeployScope, "Deployment scope: user or system")
	deployUninstallCmd.Flags().Bool("force", false, "Skip confirmation prompts for destructive actions")
	deployUninstallCmd.Flags().Bool("dry-run", false, "Print the actions that would be taken without executing them")
	deployUninstallCmd.Flags().Bool("keep-state", false, "Do not delete the supervisor on-disk state directory and unix socket")
	deployUninstallCmd.Flags().String("state-dir", "", "Override the supervisor state directory removed by uninstall (default: $XDG_STATE_HOME/workbuddy)")
	deployUninstallCmd.Flags().String("socket", "", "Override the supervisor unix socket path removed by uninstall (default: $XDG_RUNTIME_DIR/workbuddy-supervisor.sock)")

	deployCmd.AddCommand(deployUninstallCmd)
}

func parseBundleInstallFlags(cmd *cobra.Command) (*bundleInstallOpts, error) {
	scope, _ := cmd.Flags().GetString("scope")
	binaryPath, _ := cmd.Flags().GetString("binary")
	workingDir, _ := cmd.Flags().GetString("working-directory")
	envFiles, _ := cmd.Flags().GetStringArray("env-file")
	enable, _ := cmd.Flags().GetBool("enable")
	start, _ := cmd.Flags().GetBool("start")
	supervisorArgs, _ := cmd.Flags().GetStringArray("supervisor-args")
	coordinatorArgs, _ := cmd.Flags().GetStringArray("coordinator-args")
	workerArgs, _ := cmd.Flags().GetStringArray("worker-args")
	skipCoordinator, _ := cmd.Flags().GetBool("bundle-skip-coordinator")
	skipWorker, _ := cmd.Flags().GetBool("bundle-skip-worker")

	envVars, _ := cmd.Flags().GetStringArray("env")
	if len(envVars) > 0 {
		return nil, fmt.Errorf("deploy install: --env is not supported with --bundle (use --env-file)")
	}

	return &bundleInstallOpts{
		scope:            strings.TrimSpace(scope),
		binaryPath:       strings.TrimSpace(binaryPath),
		workingDirectory: strings.TrimSpace(workingDir),
		envFiles:         trimStringSlice(envFiles),
		enable:           enable,
		start:            start,
		supervisorArgs:   trimStringSlice(supervisorArgs),
		coordinatorArgs:  trimStringSlice(coordinatorArgs),
		workerArgs:       trimStringSlice(workerArgs),
		skipCoordinator:  skipCoordinator,
		skipWorker:       skipWorker,
	}, nil
}

func runDeployBundleInstallWithOpts(ctx context.Context, opts *bundleInstallOpts, stdout io.Writer) error {
	if opts == nil {
		return fmt.Errorf("deploy install --bundle: options are required")
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("deploy install --bundle: systemd deployment is only supported on Linux")
	}

	specs := []bundleUnitSpec{
		{
			name:     bundleSupervisorName,
			args:     append([]string{"supervisor"}, opts.supervisorArgs...),
			unitType: "notify",
			killMode: "process",
			restart:  "always",
		},
	}
	if !opts.skipCoordinator {
		specs = append(specs, bundleUnitSpec{
			name:     bundleCoordinatorName,
			args:     append([]string{"coordinator"}, opts.coordinatorArgs...),
			unitType: "simple",
			restart:  "on-failure",
			after:    []string{bundleSupervisorName + ".service"},
		})
	}
	if !opts.skipWorker {
		specs = append(specs, bundleUnitSpec{
			name:     bundleWorkerName,
			args:     append([]string{"worker"}, opts.workerArgs...),
			unitType: "simple",
			restart:  "on-failure",
			after:    []string{bundleSupervisorName + ".service"},
		})
	}

	for _, spec := range specs {
		install := &deployInstallOpts{
			name:             spec.name,
			scope:            opts.scope,
			binaryPath:       opts.binaryPath,
			workingDirectory: opts.workingDirectory,
			systemd:          true,
			envFiles:         append([]string(nil), opts.envFiles...),
			enable:           opts.enable,
			start:            spec.shouldStart(opts.start),
			commandArgs:      append([]string(nil), spec.args...),
		}
		if err := runDeployInstallWithOpts(ctx, install, stdout); err != nil {
			return err
		}
		// runDeployInstallWithOpts produces the manifest with default
		// Type=simple/Restart=on-failure. Patch the systemd block to
		// honor the bundle spec, re-render the unit, and rewrite.
		if err := applyBundleUnitSpec(ctx, opts.scope, spec, stdout); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(stdout, "bundle install complete (%d units)\n", len(specs)); err != nil {
		return fmt.Errorf("deploy install --bundle: write output: %w", err)
	}
	return nil
}

type bundleUnitSpec struct {
	name     string
	args     []string
	unitType string
	killMode string
	restart  string
	after    []string
}

// shouldStart suppresses Started=true for coordinator/worker if the user
// passed --start=false; the supervisor and dependent units always honor the
// flag.
func (s bundleUnitSpec) shouldStart(start bool) bool {
	return start
}

func applyBundleUnitSpec(ctx context.Context, scope string, spec bundleUnitSpec, stdout io.Writer) error {
	record, err := loadDeploymentRecordForScope(spec.name, scope)
	if err != nil {
		return fmt.Errorf("deploy install --bundle: %w", err)
	}
	if record.manifest == nil || record.manifest.Systemd == nil {
		return fmt.Errorf("deploy install --bundle: %s manifest missing systemd block", spec.name)
	}
	record.manifest.Systemd.Type = spec.unitType
	record.manifest.Systemd.KillMode = spec.killMode
	record.manifest.Systemd.Restart = spec.restart
	record.manifest.Systemd.After = append([]string(nil), spec.after...)
	unit, err := renderSystemdUnit(record.manifest, record.scopePaths.wantedBy)
	if err != nil {
		return fmt.Errorf("deploy install --bundle: render unit %s: %w", spec.name, err)
	}
	if err := writeTextFileAtomic(record.manifest.Systemd.UnitPath, unit, deploymentUnitFileMode(record.manifest)); err != nil {
		return fmt.Errorf("deploy install --bundle: write unit %s: %w", spec.name, err)
	}
	if err := writeDeploymentManifest(record.manifestPath, record.manifest); err != nil {
		return fmt.Errorf("deploy install --bundle: write manifest %s: %w", spec.name, err)
	}
	if err := deployRunSystemctl(ctx, record.manifest.Scope, "daemon-reload"); err != nil {
		return fmt.Errorf("deploy install --bundle: %w", err)
	}
	if record.manifest.Systemd.Started {
		if err := deployRunSystemctl(ctx, record.manifest.Scope, "restart", spec.name+".service"); err != nil {
			return fmt.Errorf("deploy install --bundle: %w", err)
		}
		_, _ = fmt.Fprintf(stdout, "restarted %s.service with bundle settings (Type=%s)\n", spec.name, spec.unitType)
	}
	return nil
}

func runDeployUninstallCmd(cmd *cobra.Command, _ []string) error {
	if err := requireWritable(cmd, "deploy uninstall"); err != nil {
		return err
	}
	scope, _ := cmd.Flags().GetString("scope")
	force, _ := cmd.Flags().GetBool("force")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	keepState, _ := cmd.Flags().GetBool("keep-state")
	stateDir, _ := cmd.Flags().GetString("state-dir")
	socket, _ := cmd.Flags().GetString("socket")
	if keepState {
		stateDir = ""
		socket = ""
	} else {
		if stateDir == "" {
			if dir, err := supervisor.DefaultStateDir(); err == nil {
				stateDir = dir
			}
		}
		if socket == "" {
			socket = supervisor.DefaultSocketPath()
		}
	}
	opts := &bundleUninstallOpts{
		scope:    strings.TrimSpace(scope),
		force:    force,
		dryRun:   dryRun,
		stateDir: stateDir,
		socket:   socket,
		stdin:    cmd.InOrStdin(),
	}
	return runDeployBundleUninstallWithOpts(cmd.Context(), opts, cmdStdout(cmd))
}

func runDeployBundleUninstallWithOpts(ctx context.Context, opts *bundleUninstallOpts, stdout io.Writer) error {
	if opts == nil {
		return fmt.Errorf("deploy uninstall: options are required")
	}
	scope := opts.scope
	if scope == "" {
		scope = defaultDeployScope
	}
	names := []string{bundleWorkerName, bundleCoordinatorName, bundleSupervisorName}
	for _, name := range names {
		record, err := loadDeploymentRecordForScope(name, scope)
		if err != nil {
			if _, ok := err.(*deploymentNotFoundError); ok {
				_, _ = fmt.Fprintf(stdout, "skipped %s (not installed in %s scope)\n", name, scope)
				continue
			}
			return fmt.Errorf("deploy uninstall: %w", err)
		}
		lookup := &deployLookupOpts{
			name:        name,
			scope:       scope,
			force:       opts.force,
			dryRun:      opts.dryRun,
			interactive: false,
			stdin:       opts.stdin,
		}
		if err := runDeployDeleteRecord(ctx, record, lookup, stdout); err != nil {
			return fmt.Errorf("deploy uninstall: %w", err)
		}
	}

	if opts.dryRun {
		if opts.socket != "" {
			_, _ = fmt.Fprintf(stdout, "dry-run: would remove supervisor socket %s\n", opts.socket)
		}
		if opts.stateDir != "" {
			_, _ = fmt.Fprintf(stdout, "dry-run: would remove supervisor state dir %s\n", opts.stateDir)
		}
		return nil
	}
	if opts.socket != "" {
		if err := os.Remove(opts.socket); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("deploy uninstall: remove socket %s: %w", opts.socket, err)
		}
		_, _ = fmt.Fprintf(stdout, "removed supervisor socket %s\n", opts.socket)
	}
	if opts.stateDir != "" {
		if err := os.RemoveAll(opts.stateDir); err != nil {
			return fmt.Errorf("deploy uninstall: remove state dir %s: %w", opts.stateDir, err)
		}
		_, _ = fmt.Fprintf(stdout, "removed supervisor state dir %s\n", opts.stateDir)
	}
	return nil
}

// detectV04Install reports whether the given scope contains a v0.4-style
// bundle: coordinator and worker manifests exist but supervisor does not.
// Used by `deploy upgrade --bundle` to know if it should add the missing
// supervisor unit.
func detectV04Install(scope string) (bool, error) {
	if scope == "" {
		scope = defaultDeployScope
	}
	hasName := func(name string) (bool, error) {
		_, err := loadDeploymentRecordForScope(name, scope)
		if err == nil {
			return true, nil
		}
		if _, ok := err.(*deploymentNotFoundError); ok {
			return false, nil
		}
		return false, err
	}
	hasSupervisor, err := hasName(bundleSupervisorName)
	if err != nil {
		return false, err
	}
	if hasSupervisor {
		return false, nil
	}
	hasCoordinator, err := hasName(bundleCoordinatorName)
	if err != nil {
		return false, err
	}
	hasWorker, err := hasName(bundleWorkerName)
	if err != nil {
		return false, err
	}
	return hasCoordinator || hasWorker, nil
}

// supervisorBinaryFromBundle returns the binary path recorded in either the
// coordinator or worker manifest, used to seed the supervisor manifest when
// migrating a v0.4 install. Returns "" if neither exists.
func supervisorBinaryFromBundle(scope string) (binaryPath, workingDir string, envFiles []string) {
	for _, name := range []string{bundleCoordinatorName, bundleWorkerName} {
		record, err := loadDeploymentRecordForScope(name, scope)
		if err != nil {
			continue
		}
		if record == nil || record.manifest == nil {
			continue
		}
		if record.manifest.Systemd != nil {
			envFiles = append([]string(nil), record.manifest.Systemd.EnvironmentFiles...)
		}
		return record.manifest.BinaryPath, record.manifest.WorkingDirectory, envFiles
	}
	return "", "", nil
}

// ensureSupervisorUnitForUpgrade installs the supervisor unit in-place when
// `deploy upgrade` runs against a legacy v0.4 install (only coordinator and
// worker present). It is a no-op when the supervisor manifest already exists
// or no v0.4 bundle is detected.
func ensureSupervisorUnitForUpgrade(ctx context.Context, scope string, stdout io.Writer) error {
	legacy, err := detectV04Install(scope)
	if err != nil {
		return err
	}
	if !legacy {
		return nil
	}
	binaryPath, workingDir, envFiles := supervisorBinaryFromBundle(scope)
	if binaryPath == "" {
		return nil
	}
	// Resolve workingDir to an existing absolute path; fall back to /
	// if the recorded directory has been removed (rare).
	if workingDir == "" {
		workingDir = filepath.Dir(binaryPath)
	}
	_, _ = fmt.Fprintf(stdout, "v0.4 install detected — adding %s.service\n", bundleSupervisorName)
	opts := &bundleInstallOpts{
		scope:            scope,
		binaryPath:       binaryPath,
		workingDirectory: workingDir,
		envFiles:         envFiles,
		enable:           true,
		start:            true,
		skipCoordinator:  true,
		skipWorker:       true,
	}
	return runDeployBundleInstallWithOpts(ctx, opts, stdout)
}
