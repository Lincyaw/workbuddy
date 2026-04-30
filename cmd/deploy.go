package cmd

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Lincyaw/workbuddy/internal/app"
	"github.com/spf13/cobra"
)

const (
	defaultDeployName        = "workbuddy"
	defaultDeployScope       = "user"
	defaultDeployListScope   = "all"
	defaultUpgradeRepo       = "Lincyaw/workbuddy"
	defaultDeployHTTPTimeout = 30 * time.Second
	deploymentManifestVer    = 1
)

var (
	deployExecutablePath     = os.Executable
	deployNow                = time.Now
	deployRunSystemctl       = runSystemctl
	deployHTTPClient         = &http.Client{Timeout: defaultDeployHTTPTimeout}
	deployGitHubAPIBaseURL   = "https://api.github.com"
	deployGitHubDownloadBase = "https://github.com"
	deployNamePattern        = regexp.MustCompile(`^[A-Za-z0-9@_.-]+$`)
	deploySystemBinaryPath   = "/usr/local/bin/workbuddy"
	deploySystemManifestDir  = "/etc/workbuddy/deployments"
	deploySystemUnitDir      = "/etc/systemd/system"
)

type deployInstallOpts struct {
	name                string
	scope               string
	binaryPath          string
	workingDirectory    string
	systemd             bool
	description         string
	env                 []string
	envFiles            []string
	enable              bool
	start               bool
	commandArgs         []string
	enableUpdater       bool
	updaterRepo         string
	updaterInterval     string
	updaterRestartUnits []string
	updaterName         string
}

type deployLookupOpts struct {
	name        string
	scope       string
	all         bool
	force       bool
	dryRun      bool
	interactive bool
	stdin       io.Reader
}

type deployUpgradeOpts struct {
	deployLookupOpts
	version    string
	repository string
}

type deployListOpts struct {
	scope  string
	format string
}

type deploymentManifest struct {
	SchemaVersion    int                `json:"schema_version"`
	Name             string             `json:"name"`
	Scope            string             `json:"scope"`
	BinaryPath       string             `json:"binary_path"`
	WorkingDirectory string             `json:"working_directory"`
	Command          []string           `json:"command,omitempty"`
	InstalledVersion string             `json:"installed_version,omitempty"`
	InstalledAt      time.Time          `json:"installed_at"`
	Systemd          *deploymentSystemd `json:"systemd,omitempty"`
}

type deploymentSystemd struct {
	ServiceName      string            `json:"service_name"`
	UnitPath         string            `json:"unit_path"`
	Description      string            `json:"description"`
	Enabled          bool              `json:"enabled"`
	Started          bool              `json:"started"`
	Environment      map[string]string `json:"environment,omitempty"`
	EnvironmentFiles []string          `json:"environment_files,omitempty"`
	// Type is the systemd Type= value (e.g. "simple", "notify", "exec").
	// Defaults to "simple" when empty (back-compat with v0.4 manifests).
	Type string `json:"type,omitempty"`
	// KillMode overrides the unit's KillMode= setting (e.g. "process").
	// Empty means systemd default ("control-group").
	KillMode string `json:"kill_mode,omitempty"`
	// Restart overrides the Restart= setting (e.g. "always"). Empty
	// keeps the legacy "on-failure" used by v0.4 deployments.
	Restart string `json:"restart,omitempty"`
	// After lists additional unit names to add to After= ordering.
	After []string `json:"after,omitempty"`
}

type deployScopePaths struct {
	defaultBinaryPath string
	manifestDir       string
	unitDir           string
	wantedBy          string
}

type deploymentRecord struct {
	manifest     *deploymentManifest
	manifestPath string
	scopePaths   *deployScopePaths
}

type deploymentNotFoundError struct {
	name      string
	scope     string
	installed []string
}

type deployListRow struct {
	Name       string   `json:"name"`
	Scope      string   `json:"scope"`
	BinaryPath string   `json:"binary_path"`
	Command    []string `json:"command"`
}

type deployListResponse struct {
	Deployments []deployListRow `json:"deployments"`
}

var deployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Install and manage deployed workbuddy services",
	Long:  "Install the current workbuddy binary, optionally wire it into systemd, and keep enough deployment state to support later redeploy and upgrade operations for serve, coordinator, or worker runtimes.",
}

var deployInstallCmd = &cobra.Command{
	Use:   "install [-- workbuddy args...]",
	Short: "Install the current binary and optionally create a systemd unit",
	Example: strings.TrimSpace(`
  # v0.5 bundled install: supervisor + coordinator + worker user units
  workbuddy deploy install --bundle --scope user \
    --env-file /home/ddq/.config/workbuddy/worker.env \
    --coordinator-args=--listen=127.0.0.1:8081 --coordinator-args=--auth \
    --worker-args=--coordinator=http://127.0.0.1:8081 --worker-args=--token=$WORKBUDDY_TOKEN

  # Single-process deploy (default command is "serve")
  workbuddy deploy install --name workbuddy --scope user --systemd

  # Dedicated coordinator service (non-loopback bind requires --report-base-url)
  # Pair with --env-file pointing at a file that defines WORKBUDDY_REPORT_BASE_URL
  # so session links posted to GitHub comments are clickable in a browser.
  workbuddy deploy install --name workbuddy-coordinator --scope system --systemd \
    --env-file /etc/workbuddy/coordinator.env -- \
    coordinator --listen 0.0.0.0:8081 \
      --report-base-url=https://workbuddy.example.com:8081 \
      --auth --db /srv/workbuddy/.workbuddy/workbuddy.db

  # Dedicated worker service bound to a coordinator
  workbuddy deploy install --name workbuddy-worker-dev --scope system --systemd -- \
    worker --coordinator http://127.0.0.1:8081 --token <token> --role dev --repos owner/repo=/srv/workbuddy-worker
`),
	Args: cobra.ArbitraryArgs,
	RunE: runDeployInstallCmd,
}

var deployListCmd = &cobra.Command{
	Use:   "list",
	Short: "List recorded deployments from user and system scopes",
	RunE:  runDeployListCmd,
}

var deployRedeployCmd = &cobra.Command{
	Use:   "redeploy",
	Short: "Reinstall the current binary and restart the recorded deployment",
	RunE:  runDeployRedeployCmd,
}

var deployStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop a deployed systemd service without disabling it",
	RunE:  runDeployStopCmd,
}

var deployStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start a deployed systemd service without enabling it",
	RunE:  runDeployStartCmd,
}

var deployDeleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete a recorded deployment and remove its systemd unit",
	RunE:  runDeployDeleteCmd,
}

var deployUpgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Download a release binary into an existing deployment and restart it",
	RunE:  runDeployUpgradeCmd,
}

func init() {
	deployInstallCmd.Flags().String("name", defaultDeployName, "Deployment name; also used for the systemd service name")
	deployInstallCmd.Flags().String("scope", defaultDeployScope, "Deployment scope: user or system")
	deployInstallCmd.Flags().String("binary", "", "Install path for the deployed binary")
	deployInstallCmd.Flags().String("working-directory", "", "Working directory used by the deployed process (default: current directory)")
	deployInstallCmd.Flags().Bool("systemd", false, "Also install or update a systemd unit for this deployment")
	deployInstallCmd.Flags().String("description", "", "Optional systemd unit description")
	deployInstallCmd.Flags().StringArray("env", nil, "Environment variable for the service in KEY=VALUE form (repeatable)")
	deployInstallCmd.Flags().StringArray("env-file", nil, "Environment file for the service (repeatable)")
	deployInstallCmd.Flags().Bool("enable", true, "Enable the systemd unit after writing it")
	deployInstallCmd.Flags().Bool("start", true, "Start the systemd unit after writing it")
	deployInstallCmd.Flags().Bool("enable-updater", false, "Also install and enable the workbuddy-updater.service systemd user unit that runs `deploy watch` to pull releases from GitHub")
	deployInstallCmd.Flags().String("updater-repo", defaultUpgradeRepo, "GitHub repository in OWNER/NAME form used by the updater unit (only consulted with --enable-updater)")
	deployInstallCmd.Flags().String("updater-interval", "5m", "Poll interval used by the updater unit (only consulted with --enable-updater)")
	deployInstallCmd.Flags().StringSlice("updater-restart-units", []string{"workbuddy-coordinator.service", "workbuddy-worker.service"}, "systemd unit name(s) the updater should restart after a successful upgrade (only consulted with --enable-updater)")
	deployInstallCmd.Flags().String("updater-name", "workbuddy-updater", "Service name used for the updater unit (only consulted with --enable-updater)")

	deployListCmd.Flags().String("scope", defaultDeployListScope, "Deployment scope: all, user, or system")
	addOutputFormatFlag(deployListCmd)
	addDeprecatedJSONAliasFlag(deployListCmd)

	deployRedeployCmd.Flags().String("name", defaultDeployName, "Deployment name")
	deployRedeployCmd.Flags().String("scope", defaultDeployScope, "Deployment scope: user or system")
	deployRedeployCmd.Flags().Bool("all", false, "Operate on every deployment in the requested scope")

	deployStopCmd.Flags().String("name", defaultDeployName, "Deployment name")
	deployStopCmd.Flags().String("scope", defaultDeployScope, "Deployment scope: user or system")
	deployStopCmd.Flags().Bool("all", false, "Operate on every deployment in the requested scope")
	deployStopCmd.Flags().Bool("force", false, "Skip confirmation prompts for destructive actions")
	deployStopCmd.Flags().Bool("dry-run", false, "Print the actions that would be taken without executing them")

	deployStartCmd.Flags().String("name", defaultDeployName, "Deployment name")
	deployStartCmd.Flags().String("scope", defaultDeployScope, "Deployment scope: user or system")
	deployStartCmd.Flags().Bool("all", false, "Operate on every deployment in the requested scope")

	deployDeleteCmd.Flags().String("name", defaultDeployName, "Deployment name")
	deployDeleteCmd.Flags().String("scope", defaultDeployScope, "Deployment scope: user or system")
	deployDeleteCmd.Flags().Bool("all", false, "Operate on every deployment in the requested scope")
	deployDeleteCmd.Flags().Bool("force", false, "Skip confirmation prompts for destructive actions")
	deployDeleteCmd.Flags().Bool("dry-run", false, "Print the actions that would be taken without executing them")

	deployUpgradeCmd.Flags().String("name", defaultDeployName, "Deployment name")
	deployUpgradeCmd.Flags().String("scope", defaultDeployScope, "Deployment scope: user or system")
	deployUpgradeCmd.Flags().String("version", "latest", "Release version to install (for example latest or v0.2.0)")
	deployUpgradeCmd.Flags().String("repository", defaultUpgradeRepo, "GitHub repository used for release upgrades in OWNER/NAME form")
	deployUpgradeCmd.Flags().Bool("all", false, "Operate on every deployment in the requested scope")
	deployUpgradeCmd.Flags().Bool("force", false, "Skip confirmation prompts for destructive actions")
	deployUpgradeCmd.Flags().Bool("dry-run", false, "Print the actions that would be taken without executing them")
	deployUpgradeCmd.Flags().Bool("bundle", false, "After upgrading, ensure the v0.5 supervisor unit is installed (used to migrate a v0.4 install that only has coordinator+worker)")

	deployCmd.AddCommand(deployInstallCmd, deployListCmd, deployRedeployCmd, deployStopCmd, deployStartCmd, deployDeleteCmd, deployUpgradeCmd)
	rootCmd.AddCommand(deployCmd)
}

func runDeployInstallCmd(cmd *cobra.Command, args []string) error {
	if err := requireWritable(cmd, "deploy install"); err != nil {
		return err
	}
	if bundleEnabled(cmd) {
		bundleOpts, err := parseBundleInstallFlags(cmd)
		if err != nil {
			return err
		}
		if len(args) > 0 {
			return fmt.Errorf("deploy install: --bundle does not accept trailing -- args (use --supervisor-args/--coordinator-args/--worker-args)")
		}
		if cmd.Flags().Changed("name") {
			return fmt.Errorf("deploy install: --bundle ignores --name; remove the flag")
		}
		return runDeployBundleInstallWithOpts(cmd.Context(), bundleOpts, cmdStdout(cmd))
	}
	opts, err := parseDeployInstallFlags(cmd, args)
	if err != nil {
		return err
	}
	return runDeployInstallWithOpts(cmd.Context(), opts, cmdStdout(cmd))
}

func runDeployListCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseDeployListFlags(cmd)
	if err != nil {
		return err
	}
	return runDeployListWithOpts(opts, cmd.OutOrStdout())
}

func runDeployRedeployCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseDeployLookupFlags(cmd)
	if err != nil {
		return err
	}
	if err := requireWritable(cmd, "deploy redeploy"); err != nil {
		return err
	}
	return runDeployRedeployWithOpts(cmd.Context(), opts, cmdStdout(cmd))
}

func runDeployStopCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseDeployLookupFlags(cmd)
	if err != nil {
		return err
	}
	if err := requireWritable(cmd, "deploy stop"); err != nil {
		return err
	}
	return runDeployStopWithOpts(cmd.Context(), opts, cmdStdout(cmd))
}

func runDeployStartCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseDeployLookupFlags(cmd)
	if err != nil {
		return err
	}
	if err := requireWritable(cmd, "deploy start"); err != nil {
		return err
	}
	return runDeployStartWithOpts(cmd.Context(), opts, cmdStdout(cmd))
}

func runDeployDeleteCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseDeployLookupFlags(cmd)
	if err != nil {
		return err
	}
	if err := requireWritable(cmd, "deploy delete"); err != nil {
		return err
	}
	return runDeployDeleteWithOpts(cmd.Context(), opts, cmdStdout(cmd))
}

func runDeployUpgradeCmd(cmd *cobra.Command, _ []string) error {
	lookup, err := parseDeployLookupFlags(cmd)
	if err != nil {
		return err
	}
	if err := requireWritable(cmd, "deploy upgrade"); err != nil {
		return err
	}
	version, _ := cmd.Flags().GetString("version")
	repository, _ := cmd.Flags().GetString("repository")
	if err := runDeployUpgradeWithOpts(cmd.Context(), &deployUpgradeOpts{
		deployLookupOpts: *lookup,
		version:          strings.TrimSpace(version),
		repository:       strings.TrimSpace(repository),
	}, cmdStdout(cmd)); err != nil {
		return err
	}
	if bundle, _ := cmd.Flags().GetBool("bundle"); bundle {
		return ensureSupervisorUnitForUpgrade(cmd.Context(), lookup.scope, cmdStdout(cmd))
	}
	return nil
}

func parseDeployListFlags(cmd *cobra.Command) (*deployListOpts, error) {
	scope, _ := cmd.Flags().GetString("scope")
	format, err := resolveOutputFormat(cmd, "deploy list")
	if err != nil {
		return nil, err
	}
	if _, err := resolveDeployScopes(scope, defaultDeployListScope); err != nil {
		return nil, fmt.Errorf("deploy list: %w", err)
	}
	return &deployListOpts{
		scope:  strings.TrimSpace(scope),
		format: format,
	}, nil
}

func parseDeployInstallFlags(cmd *cobra.Command, args []string) (*deployInstallOpts, error) {
	name, _ := cmd.Flags().GetString("name")
	scope, _ := cmd.Flags().GetString("scope")
	binaryPath, _ := cmd.Flags().GetString("binary")
	workingDir, _ := cmd.Flags().GetString("working-directory")
	systemd, _ := cmd.Flags().GetBool("systemd")
	description, _ := cmd.Flags().GetString("description")
	envVars, _ := cmd.Flags().GetStringArray("env")
	envFiles, _ := cmd.Flags().GetStringArray("env-file")
	enable, _ := cmd.Flags().GetBool("enable")
	start, _ := cmd.Flags().GetBool("start")
	enableUpdater, _ := cmd.Flags().GetBool("enable-updater")
	updaterRepo, _ := cmd.Flags().GetString("updater-repo")
	updaterInterval, _ := cmd.Flags().GetString("updater-interval")
	updaterRestartUnits, _ := cmd.Flags().GetStringSlice("updater-restart-units")
	updaterName, _ := cmd.Flags().GetString("updater-name")

	return &deployInstallOpts{
		name:                strings.TrimSpace(name),
		scope:               strings.TrimSpace(scope),
		binaryPath:          strings.TrimSpace(binaryPath),
		workingDirectory:    strings.TrimSpace(workingDir),
		systemd:             systemd,
		description:         strings.TrimSpace(description),
		env:                 envVars,
		envFiles:            trimStringSlice(envFiles),
		enable:              enable,
		start:               start,
		commandArgs:         append([]string(nil), args...),
		enableUpdater:       enableUpdater,
		updaterRepo:         strings.TrimSpace(updaterRepo),
		updaterInterval:     strings.TrimSpace(updaterInterval),
		updaterRestartUnits: trimStringSlice(updaterRestartUnits),
		updaterName:         strings.TrimSpace(updaterName),
	}, nil
}

func parseDeployLookupFlags(cmd *cobra.Command) (*deployLookupOpts, error) {
	name, _ := cmd.Flags().GetString("name")
	scope, _ := cmd.Flags().GetString("scope")
	all, _ := cmd.Flags().GetBool("all")
	if all && cmd.Flags().Changed("name") {
		return nil, fmt.Errorf("deploy %s: --name and --all are mutually exclusive", cmd.Name())
	}
	force := getBoolFlagIfDefined(cmd, "force")
	dryRun := getBoolFlagIfDefined(cmd, "dry-run")
	return &deployLookupOpts{
		name:        strings.TrimSpace(name),
		scope:       strings.TrimSpace(scope),
		all:         all,
		force:       force,
		dryRun:      dryRun,
		interactive: commandIsInteractiveTerminal(),
		stdin:       cmd.InOrStdin(),
	}, nil
}

func getBoolFlagIfDefined(cmd *cobra.Command, name string) bool {
	if cmd.Flags().Lookup(name) == nil {
		return false
	}
	v, _ := cmd.Flags().GetBool(name)
	return v
}

func runDeployInstallWithOpts(ctx context.Context, opts *deployInstallOpts, stdout io.Writer) error {
	if opts == nil {
		return fmt.Errorf("deploy install: options are required")
	}
	name, scope, scopePaths, err := validateDeployIdentity(opts.name, opts.scope)
	if err != nil {
		return fmt.Errorf("deploy install: %w", err)
	}

	binaryPath, err := resolveBinaryPath(scopePaths.defaultBinaryPath, opts.binaryPath)
	if err != nil {
		return fmt.Errorf("deploy install: %w", err)
	}
	workingDir, err := resolveWorkingDirectory(opts.workingDirectory)
	if err != nil {
		return fmt.Errorf("deploy install: %w", err)
	}
	commandArgs, err := normalizeDeployCommandArgs(opts.commandArgs)
	if err != nil {
		return fmt.Errorf("deploy install: %w", err)
	}
	serviceEnv, err := parseDeployEnv(opts.env)
	if err != nil {
		return fmt.Errorf("deploy install: %w", err)
	}
	if opts.systemd && runtime.GOOS != "linux" {
		return fmt.Errorf("deploy install: systemd deployment is only supported on Linux")
	}

	sourceBinary, err := deployExecutablePath()
	if err != nil {
		return fmt.Errorf("deploy install: locate current executable: %w", err)
	}
	if err := copyFileAtomic(sourceBinary, binaryPath, 0o755); err != nil {
		return fmt.Errorf("deploy install: install binary: %w", err)
	}

	manifest := &deploymentManifest{
		SchemaVersion:    deploymentManifestVer,
		Name:             name,
		Scope:            scope,
		BinaryPath:       binaryPath,
		WorkingDirectory: workingDir,
		Command:          commandArgs,
		InstalledVersion: deployedVersionLabel(),
		InstalledAt:      deployNow().UTC(),
	}

	manifestPath := deploymentManifestPath(scopePaths.manifestDir, name)
	if opts.systemd {
		manifest.Systemd = &deploymentSystemd{
			ServiceName:      name,
			UnitPath:         filepath.Join(scopePaths.unitDir, name+".service"),
			Description:      defaultDeployDescription(name, commandArgs, opts.description),
			Enabled:          opts.enable,
			Started:          opts.start,
			Environment:      serviceEnv,
			EnvironmentFiles: trimStringSlice(opts.envFiles),
		}
		unit, err := renderSystemdUnit(manifest, scopePaths.wantedBy)
		if err != nil {
			return fmt.Errorf("deploy install: render systemd unit: %w", err)
		}
		if err := writeTextFileAtomic(manifest.Systemd.UnitPath, unit, deploymentUnitFileMode(manifest)); err != nil {
			return fmt.Errorf("deploy install: write systemd unit: %w", err)
		}
	}

	if err := writeDeploymentManifest(manifestPath, manifest); err != nil {
		return fmt.Errorf("deploy install: write manifest: %w", err)
	}

	if _, err := fmt.Fprintf(stdout, "installed binary to %s\n", binaryPath); err != nil {
		return fmt.Errorf("deploy install: write output: %w", err)
	}
	if _, err := fmt.Fprintf(stdout, "wrote deployment manifest %s\n", manifestPath); err != nil {
		return fmt.Errorf("deploy install: write output: %w", err)
	}

	if manifest.Systemd == nil {
		if opts.enableUpdater {
			if err := installUpdaterUnit(ctx, opts, scope, scopePaths, binaryPath, workingDir, stdout); err != nil {
				return err
			}
		}
		return nil
	}

	if _, err := fmt.Fprintf(stdout, "wrote systemd unit %s\n", manifest.Systemd.UnitPath); err != nil {
		return fmt.Errorf("deploy install: write output: %w", err)
	}
	if err := deployRunSystemctl(ctx, scope, "daemon-reload"); err != nil {
		return fmt.Errorf("deploy install: %w", err)
	}
	serviceName := manifest.Systemd.ServiceName + ".service"
	if opts.enable {
		if err := deployRunSystemctl(ctx, scope, "enable", serviceName); err != nil {
			return fmt.Errorf("deploy install: %w", err)
		}
		if _, err := fmt.Fprintf(stdout, "enabled %s\n", serviceName); err != nil {
			return fmt.Errorf("deploy install: write output: %w", err)
		}
	}
	if opts.start {
		if err := deployRunSystemctl(ctx, scope, "start", serviceName); err != nil {
			return fmt.Errorf("deploy install: %w", err)
		}
		if _, err := fmt.Fprintf(stdout, "started %s\n", serviceName); err != nil {
			return fmt.Errorf("deploy install: write output: %w", err)
		}
	}
	if opts.enableUpdater {
		if err := installUpdaterUnit(ctx, opts, scope, scopePaths, binaryPath, workingDir, stdout); err != nil {
			return err
		}
	}
	return nil
}

// installUpdaterUnit writes the workbuddy-updater.service systemd unit, runs
// daemon-reload, and enables + starts it. The updater runs `workbuddy deploy
// watch` so it pulls new releases on a schedule (Phase 2.3 of #224). It
// reuses the same scope/EnvironmentFile as the main install so the user can
// keep a single env file containing the GitHub token used for release reads.
func installUpdaterUnit(
	ctx context.Context,
	opts *deployInstallOpts,
	scope string,
	scopePaths *deployScopePaths,
	binaryPath, workingDir string,
	stdout io.Writer,
) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("deploy install: --enable-updater requires Linux")
	}
	updaterName := opts.updaterName
	if updaterName == "" {
		updaterName = "workbuddy-updater"
	}
	if !deployNamePattern.MatchString(updaterName) {
		return fmt.Errorf("deploy install: invalid --updater-name %q", updaterName)
	}
	repo := opts.updaterRepo
	if repo == "" {
		repo = defaultUpgradeRepo
	}
	if !strings.Contains(repo, "/") {
		return fmt.Errorf("deploy install: --updater-repo must be owner/name, got %q", repo)
	}
	interval := opts.updaterInterval
	if interval == "" {
		interval = "5m"
	}
	if _, err := time.ParseDuration(interval); err != nil {
		return fmt.Errorf("deploy install: invalid --updater-interval %q: %w", interval, err)
	}
	restartUnits := trimStringSlice(opts.updaterRestartUnits)

	watchArgs := []string{
		"deploy", "watch",
		"--repo", repo,
		"--interval", interval,
		"--systemctl-scope", scope,
	}
	if len(restartUnits) > 0 {
		watchArgs = append(watchArgs, "--restart-units", strings.Join(restartUnits, ","))
	}
	execArgs := append([]string{binaryPath}, watchArgs...)
	for _, arg := range execArgs {
		if strings.ContainsRune(arg, '\n') {
			return fmt.Errorf("deploy install: updater command arguments may not contain newlines")
		}
	}

	unitPath := filepath.Join(scopePaths.unitDir, updaterName+".service")
	description := fmt.Sprintf("Workbuddy %s (deploy watch)", updaterName)

	var b strings.Builder
	b.WriteString("[Unit]\n")
	fmt.Fprintf(&b, "Description=%s\n", escapeUnitValue(description))
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n\n")

	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	fmt.Fprintf(&b, "WorkingDirectory=%s\n", systemdSingleValue(workingDir))
	fmt.Fprintf(&b, "ExecStart=%s\n", joinSystemdCommand(execArgs))
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=30s\n")
	for _, envFile := range trimStringSlice(opts.envFiles) {
		fmt.Fprintf(&b, "EnvironmentFile=%s\n", systemdSingleValue(envFile))
	}

	b.WriteString("\n[Install]\n")
	fmt.Fprintf(&b, "WantedBy=%s\n", scopePaths.wantedBy)

	if err := writeTextFileAtomic(unitPath, b.String(), 0o644); err != nil {
		return fmt.Errorf("deploy install: write updater unit: %w", err)
	}
	if _, err := fmt.Fprintf(stdout, "wrote updater unit %s\n", unitPath); err != nil {
		return fmt.Errorf("deploy install: write output: %w", err)
	}
	if err := deployRunSystemctl(ctx, scope, "daemon-reload"); err != nil {
		return fmt.Errorf("deploy install: %w", err)
	}
	updaterServiceName := updaterName + ".service"
	if err := deployRunSystemctl(ctx, scope, "enable", updaterServiceName); err != nil {
		return fmt.Errorf("deploy install: %w", err)
	}
	if _, err := fmt.Fprintf(stdout, "enabled %s\n", updaterServiceName); err != nil {
		return fmt.Errorf("deploy install: write output: %w", err)
	}
	if err := deployRunSystemctl(ctx, scope, "start", updaterServiceName); err != nil {
		return fmt.Errorf("deploy install: %w", err)
	}
	if _, err := fmt.Fprintf(stdout, "started %s\n", updaterServiceName); err != nil {
		return fmt.Errorf("deploy install: write output: %w", err)
	}
	return nil
}

func runDeployListWithOpts(opts *deployListOpts, stdout io.Writer) error {
	if opts == nil {
		return fmt.Errorf("deploy list: options are required")
	}
	rows, err := collectDeployListRows(opts.scope)
	if err != nil {
		return fmt.Errorf("deploy list: %w", err)
	}
	if opts.format == "json" {
		payload, err := json.MarshalIndent(deployListResponse{Deployments: rows}, "", "  ")
		if err != nil {
			return fmt.Errorf("deploy list: marshal output: %w", err)
		}
		payload = append(payload, '\n')
		if _, err := stdout.Write(payload); err != nil {
			return fmt.Errorf("deploy list: write output: %w", err)
		}
		return nil
	}
	if len(rows) == 0 {
		if _, err := fmt.Fprintln(stdout, "no deployments found"); err != nil {
			return fmt.Errorf("deploy list: write output: %w", err)
		}
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tSCOPE\tBINARY PATH\tCOMMAND"); err != nil {
		return fmt.Errorf("deploy list: write output: %w", err)
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(
			tw,
			"%s\t%s\t%s\t%s\n",
			row.Name,
			row.Scope,
			row.BinaryPath,
			renderDeployCommand(row.Command),
		); err != nil {
			return fmt.Errorf("deploy list: write output: %w", err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("deploy list: flush output: %w", err)
	}
	return nil
}

func runDeployRedeployWithOpts(ctx context.Context, opts *deployLookupOpts, stdout io.Writer) error {
	if opts == nil {
		return fmt.Errorf("deploy redeploy: options are required")
	}
	if opts.all {
		return runDeployAcrossScope(ctx, opts.scope, stdout, "deploy redeploy", runDeployRedeployRecord)
	}
	record, err := loadDeploymentRecordForScope(opts.name, opts.scope)
	if err != nil {
		return fmt.Errorf("deploy redeploy: %w", err)
	}
	return runDeployRedeployRecord(ctx, record, stdout)
}

func runDeployRedeployRecord(ctx context.Context, record *deploymentRecord, stdout io.Writer) error {
	if record == nil || record.manifest == nil {
		return fmt.Errorf("deploy redeploy: deployment record is required")
	}
	manifest := record.manifest
	sourceBinary, err := deployExecutablePath()
	if err != nil {
		return fmt.Errorf("deploy redeploy: locate current executable: %w", err)
	}
	if err := copyFileAtomic(sourceBinary, manifest.BinaryPath, 0o755); err != nil {
		return fmt.Errorf("deploy redeploy: install binary: %w", err)
	}
	manifest.InstalledVersion = deployedVersionLabel()
	manifest.InstalledAt = deployNow().UTC()

	if manifest.Systemd != nil {
		if runtime.GOOS != "linux" {
			return fmt.Errorf("deploy redeploy: systemd deployment is only supported on Linux")
		}
		unit, err := renderSystemdUnit(manifest, record.scopePaths.wantedBy)
		if err != nil {
			return fmt.Errorf("deploy redeploy: render systemd unit: %w", err)
		}
		if err := writeTextFileAtomic(manifest.Systemd.UnitPath, unit, deploymentUnitFileMode(manifest)); err != nil {
			return fmt.Errorf("deploy redeploy: write systemd unit: %w", err)
		}
	}
	if err := writeDeploymentManifest(record.manifestPath, manifest); err != nil {
		return fmt.Errorf("deploy redeploy: write manifest: %w", err)
	}

	if _, err := fmt.Fprintf(stdout, "reinstalled binary to %s\n", manifest.BinaryPath); err != nil {
		return fmt.Errorf("deploy redeploy: write output: %w", err)
	}
	if manifest.Systemd == nil {
		return nil
	}
	if err := syncSystemdDeploymentState(ctx, "deploy redeploy", manifest, stdout); err != nil {
		return err
	}
	return nil
}

func runDeployStopWithOpts(ctx context.Context, opts *deployLookupOpts, stdout io.Writer) error {
	if opts == nil {
		return fmt.Errorf("deploy stop: options are required")
	}
	runOne := func(ctx context.Context, record *deploymentRecord, stdout io.Writer) error {
		return runDeployStopRecord(ctx, record, opts, stdout)
	}
	if opts.all {
		return runDeployAcrossScope(ctx, opts.scope, stdout, "deploy stop", runOne)
	}
	record, err := loadDeploymentRecordForScope(opts.name, opts.scope)
	if err != nil {
		return fmt.Errorf("deploy stop: %w", err)
	}
	return runOne(ctx, record, stdout)
}

func runDeployStopRecord(ctx context.Context, record *deploymentRecord, opts *deployLookupOpts, stdout io.Writer) error {
	if record == nil || record.manifest == nil {
		return fmt.Errorf("deploy stop: deployment record is required")
	}
	manifest := record.manifest
	manifestPath := record.manifestPath
	if manifest.Systemd == nil {
		return fmt.Errorf("deploy stop: deployment %q is not managed by systemd", manifest.Name)
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("deploy stop: systemd deployment is only supported on Linux")
	}

	serviceName := manifest.Systemd.ServiceName + ".service"
	if opts.dryRun {
		_, _ = fmt.Fprintf(stdout, "dry-run: would stop %s\n", serviceName)
		_, _ = fmt.Fprintf(stdout, "dry-run: would write deployment manifest %s\n", manifestPath)
		return nil
	}
	ok, err := confirmDestructiveAction(
		"deploy stop",
		opts.stdin,
		stdout,
		opts.interactive,
		opts.force,
		opts.dryRun,
		fmt.Sprintf("Stop deployment %q?", manifest.Name),
		[]string{
			fmt.Sprintf("stop systemd service %s", serviceName),
			fmt.Sprintf("update deployment manifest %s", manifestPath),
		},
	)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := deployRunSystemctl(ctx, manifest.Scope, "stop", serviceName); err != nil {
		return fmt.Errorf("deploy stop: %w", err)
	}
	_ = deployRunSystemctl(ctx, manifest.Scope, "reset-failed", serviceName)
	manifest.Systemd.Started = false
	if err := writeDeploymentManifest(manifestPath, manifest); err != nil {
		return fmt.Errorf("deploy stop: write manifest: %w", err)
	}
	if _, err := fmt.Fprintf(stdout, "stopped %s\n", serviceName); err != nil {
		return fmt.Errorf("deploy stop: write output: %w", err)
	}
	return nil
}

func runDeployStartWithOpts(ctx context.Context, opts *deployLookupOpts, stdout io.Writer) error {
	if opts == nil {
		return fmt.Errorf("deploy start: options are required")
	}
	if opts.all {
		return runDeployAcrossScope(ctx, opts.scope, stdout, "deploy start", runDeployStartRecord)
	}
	record, err := loadDeploymentRecordForScope(opts.name, opts.scope)
	if err != nil {
		return fmt.Errorf("deploy start: %w", err)
	}
	return runDeployStartRecord(ctx, record, stdout)
}

func runDeployStartRecord(ctx context.Context, record *deploymentRecord, stdout io.Writer) error {
	if record == nil || record.manifest == nil {
		return fmt.Errorf("deploy start: deployment record is required")
	}
	manifest := record.manifest
	if manifest.Systemd == nil {
		return fmt.Errorf("deploy start: deployment %q is not managed by systemd", manifest.Name)
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("deploy start: systemd deployment is only supported on Linux")
	}

	if err := deployRunSystemctl(ctx, manifest.Scope, "daemon-reload"); err != nil {
		return fmt.Errorf("deploy start: %w", err)
	}
	serviceName := manifest.Systemd.ServiceName + ".service"
	if err := deployRunSystemctl(ctx, manifest.Scope, "start", serviceName); err != nil {
		return fmt.Errorf("deploy start: %w", err)
	}
	manifest.Systemd.Started = true
	if err := writeDeploymentManifest(record.manifestPath, manifest); err != nil {
		return fmt.Errorf("deploy start: write manifest: %w", err)
	}
	if _, err := fmt.Fprintf(stdout, "started %s\n", serviceName); err != nil {
		return fmt.Errorf("deploy start: write output: %w", err)
	}
	return nil
}

func runDeployDeleteWithOpts(ctx context.Context, opts *deployLookupOpts, stdout io.Writer) error {
	if opts == nil {
		return fmt.Errorf("deploy delete: options are required")
	}
	runOne := func(ctx context.Context, record *deploymentRecord, stdout io.Writer) error {
		return runDeployDeleteRecord(ctx, record, opts, stdout)
	}
	if opts.all {
		return runDeployAcrossScope(ctx, opts.scope, stdout, "deploy delete", runOne)
	}
	record, err := loadDeploymentRecordForScope(opts.name, opts.scope)
	if err != nil {
		return fmt.Errorf("deploy delete: %w", err)
	}
	return runOne(ctx, record, stdout)
}

func runDeployDeleteRecord(ctx context.Context, record *deploymentRecord, opts *deployLookupOpts, stdout io.Writer) error {
	if record == nil || record.manifest == nil {
		return fmt.Errorf("deploy delete: deployment record is required")
	}
	manifest := record.manifest
	manifestPath := record.manifestPath
	unitRemoved := false
	serviceName := ""
	if manifest.Systemd != nil {
		if runtime.GOOS != "linux" {
			return fmt.Errorf("deploy delete: systemd deployment is only supported on Linux")
		}
		serviceName = manifest.Systemd.ServiceName + ".service"
	}
	if opts.dryRun {
		if serviceName != "" {
			_, _ = fmt.Fprintf(stdout, "dry-run: would disable and stop %s\n", serviceName)
			_, _ = fmt.Fprintf(stdout, "dry-run: would remove systemd unit %s\n", manifest.Systemd.UnitPath)
		}
		_, _ = fmt.Fprintf(stdout, "dry-run: would delete deployment manifest %s\n", manifestPath)
		_, _ = fmt.Fprintf(stdout, "dry-run: would leave binary in place at %s\n", manifest.BinaryPath)
		return nil
	}
	ok, err := confirmDestructiveAction(
		"deploy delete",
		opts.stdin,
		stdout,
		opts.interactive,
		opts.force,
		opts.dryRun,
		fmt.Sprintf("Delete deployment %q?", manifest.Name),
		[]string{
			fmt.Sprintf("delete deployment manifest %s", manifestPath),
			fmt.Sprintf("leave binary in place at %s", manifest.BinaryPath),
			func() string {
				if serviceName == "" {
					return ""
				}
				return fmt.Sprintf("disable and stop systemd service %s", serviceName)
			}(),
			func() string {
				if manifest.Systemd == nil || manifest.Systemd.UnitPath == "" {
					return ""
				}
				return fmt.Sprintf("remove systemd unit %s", manifest.Systemd.UnitPath)
			}(),
		},
	)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if manifest.Systemd != nil {
		if err := deployRunSystemctl(ctx, manifest.Scope, "disable", "--now", serviceName); err != nil {
			return fmt.Errorf("deploy delete: %w", err)
		}
		_ = deployRunSystemctl(ctx, manifest.Scope, "reset-failed", serviceName)
		if manifest.Systemd.UnitPath != "" {
			if err := os.Remove(manifest.Systemd.UnitPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("deploy delete: remove unit %s: %w", manifest.Systemd.UnitPath, err)
			}
			unitRemoved = true
		}
		if err := deployRunSystemctl(ctx, manifest.Scope, "daemon-reload"); err != nil {
			return fmt.Errorf("deploy delete: %w", err)
		}
	}

	if err := os.Remove(record.manifestPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("deploy delete: remove manifest %s: %w", record.manifestPath, err)
	}
	if _, err := fmt.Fprintf(stdout, "deleted deployment manifest %s\n", record.manifestPath); err != nil {
		return fmt.Errorf("deploy delete: write output: %w", err)
	}
	if unitRemoved {
		if _, err := fmt.Fprintf(stdout, "removed systemd unit %s\n", manifest.Systemd.UnitPath); err != nil {
			return fmt.Errorf("deploy delete: write output: %w", err)
		}
	}
	if _, err := fmt.Fprintf(stdout, "left binary in place at %s\n", manifest.BinaryPath); err != nil {
		return fmt.Errorf("deploy delete: write output: %w", err)
	}
	return nil
}

func runDeployUpgradeWithOpts(ctx context.Context, opts *deployUpgradeOpts, stdout io.Writer) error {
	if opts == nil {
		return fmt.Errorf("deploy upgrade: options are required")
	}
	version := strings.TrimSpace(opts.version)
	if version == "" {
		version = "latest"
	}
	repository := strings.TrimSpace(opts.repository)
	if repository == "" {
		repository = defaultUpgradeRepo
	}
	if opts.all {
		return runDeployAcrossScope(ctx, opts.scope, stdout, "deploy upgrade", func(ctx context.Context, record *deploymentRecord, stdout io.Writer) error {
			return runDeployUpgradeRecord(ctx, record, version, repository, stdout)
		})
	}
	record, err := loadDeploymentRecordForScope(opts.name, opts.scope)
	if err != nil {
		return fmt.Errorf("deploy upgrade: %w", err)
	}
	return runDeployUpgradeRecord(ctx, record, version, repository, stdout)
}

func runDeployUpgradeRecord(
	ctx context.Context,
	record *deploymentRecord,
	version string,
	repository string,
	stdout io.Writer,
) error {
	if record == nil || record.manifest == nil {
		return fmt.Errorf("deploy upgrade: deployment record is required")
	}
	manifest := record.manifest
	resolvedVersion, err := downloadReleaseBinary(ctx, repository, version, manifest.BinaryPath)
	if err != nil {
		return fmt.Errorf("deploy upgrade: %w", err)
	}
	manifest.InstalledVersion = resolvedVersion
	manifest.InstalledAt = deployNow().UTC()

	if manifest.Systemd != nil {
		if runtime.GOOS != "linux" {
			return fmt.Errorf("deploy upgrade: systemd deployment is only supported on Linux")
		}
		unit, err := renderSystemdUnit(manifest, record.scopePaths.wantedBy)
		if err != nil {
			return fmt.Errorf("deploy upgrade: render systemd unit: %w", err)
		}
		if err := writeTextFileAtomic(manifest.Systemd.UnitPath, unit, deploymentUnitFileMode(manifest)); err != nil {
			return fmt.Errorf("deploy upgrade: write systemd unit: %w", err)
		}
	}
	if err := writeDeploymentManifest(record.manifestPath, manifest); err != nil {
		return fmt.Errorf("deploy upgrade: write manifest: %w", err)
	}

	if _, err := fmt.Fprintf(stdout, "installed release %s to %s\n", resolvedVersion, manifest.BinaryPath); err != nil {
		return fmt.Errorf("deploy upgrade: write output: %w", err)
	}
	if manifest.Systemd == nil {
		return nil
	}
	if err := syncSystemdDeploymentState(ctx, "deploy upgrade", manifest, stdout); err != nil {
		return err
	}
	return nil
}

func syncSystemdDeploymentState(ctx context.Context, op string, manifest *deploymentManifest, stdout io.Writer) error {
	if manifest == nil || manifest.Systemd == nil {
		return nil
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("%s: systemd deployment is only supported on Linux", op)
	}
	if err := deployRunSystemctl(ctx, manifest.Scope, "daemon-reload"); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	serviceName := manifest.Systemd.ServiceName + ".service"
	if manifest.Systemd.Enabled {
		if err := deployRunSystemctl(ctx, manifest.Scope, "enable", serviceName); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
	}
	if !manifest.Systemd.Started {
		if _, err := fmt.Fprintf(stdout, "left %s stopped\n", serviceName); err != nil {
			return fmt.Errorf("%s: write output: %w", op, err)
		}
		return nil
	}
	if err := deployRunSystemctl(ctx, manifest.Scope, "restart", serviceName); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if _, err := fmt.Fprintf(stdout, "restarted %s\n", serviceName); err != nil {
		return fmt.Errorf("%s: write output: %w", op, err)
	}
	return nil
}

func validateDeployIdentity(name, scope string) (string, string, *deployScopePaths, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", nil, fmt.Errorf("--name is required")
	}
	if !deployNamePattern.MatchString(name) {
		return "", "", nil, fmt.Errorf("invalid deployment name %q", name)
	}
	normalizedScope, scopePaths, err := resolveDeployScopePaths(scope)
	if err != nil {
		return "", "", nil, err
	}
	return name, normalizedScope, scopePaths, nil
}

func resolveDeployScopePaths(scope string) (string, *deployScopePaths, error) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" {
		scope = defaultDeployScope
	}
	switch scope {
	case "user":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", nil, fmt.Errorf("resolve user home: %w", err)
		}
		configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
		if configHome == "" {
			configHome = filepath.Join(home, ".config")
		}
		return scope, &deployScopePaths{
			defaultBinaryPath: filepath.Join(home, ".local", "bin", "workbuddy"),
			manifestDir:       filepath.Join(configHome, "workbuddy", "deployments"),
			unitDir:           filepath.Join(configHome, "systemd", "user"),
			wantedBy:          "default.target",
		}, nil
	case "system":
		return scope, &deployScopePaths{
			defaultBinaryPath: deploySystemBinaryPath,
			manifestDir:       deploySystemManifestDir,
			unitDir:           deploySystemUnitDir,
			wantedBy:          "multi-user.target",
		}, nil
	default:
		return "", nil, fmt.Errorf("--scope must be one of user or system")
	}
}

func resolveBinaryPath(defaultPath, requested string) (string, error) {
	if strings.TrimSpace(requested) == "" {
		return defaultPath, nil
	}
	abs, err := filepath.Abs(requested)
	if err != nil {
		return "", fmt.Errorf("resolve binary path: %w", err)
	}
	return abs, nil
}

func resolveWorkingDirectory(requested string) (string, error) {
	if strings.TrimSpace(requested) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
		return cwd, nil
	}
	abs, err := filepath.Abs(requested)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	return abs, nil
}

func parseDeployEnv(entries []string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("--env values must be KEY=VALUE, got %q", entry)
		}
		if strings.ContainsAny(key, "\r\n") || strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("--env values may not contain newlines, got %q", entry)
		}
		env[key] = value
	}
	return env, nil
}

func normalizeDeployCommandArgs(args []string) ([]string, error) {
	trimmed := trimStringSlice(args)
	if len(trimmed) > 0 {
		base := filepath.Base(trimmed[0])
		if base == "workbuddy" {
			trimmed = trimmed[1:]
		}
	}
	if len(trimmed) == 0 {
		return []string{
			"serve",
			"--config-dir", ".github/workbuddy",
			"--db-path", ".workbuddy/workbuddy.db",
			"--max-parallel-tasks", strconv.Itoa(app.DefaultWorkerParallelism()),
		}, nil
	}
	for _, arg := range trimmed {
		if strings.ContainsRune(arg, '\n') {
			return nil, fmt.Errorf("command arguments may not contain newlines")
		}
	}
	return trimmed, nil
}

func trimStringSlice(values []string) []string {
	trimmed := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		trimmed = append(trimmed, value)
	}
	return trimmed
}

func defaultDeployDescription(name string, commandArgs []string, override string) string {
	if override != "" {
		return override
	}
	mode := "service"
	if len(commandArgs) > 0 {
		mode = commandArgs[0]
	}
	return fmt.Sprintf("Workbuddy %s (%s)", name, mode)
}

func deploymentManifestPath(dir, name string) string {
	return filepath.Join(dir, name+".json")
}

func (e *deploymentNotFoundError) Error() string {
	if e == nil {
		return "deployment not found"
	}
	installed := "none"
	if len(e.installed) > 0 {
		installed = strings.Join(e.installed, ", ")
	}
	return fmt.Sprintf("deployment %q not found in %s scope (installed: %s)", e.name, e.scope, installed)
}

func (e *deploymentNotFoundError) ExitCode() int {
	return int(ExitCodeNotFound)
}

func collectDeployListRows(scope string) ([]deployListRow, error) {
	records, err := loadDeploymentRecords(scope, defaultDeployListScope)
	if err != nil {
		return nil, err
	}
	rows := make([]deployListRow, 0, len(records))
	for _, record := range records {
		rows = append(rows, deployListRow{
			Name:       record.manifest.Name,
			Scope:      record.manifest.Scope,
			BinaryPath: record.manifest.BinaryPath,
			Command:    append([]string(nil), record.manifest.Command...),
		})
	}
	return rows, nil
}

func renderDeployCommand(args []string) string {
	if len(args) == 0 {
		return ""
	}
	rendered := make([]string, len(args))
	for i, arg := range args {
		if strings.ContainsAny(arg, " \t") {
			rendered[i] = strconv.Quote(arg)
			continue
		}
		rendered[i] = arg
	}
	return strings.Join(rendered, " ")
}

func loadDeploymentForScope(name, scope string) (*deploymentManifest, *deployScopePaths, string, error) {
	record, err := loadDeploymentRecordForScope(name, scope)
	if err != nil {
		return nil, nil, "", err
	}
	return record.manifest, record.scopePaths, record.manifestPath, nil
}

func loadDeploymentRecordForScope(name, scope string) (*deploymentRecord, error) {
	validatedName, normalizedScope, _, err := validateDeployIdentity(name, scope)
	if err != nil {
		return nil, err
	}
	records, err := loadDeploymentRecords(normalizedScope, defaultDeployScope)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.manifest.Name == validatedName {
			return record, nil
		}
	}
	installed := make([]string, 0, len(records))
	for _, record := range records {
		installed = append(installed, record.manifest.Name)
	}
	return nil, &deploymentNotFoundError{
		name:      validatedName,
		scope:     normalizedScope,
		installed: installed,
	}
}

func loadDeploymentRecords(scope, defaultScope string) ([]*deploymentRecord, error) {
	scopes, err := resolveDeployScopes(scope, defaultScope)
	if err != nil {
		return nil, err
	}
	records := make([]*deploymentRecord, 0)
	for _, resolvedScope := range scopes {
		normalizedScope, scopePaths, err := resolveDeployScopePaths(resolvedScope)
		if err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(scopePaths.manifestDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read manifest directory %s: %w", scopePaths.manifestDir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			manifestPath := filepath.Join(scopePaths.manifestDir, entry.Name())
			manifest, err := readDeploymentManifest(manifestPath)
			if err != nil {
				return nil, err
			}
			manifest.Scope = normalizedScope
			records = append(records, &deploymentRecord{
				manifest:     manifest,
				manifestPath: manifestPath,
				scopePaths:   scopePaths,
			})
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].manifest.Scope != records[j].manifest.Scope {
			return records[i].manifest.Scope < records[j].manifest.Scope
		}
		if records[i].manifest.Name != records[j].manifest.Name {
			return records[i].manifest.Name < records[j].manifest.Name
		}
		return records[i].manifestPath < records[j].manifestPath
	})
	return records, nil
}

func resolveDeployScopes(scope, defaultScope string) ([]string, error) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" {
		scope = defaultScope
	}
	switch scope {
	case "all":
		return []string{"system", "user"}, nil
	case "user", "system":
		return []string{scope}, nil
	default:
		return nil, fmt.Errorf("--scope must be one of all, user, or system")
	}
}

func runDeployAcrossScope(
	ctx context.Context,
	scope string,
	stdout io.Writer,
	op string,
	run func(context.Context, *deploymentRecord, io.Writer) error,
) error {
	records, err := loadDeploymentRecords(scope, defaultDeployScope)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	resolvedScope, _, err := resolveDeployScopePaths(scope)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if len(records) == 0 {
		return fmt.Errorf("%s: no deployments installed in %s scope", op, resolvedScope)
	}
	for _, record := range records {
		if err := run(ctx, record, stdout); err != nil {
			return err
		}
	}
	return nil
}

func readDeploymentManifest(path string) (*deploymentManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", path, err)
	}
	var manifest deploymentManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	if manifest.SchemaVersion == 0 {
		manifest.SchemaVersion = deploymentManifestVer
	}
	if manifest.SchemaVersion < 0 {
		return nil, fmt.Errorf("manifest %s has invalid schema_version %d", path, manifest.SchemaVersion)
	}
	if manifest.SchemaVersion > deploymentManifestVer {
		return nil, fmt.Errorf(
			"manifest %s uses unsupported schema_version %d (max supported %d)",
			path,
			manifest.SchemaVersion,
			deploymentManifestVer,
		)
	}
	if manifest.BinaryPath == "" {
		return nil, fmt.Errorf("manifest %s is missing binary_path", path)
	}
	if manifest.Name == "" {
		manifest.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if len(manifest.Command) == 0 {
		commandArgs, err := normalizeDeployCommandArgs(nil)
		if err != nil {
			return nil, err
		}
		manifest.Command = commandArgs
	}
	return &manifest, nil
}

func writeDeploymentManifest(path string, manifest *deploymentManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	data = append(data, '\n')
	return writeBytesAtomic(path, data, 0o600)
}

func deploymentUnitFileMode(manifest *deploymentManifest) os.FileMode {
	if manifest != nil && manifest.Systemd != nil && len(manifest.Systemd.Environment) > 0 {
		return 0o600
	}
	return 0o644
}

func renderSystemdUnit(manifest *deploymentManifest, wantedBy string) (string, error) {
	if manifest == nil || manifest.Systemd == nil {
		return "", fmt.Errorf("systemd settings are required")
	}
	if manifest.BinaryPath == "" {
		return "", fmt.Errorf("binary path is required")
	}
	if manifest.WorkingDirectory == "" {
		return "", fmt.Errorf("working directory is required")
	}
	execArgs := append([]string{manifest.BinaryPath}, manifest.Command...)
	for _, arg := range execArgs {
		if strings.ContainsRune(arg, '\n') {
			return "", fmt.Errorf("systemd command arguments may not contain newlines")
		}
	}

	var b strings.Builder
	b.WriteString("[Unit]\n")
	fmt.Fprintf(&b, "Description=%s\n", escapeUnitValue(manifest.Systemd.Description))
	afterUnits := append([]string{"network-online.target"}, trimStringSlice(manifest.Systemd.After)...)
	fmt.Fprintf(&b, "After=%s\n", strings.Join(afterUnits, " "))
	b.WriteString("Wants=network-online.target\n\n")

	unitType := strings.TrimSpace(manifest.Systemd.Type)
	if unitType == "" {
		unitType = "simple"
	}
	restart := strings.TrimSpace(manifest.Systemd.Restart)
	if restart == "" {
		restart = "on-failure"
	}

	b.WriteString("[Service]\n")
	fmt.Fprintf(&b, "Type=%s\n", unitType)
	fmt.Fprintf(&b, "WorkingDirectory=%s\n", systemdSingleValue(manifest.WorkingDirectory))
	fmt.Fprintf(&b, "ExecStart=%s\n", joinSystemdCommand(execArgs))
	fmt.Fprintf(&b, "Restart=%s\n", restart)
	b.WriteString("RestartSec=5s\n")
	if km := strings.TrimSpace(manifest.Systemd.KillMode); km != "" {
		fmt.Fprintf(&b, "KillMode=%s\n", km)
	}

	keys := make([]string, 0, len(manifest.Systemd.Environment))
	for key := range manifest.Systemd.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&b, "Environment=%s\n", systemdQuote(key+"="+manifest.Systemd.Environment[key]))
	}
	for _, envFile := range manifest.Systemd.EnvironmentFiles {
		fmt.Fprintf(&b, "EnvironmentFile=%s\n", systemdSingleValue(envFile))
	}

	b.WriteString("\n[Install]\n")
	fmt.Fprintf(&b, "WantedBy=%s\n", wantedBy)
	return b.String(), nil
}

func escapeUnitValue(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.ReplaceAll(value, "%", "%%")
}

func joinSystemdCommand(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = systemdQuote(arg)
	}
	return strings.Join(quoted, " ")
}

func systemdQuote(value string) string {
	value = strings.ReplaceAll(value, "%", "%%")
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}

// systemdSingleValue renders a value for systemd settings that take a single
// unstructured token (e.g. WorkingDirectory=, EnvironmentFile=). systemd's
// parser does not strip outer double quotes for these, so we only escape the
// `%` specifier and collapse newlines instead of wrapping in quotes.
func systemdSingleValue(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.ReplaceAll(value, "%", "%%")
}

func writeTextFileAtomic(path, contents string, mode os.FileMode) error {
	return writeBytesAtomic(path, []byte(contents), mode)
}

func writeBytesAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".workbuddy-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func copyFileAtomic(src, dst string, mode os.FileMode) error {
	input, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source %s: %w", src, err)
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".workbuddy-binary-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := io.Copy(tmp, input); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy file contents: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return fmt.Errorf("replace %s: %w", dst, err)
	}
	return nil
}

func runSystemctl(ctx context.Context, scope string, args ...string) error {
	scope = strings.ToLower(strings.TrimSpace(scope))
	commandArgs := append([]string(nil), args...)
	if scope == "user" {
		commandArgs = append([]string{"--user"}, commandArgs...)
	}
	cmd := exec.CommandContext(ctx, "systemctl", commandArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return fmt.Errorf("systemctl %s: %w", strings.Join(commandArgs, " "), err)
		}
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(commandArgs, " "), err, message)
	}
	return nil
}

func deployedVersionLabel() string {
	if strings.TrimSpace(Version) == "" {
		return "unknown"
	}
	return strings.TrimSpace(Version)
}

func downloadReleaseBinary(ctx context.Context, repository, version, dst string) (string, error) {
	resolvedVersion, err := resolveReleaseVersion(ctx, repository, version)
	if err != nil {
		return "", err
	}
	assetURL := fmt.Sprintf("%s/%s/releases/download/v%s/%s", strings.TrimRight(deployGitHubDownloadBase, "/"), repository, resolvedVersion, releaseArchiveName(resolvedVersion))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return "", fmt.Errorf("build release download request: %w", err)
	}
	applyGitHubAuth(request)
	response, err := deployHTTPClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("download release archive: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download release archive: unexpected status %s", response.Status)
	}
	if err := installBinaryFromArchive(response.Body, dst, 0o755); err != nil {
		return "", err
	}
	return resolvedVersion, nil
}

func resolveReleaseVersion(ctx context.Context, repository, version string) (string, error) {
	version = strings.TrimSpace(version)
	if version == "" || version == "latest" {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(deployGitHubAPIBaseURL, "/")+"/repos/"+repository+"/releases/latest", nil)
		if err != nil {
			return "", fmt.Errorf("build latest-release request: %w", err)
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		applyGitHubAuth(request)
		response, err := deployHTTPClient.Do(request)
		if err != nil {
			return "", fmt.Errorf("resolve latest release: %w", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return "", fmt.Errorf("resolve latest release: unexpected status %s", response.Status)
		}
		var payload struct {
			TagName string `json:"tag_name"`
		}
		if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
			return "", fmt.Errorf("decode latest release: %w", err)
		}
		if payload.TagName == "" {
			return "", fmt.Errorf("resolve latest release: tag_name missing")
		}
		return strings.TrimPrefix(payload.TagName, "v"), nil
	}
	return strings.TrimPrefix(version, "v"), nil
}

func releaseArchiveName(version string) string {
	return fmt.Sprintf("workbuddy_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
}

func installBinaryFromArchive(reader io.Reader, dst string, mode os.FileMode) error {
	gzipReader, err := gzip.NewReader(reader)
	if err != nil {
		return fmt.Errorf("read release archive: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read release archive: %w", err)
		}
		if header == nil || header.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(header.Name) != "workbuddy" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("create parent directory: %w", err)
		}
		tmp, err := os.CreateTemp(filepath.Dir(dst), ".workbuddy-upgrade-*")
		if err != nil {
			return fmt.Errorf("create temp file: %w", err)
		}
		tmpPath := tmp.Name()
		defer os.Remove(tmpPath)
		if _, err := io.Copy(tmp, tarReader); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("extract workbuddy binary: %w", err)
		}
		if err := tmp.Chmod(mode); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("chmod extracted binary: %w", err)
		}
		if err := tmp.Close(); err != nil {
			return fmt.Errorf("close extracted binary: %w", err)
		}
		if err := os.Rename(tmpPath, dst); err != nil {
			return fmt.Errorf("replace %s: %w", dst, err)
		}
		return nil
	}
	return fmt.Errorf("release archive did not contain a workbuddy binary")
}

func applyGitHubAuth(request *http.Request) {
	if token := findDeployGitHubToken(); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
}

func findDeployGitHubToken() string {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GITHUB_OAUTH"} {
		if token := strings.TrimSpace(os.Getenv(key)); token != "" {
			return token
		}
	}
	return ""
}
