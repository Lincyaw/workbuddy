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
	"time"

	"github.com/spf13/cobra"
)

const (
	defaultDeployName     = "workbuddy"
	defaultDeployScope    = "user"
	defaultUpgradeRepo    = "Lincyaw/workbuddy"
	deploymentManifestVer = 1
)

var (
	deployExecutablePath     = os.Executable
	deployNow                = time.Now
	deployRunSystemctl       = runSystemctl
	deployHTTPClient         = http.DefaultClient
	deployGitHubAPIBaseURL   = "https://api.github.com"
	deployGitHubDownloadBase = "https://github.com"
	deployNamePattern        = regexp.MustCompile(`^[A-Za-z0-9@_.-]+$`)
)

type deployInstallOpts struct {
	name             string
	scope            string
	binaryPath       string
	workingDirectory string
	systemd          bool
	description      string
	env              []string
	envFiles         []string
	enable           bool
	start            bool
	commandArgs      []string
}

type deployLookupOpts struct {
	name  string
	scope string
}

type deployUpgradeOpts struct {
	deployLookupOpts
	version    string
	repository string
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
}

type deployScopePaths struct {
	defaultBinaryPath string
	manifestDir       string
	unitDir           string
	wantedBy          string
}

var deployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Install and manage deployed workbuddy services",
	Long:  "Install the current workbuddy binary, optionally wire it into systemd, and keep enough deployment state to support later redeploy and upgrade operations.",
}

var deployInstallCmd = &cobra.Command{
	Use:   "install [-- workbuddy args...]",
	Short: "Install the current binary and optionally create a systemd unit",
	Args:  cobra.ArbitraryArgs,
	RunE:  runDeployInstallCmd,
}

var deployRedeployCmd = &cobra.Command{
	Use:   "redeploy",
	Short: "Reinstall the current binary and restart the recorded deployment",
	RunE:  runDeployRedeployCmd,
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

	deployRedeployCmd.Flags().String("name", defaultDeployName, "Deployment name")
	deployRedeployCmd.Flags().String("scope", defaultDeployScope, "Deployment scope: user or system")

	deployUpgradeCmd.Flags().String("name", defaultDeployName, "Deployment name")
	deployUpgradeCmd.Flags().String("scope", defaultDeployScope, "Deployment scope: user or system")
	deployUpgradeCmd.Flags().String("version", "latest", "Release version to install (for example latest or v0.2.0)")
	deployUpgradeCmd.Flags().String("repository", defaultUpgradeRepo, "GitHub repository used for release upgrades in OWNER/NAME form")

	deployCmd.AddCommand(deployInstallCmd, deployRedeployCmd, deployUpgradeCmd)
	rootCmd.AddCommand(deployCmd)
}

func runDeployInstallCmd(cmd *cobra.Command, args []string) error {
	opts, err := parseDeployInstallFlags(cmd, args)
	if err != nil {
		return err
	}
	return runDeployInstallWithOpts(cmd.Context(), opts, cmd.OutOrStdout())
}

func runDeployRedeployCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseDeployLookupFlags(cmd)
	if err != nil {
		return err
	}
	return runDeployRedeployWithOpts(cmd.Context(), opts, cmd.OutOrStdout())
}

func runDeployUpgradeCmd(cmd *cobra.Command, _ []string) error {
	lookup, err := parseDeployLookupFlags(cmd)
	if err != nil {
		return err
	}
	version, _ := cmd.Flags().GetString("version")
	repository, _ := cmd.Flags().GetString("repository")
	return runDeployUpgradeWithOpts(cmd.Context(), &deployUpgradeOpts{
		deployLookupOpts: *lookup,
		version:          strings.TrimSpace(version),
		repository:       strings.TrimSpace(repository),
	}, cmd.OutOrStdout())
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

	return &deployInstallOpts{
		name:             strings.TrimSpace(name),
		scope:            strings.TrimSpace(scope),
		binaryPath:       strings.TrimSpace(binaryPath),
		workingDirectory: strings.TrimSpace(workingDir),
		systemd:          systemd,
		description:      strings.TrimSpace(description),
		env:              envVars,
		envFiles:         trimStringSlice(envFiles),
		enable:           enable,
		start:            start,
		commandArgs:      append([]string(nil), args...),
	}, nil
}

func parseDeployLookupFlags(cmd *cobra.Command) (*deployLookupOpts, error) {
	name, _ := cmd.Flags().GetString("name")
	scope, _ := cmd.Flags().GetString("scope")
	return &deployLookupOpts{
		name:  strings.TrimSpace(name),
		scope: strings.TrimSpace(scope),
	}, nil
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
		if err := writeTextFileAtomic(manifest.Systemd.UnitPath, unit, 0o644); err != nil {
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
	return nil
}

func runDeployRedeployWithOpts(ctx context.Context, opts *deployLookupOpts, stdout io.Writer) error {
	if opts == nil {
		return fmt.Errorf("deploy redeploy: options are required")
	}
	manifest, scopePaths, manifestPath, err := loadDeploymentForScope(opts.name, opts.scope)
	if err != nil {
		return fmt.Errorf("deploy redeploy: %w", err)
	}

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
		unit, err := renderSystemdUnit(manifest, scopePaths.wantedBy)
		if err != nil {
			return fmt.Errorf("deploy redeploy: render systemd unit: %w", err)
		}
		if err := writeTextFileAtomic(manifest.Systemd.UnitPath, unit, 0o644); err != nil {
			return fmt.Errorf("deploy redeploy: write systemd unit: %w", err)
		}
	}
	if err := writeDeploymentManifest(manifestPath, manifest); err != nil {
		return fmt.Errorf("deploy redeploy: write manifest: %w", err)
	}

	if _, err := fmt.Fprintf(stdout, "reinstalled binary to %s\n", manifest.BinaryPath); err != nil {
		return fmt.Errorf("deploy redeploy: write output: %w", err)
	}
	if manifest.Systemd == nil {
		return nil
	}
	if err := deployRunSystemctl(ctx, manifest.Scope, "daemon-reload"); err != nil {
		return fmt.Errorf("deploy redeploy: %w", err)
	}
	serviceName := manifest.Systemd.ServiceName + ".service"
	if manifest.Systemd.Enabled {
		if err := deployRunSystemctl(ctx, manifest.Scope, "enable", serviceName); err != nil {
			return fmt.Errorf("deploy redeploy: %w", err)
		}
	}
	if err := deployRunSystemctl(ctx, manifest.Scope, "restart", serviceName); err != nil {
		return fmt.Errorf("deploy redeploy: %w", err)
	}
	if _, err := fmt.Fprintf(stdout, "restarted %s\n", serviceName); err != nil {
		return fmt.Errorf("deploy redeploy: write output: %w", err)
	}
	return nil
}

func runDeployUpgradeWithOpts(ctx context.Context, opts *deployUpgradeOpts, stdout io.Writer) error {
	if opts == nil {
		return fmt.Errorf("deploy upgrade: options are required")
	}
	manifest, scopePaths, manifestPath, err := loadDeploymentForScope(opts.name, opts.scope)
	if err != nil {
		return fmt.Errorf("deploy upgrade: %w", err)
	}
	version := strings.TrimSpace(opts.version)
	if version == "" {
		version = "latest"
	}
	repository := strings.TrimSpace(opts.repository)
	if repository == "" {
		repository = defaultUpgradeRepo
	}
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
		unit, err := renderSystemdUnit(manifest, scopePaths.wantedBy)
		if err != nil {
			return fmt.Errorf("deploy upgrade: render systemd unit: %w", err)
		}
		if err := writeTextFileAtomic(manifest.Systemd.UnitPath, unit, 0o644); err != nil {
			return fmt.Errorf("deploy upgrade: write systemd unit: %w", err)
		}
	}
	if err := writeDeploymentManifest(manifestPath, manifest); err != nil {
		return fmt.Errorf("deploy upgrade: write manifest: %w", err)
	}

	if _, err := fmt.Fprintf(stdout, "installed release %s to %s\n", resolvedVersion, manifest.BinaryPath); err != nil {
		return fmt.Errorf("deploy upgrade: write output: %w", err)
	}
	if manifest.Systemd == nil {
		return nil
	}
	if err := deployRunSystemctl(ctx, manifest.Scope, "daemon-reload"); err != nil {
		return fmt.Errorf("deploy upgrade: %w", err)
	}
	serviceName := manifest.Systemd.ServiceName + ".service"
	if manifest.Systemd.Enabled {
		if err := deployRunSystemctl(ctx, manifest.Scope, "enable", serviceName); err != nil {
			return fmt.Errorf("deploy upgrade: %w", err)
		}
	}
	if err := deployRunSystemctl(ctx, manifest.Scope, "restart", serviceName); err != nil {
		return fmt.Errorf("deploy upgrade: %w", err)
	}
	if _, err := fmt.Fprintf(stdout, "restarted %s\n", serviceName); err != nil {
		return fmt.Errorf("deploy upgrade: write output: %w", err)
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
			defaultBinaryPath: "/usr/local/bin/workbuddy",
			manifestDir:       "/etc/workbuddy/deployments",
			unitDir:           "/etc/systemd/system",
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
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("--env values must be KEY=VALUE, got %q", entry)
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
			"--max-parallel-tasks", strconv.Itoa(defaultEmbeddedWorkerParallelism()),
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

func loadDeploymentForScope(name, scope string) (*deploymentManifest, *deployScopePaths, string, error) {
	validatedName, normalizedScope, scopePaths, err := validateDeployIdentity(name, scope)
	if err != nil {
		return nil, nil, "", err
	}
	manifestPath := deploymentManifestPath(scopePaths.manifestDir, validatedName)
	manifest, err := readDeploymentManifest(manifestPath)
	if err != nil {
		return nil, nil, "", err
	}
	manifest.Scope = normalizedScope
	if manifest.Name == "" {
		manifest.Name = validatedName
	}
	return manifest, scopePaths, manifestPath, nil
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
	return writeBytesAtomic(path, data, 0o644)
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
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n\n")

	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	fmt.Fprintf(&b, "WorkingDirectory=%s\n", systemdQuote(manifest.WorkingDirectory))
	fmt.Fprintf(&b, "ExecStart=%s\n", joinSystemdCommand(execArgs))
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=5s\n")

	keys := make([]string, 0, len(manifest.Systemd.Environment))
	for key := range manifest.Systemd.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&b, "Environment=%s\n", systemdQuote(key+"="+manifest.Systemd.Environment[key]))
	}
	for _, envFile := range manifest.Systemd.EnvironmentFiles {
		fmt.Fprintf(&b, "EnvironmentFile=%s\n", systemdQuote(envFile))
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
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
}
