package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestRunDeployInstallWithSystemdWritesManifestAndUnit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}

	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Chdir(repoDir)

	sourceBinary := filepath.Join(tempDir, "current-workbuddy")
	if err := os.WriteFile(sourceBinary, []byte("binary-v1"), 0o755); err != nil {
		t.Fatalf("write source binary: %v", err)
	}

	restore := overrideDeployGlobals(t, sourceBinary)
	defer restore()

	var systemctlCalls []string
	deployRunSystemctl = func(_ context.Context, scope string, args ...string) error {
		systemctlCalls = append(systemctlCalls, scope+":"+strings.Join(args, " "))
		return nil
	}
	deployNow = func() time.Time {
		return time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)
	}

	var stdout bytes.Buffer
	err := runDeployInstallWithOpts(context.Background(), &deployInstallOpts{
		name:        "demo",
		scope:       "user",
		systemd:     true,
		enable:      true,
		start:       true,
		env:         []string{"FOO=bar baz"},
		envFiles:    []string{"/etc/workbuddy/demo.env"},
		commandArgs: nil,
	}, &stdout)
	if err != nil {
		t.Fatalf("runDeployInstallWithOpts: %v", err)
	}

	installedBinary := filepath.Join(homeDir, ".local", "bin", "workbuddy")
	content, err := os.ReadFile(installedBinary)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if got, want := string(content), "binary-v1"; got != want {
		t.Fatalf("installed binary content = %q, want %q", got, want)
	}

	manifestPath := filepath.Join(configDir, "workbuddy", "deployments", "demo.json")
	manifest := mustReadManifest(t, manifestPath)
	if got, want := manifest.Scope, "user"; got != want {
		t.Fatalf("manifest scope = %q, want %q", got, want)
	}
	if got, want := manifest.Command[0], "serve"; got != want {
		t.Fatalf("manifest command[0] = %q, want %q", got, want)
	}
	if manifest.Systemd == nil {
		t.Fatal("expected systemd settings in manifest")
	}
	if got, want := manifest.Systemd.Environment["FOO"], "bar baz"; got != want {
		t.Fatalf("manifest env FOO = %q, want %q", got, want)
	}
	if got, want := fileMode(t, manifestPath), os.FileMode(0o600); got != want {
		t.Fatalf("manifest mode = %#o, want %#o", got, want)
	}

	unitPath := filepath.Join(configDir, "systemd", "user", "demo.service")
	unitBytes, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read unit file: %v", err)
	}
	unit := string(unitBytes)
	if !strings.Contains(unit, `ExecStart="`+installedBinary+`" "serve"`) {
		t.Fatalf("unit file missing ExecStart for installed binary:\n%s", unit)
	}
	if !strings.Contains(unit, `Environment="FOO=bar baz"`) {
		t.Fatalf("unit file missing environment line:\n%s", unit)
	}
	if !strings.Contains(unit, "EnvironmentFile=/etc/workbuddy/demo.env\n") {
		t.Fatalf("unit file missing environment file line:\n%s", unit)
	}
	if !strings.Contains(unit, "WorkingDirectory="+repoDir+"\n") {
		t.Fatalf("unit file missing unquoted WorkingDirectory line:\n%s", unit)
	}
	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Fatalf("unit file missing user WantedBy:\n%s", unit)
	}
	if got, want := fileMode(t, unitPath), os.FileMode(0o600); got != want {
		t.Fatalf("unit mode = %#o, want %#o", got, want)
	}

	gotCalls := strings.Join(systemctlCalls, " | ")
	wantCalls := "user:daemon-reload | user:enable demo.service | user:start demo.service"
	if gotCalls != wantCalls {
		t.Fatalf("systemctl calls = %q, want %q", gotCalls, wantCalls)
	}
	if !strings.Contains(stdout.String(), "wrote deployment manifest ") {
		t.Fatalf("stdout missing manifest message: %q", stdout.String())
	}
}

func TestRunDeployRedeployReinstallsBinaryAndRestartsSystemd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}

	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	deployedBinary := filepath.Join(homeDir, ".local", "bin", "workbuddy")
	if err := os.MkdirAll(filepath.Dir(deployedBinary), 0o755); err != nil {
		t.Fatalf("mkdir binary dir: %v", err)
	}
	if err := os.WriteFile(deployedBinary, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("write deployed binary: %v", err)
	}

	unitPath := filepath.Join(configDir, "systemd", "user", "demo.service")
	manifestPath := filepath.Join(configDir, "workbuddy", "deployments", "demo.json")
	manifest := &deploymentManifest{
		SchemaVersion:    deploymentManifestVer,
		Name:             "demo",
		Scope:            "user",
		BinaryPath:       deployedBinary,
		WorkingDirectory: repoDir,
		Command:          []string{"coordinator", "--db", ".workbuddy/workbuddy.db"},
		InstalledVersion: "old",
		InstalledAt:      time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC),
		Systemd: &deploymentSystemd{
			ServiceName: "demo",
			UnitPath:    unitPath,
			Description: "Workbuddy demo (coordinator)",
			Enabled:     true,
			Started:     true,
		},
	}
	if err := writeDeploymentManifest(manifestPath, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	sourceBinary := filepath.Join(tempDir, "current-workbuddy")
	if err := os.WriteFile(sourceBinary, []byte("new-binary"), 0o755); err != nil {
		t.Fatalf("write source binary: %v", err)
	}

	restore := overrideDeployGlobals(t, sourceBinary)
	defer restore()

	var systemctlCalls []string
	deployRunSystemctl = func(_ context.Context, scope string, args ...string) error {
		systemctlCalls = append(systemctlCalls, scope+":"+strings.Join(args, " "))
		return nil
	}
	deployNow = func() time.Time {
		return time.Date(2026, 4, 18, 13, 0, 0, 0, time.UTC)
	}

	var stdout bytes.Buffer
	err := runDeployRedeployWithOpts(context.Background(), &deployLookupOpts{name: "demo", scope: "user"}, &stdout)
	if err != nil {
		t.Fatalf("runDeployRedeployWithOpts: %v", err)
	}

	content, err := os.ReadFile(deployedBinary)
	if err != nil {
		t.Fatalf("read deployed binary: %v", err)
	}
	if got, want := string(content), "new-binary"; got != want {
		t.Fatalf("deployed binary content = %q, want %q", got, want)
	}

	unitBytes, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read unit file: %v", err)
	}
	unit := string(unitBytes)
	if !strings.Contains(unit, `ExecStart="`+deployedBinary+`" "coordinator" "--db" ".workbuddy/workbuddy.db"`) {
		t.Fatalf("unit file missing coordinator command:\n%s", unit)
	}

	updated := mustReadManifest(t, manifestPath)
	if got, want := updated.InstalledVersion, deployedVersionLabel(); got != want {
		t.Fatalf("updated manifest version = %q, want %q", got, want)
	}

	gotCalls := strings.Join(systemctlCalls, " | ")
	wantCalls := "user:daemon-reload | user:enable demo.service | user:restart demo.service"
	if gotCalls != wantCalls {
		t.Fatalf("systemctl calls = %q, want %q", gotCalls, wantCalls)
	}
	if !strings.Contains(stdout.String(), "restarted demo.service") {
		t.Fatalf("stdout missing restart message: %q", stdout.String())
	}
}

func TestRunDeployRedeployKeepsStoppedServiceStopped(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}

	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)

	deployedBinary := filepath.Join(homeDir, ".local", "bin", "workbuddy")
	if err := os.MkdirAll(filepath.Dir(deployedBinary), 0o755); err != nil {
		t.Fatalf("mkdir binary dir: %v", err)
	}
	if err := os.WriteFile(deployedBinary, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("write deployed binary: %v", err)
	}

	unitPath := filepath.Join(configDir, "systemd", "user", "demo.service")
	manifestPath := filepath.Join(configDir, "workbuddy", "deployments", "demo.json")
	manifest := &deploymentManifest{
		SchemaVersion:    deploymentManifestVer,
		Name:             "demo",
		Scope:            "user",
		BinaryPath:       deployedBinary,
		WorkingDirectory: repoDir,
		Command:          []string{"serve"},
		InstalledVersion: "old",
		InstalledAt:      time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC),
		Systemd: &deploymentSystemd{
			ServiceName: "demo",
			UnitPath:    unitPath,
			Description: "Workbuddy demo (serve)",
			Enabled:     true,
			Started:     false,
		},
	}
	if err := writeDeploymentManifest(manifestPath, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	sourceBinary := filepath.Join(tempDir, "current-workbuddy")
	if err := os.WriteFile(sourceBinary, []byte("new-binary"), 0o755); err != nil {
		t.Fatalf("write source binary: %v", err)
	}

	restore := overrideDeployGlobals(t, sourceBinary)
	defer restore()

	var systemctlCalls []string
	deployRunSystemctl = func(_ context.Context, scope string, args ...string) error {
		systemctlCalls = append(systemctlCalls, scope+":"+strings.Join(args, " "))
		return nil
	}

	var stdout bytes.Buffer
	err := runDeployRedeployWithOpts(context.Background(), &deployLookupOpts{name: "demo", scope: "user"}, &stdout)
	if err != nil {
		t.Fatalf("runDeployRedeployWithOpts: %v", err)
	}

	gotCalls := strings.Join(systemctlCalls, " | ")
	wantCalls := "user:daemon-reload | user:enable demo.service"
	if gotCalls != wantCalls {
		t.Fatalf("systemctl calls = %q, want %q", gotCalls, wantCalls)
	}
	if !strings.Contains(stdout.String(), "left demo.service stopped") {
		t.Fatalf("stdout missing stopped message: %q", stdout.String())
	}
}

func TestRunDeployStopStopsSystemdAndUpdatesManifest(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}

	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)

	deployedBinary := filepath.Join(homeDir, ".local", "bin", "workbuddy")
	if err := os.MkdirAll(filepath.Dir(deployedBinary), 0o755); err != nil {
		t.Fatalf("mkdir binary dir: %v", err)
	}
	if err := os.WriteFile(deployedBinary, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write deployed binary: %v", err)
	}

	unitPath := filepath.Join(configDir, "systemd", "user", "demo.service")
	manifestPath := filepath.Join(configDir, "workbuddy", "deployments", "demo.json")
	manifest := &deploymentManifest{
		SchemaVersion:    deploymentManifestVer,
		Name:             "demo",
		Scope:            "user",
		BinaryPath:       deployedBinary,
		WorkingDirectory: repoDir,
		Command:          []string{"serve"},
		InstalledVersion: "old",
		InstalledAt:      time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC),
		Systemd: &deploymentSystemd{
			ServiceName: "demo",
			UnitPath:    unitPath,
			Description: "Workbuddy demo (serve)",
			Enabled:     true,
			Started:     true,
		},
	}
	if err := writeDeploymentManifest(manifestPath, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	var systemctlCalls []string
	deployRunSystemctl = func(_ context.Context, scope string, args ...string) error {
		systemctlCalls = append(systemctlCalls, scope+":"+strings.Join(args, " "))
		return nil
	}

	var stdout bytes.Buffer
	err := runDeployStopWithOpts(context.Background(), &deployLookupOpts{name: "demo", scope: "user", force: true}, &stdout)
	if err != nil {
		t.Fatalf("runDeployStopWithOpts: %v", err)
	}

	gotCalls := strings.Join(systemctlCalls, " | ")
	wantCalls := "user:stop demo.service | user:reset-failed demo.service"
	if gotCalls != wantCalls {
		t.Fatalf("systemctl calls = %q, want %q", gotCalls, wantCalls)
	}
	if !strings.Contains(stdout.String(), "stopped demo.service") {
		t.Fatalf("stdout missing stop message: %q", stdout.String())
	}

	updated := mustReadManifest(t, manifestPath)
	if updated.Systemd == nil || updated.Systemd.Started {
		t.Fatalf("updated manifest systemd.started = %#v, want false", updated.Systemd)
	}
}

func TestRunDeployStopForceBypassesTTYConfirmation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}

	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)

	deployedBinary := filepath.Join(homeDir, ".local", "bin", "workbuddy")
	if err := os.MkdirAll(filepath.Dir(deployedBinary), 0o755); err != nil {
		t.Fatalf("mkdir binary dir: %v", err)
	}
	if err := os.WriteFile(deployedBinary, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write deployed binary: %v", err)
	}

	unitPath := filepath.Join(configDir, "systemd", "user", "demo.service")
	manifestPath := filepath.Join(configDir, "workbuddy", "deployments", "demo.json")
	manifest := &deploymentManifest{
		SchemaVersion:    deploymentManifestVer,
		Name:             "demo",
		Scope:            "user",
		BinaryPath:       deployedBinary,
		WorkingDirectory: repoDir,
		Command:          []string{"serve"},
		Systemd: &deploymentSystemd{
			ServiceName: "demo",
			UnitPath:    unitPath,
			Description: "Workbuddy demo (serve)",
			Enabled:     true,
			Started:     true,
		},
	}
	if err := writeDeploymentManifest(manifestPath, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	var systemctlCalls []string
	deployRunSystemctl = func(_ context.Context, scope string, args ...string) error {
		systemctlCalls = append(systemctlCalls, scope+":"+strings.Join(args, " "))
		return nil
	}

	restoreTTY := overrideCommandIsInteractiveTerminal(t, true)
	defer restoreTTY()

	cmd := &cobra.Command{}
	cmd.Flags().String("name", "", "")
	cmd.Flags().String("scope", "", "")
	cmd.Flags().Bool("force", false, "")
	cmd.Flags().Bool("dry-run", false, "")
	if err := cmd.Flags().Set("name", "demo"); err != nil {
		t.Fatalf("set name: %v", err)
	}
	if err := cmd.Flags().Set("scope", "user"); err != nil {
		t.Fatalf("set scope: %v", err)
	}
	if err := cmd.Flags().Set("force", "true"); err != nil {
		t.Fatalf("set force: %v", err)
	}
	cmd.SetIn(strings.NewReader("no\n"))

	opts, err := parseDeployLookupFlags(cmd)
	if err != nil {
		t.Fatalf("parseDeployLookupFlags: %v", err)
	}
	if !opts.interactive {
		t.Fatal("expected mocked interactive terminal")
	}

	var stdout bytes.Buffer
	err = runDeployStopWithOpts(context.Background(), opts, &stdout)
	if err != nil {
		t.Fatalf("runDeployStopWithOpts: %v", err)
	}
	if strings.Contains(stdout.String(), "Type 'yes' to continue:") {
		t.Fatalf("unexpected confirmation prompt with --force: %q", stdout.String())
	}

	gotCalls := strings.Join(systemctlCalls, " | ")
	wantCalls := "user:stop demo.service | user:reset-failed demo.service"
	if gotCalls != wantCalls {
		t.Fatalf("systemctl calls = %q, want %q", gotCalls, wantCalls)
	}
}

func TestRunDeployStopRejectsNonSystemdDeployment(t *testing.T) {
	tempDir := t.TempDir()
	configDir := filepath.Join(tempDir, "xdg")
	manifestPath := filepath.Join(configDir, "workbuddy", "deployments", "demo.json")
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("HOME", filepath.Join(tempDir, "home"))

	manifest := &deploymentManifest{
		SchemaVersion:    deploymentManifestVer,
		Name:             "demo",
		Scope:            "user",
		BinaryPath:       "/tmp/workbuddy",
		WorkingDirectory: tempDir,
		Command:          []string{"serve"},
	}
	if err := writeDeploymentManifest(manifestPath, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	var stdout bytes.Buffer
	err := runDeployStopWithOpts(context.Background(), &deployLookupOpts{name: "demo", scope: "user", force: true}, &stdout)
	if err == nil {
		t.Fatal("expected non-systemd deployment stop to fail")
	}
	if !strings.Contains(err.Error(), "is not managed by systemd") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunDeployStartStartsSystemdAndUpdatesManifest(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}

	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)

	deployedBinary := filepath.Join(homeDir, ".local", "bin", "workbuddy")
	if err := os.MkdirAll(filepath.Dir(deployedBinary), 0o755); err != nil {
		t.Fatalf("mkdir binary dir: %v", err)
	}
	if err := os.WriteFile(deployedBinary, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write deployed binary: %v", err)
	}

	unitPath := filepath.Join(configDir, "systemd", "user", "demo.service")
	manifestPath := filepath.Join(configDir, "workbuddy", "deployments", "demo.json")
	manifest := &deploymentManifest{
		SchemaVersion:    deploymentManifestVer,
		Name:             "demo",
		Scope:            "user",
		BinaryPath:       deployedBinary,
		WorkingDirectory: repoDir,
		Command:          []string{"serve"},
		Systemd: &deploymentSystemd{
			ServiceName: "demo",
			UnitPath:    unitPath,
			Description: "Workbuddy demo (serve)",
			Enabled:     false,
			Started:     false,
		},
	}
	if err := writeDeploymentManifest(manifestPath, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	var systemctlCalls []string
	deployRunSystemctl = func(_ context.Context, scope string, args ...string) error {
		systemctlCalls = append(systemctlCalls, scope+":"+strings.Join(args, " "))
		return nil
	}

	var stdout bytes.Buffer
	err := runDeployStartWithOpts(context.Background(), &deployLookupOpts{name: "demo", scope: "user"}, &stdout)
	if err != nil {
		t.Fatalf("runDeployStartWithOpts: %v", err)
	}

	gotCalls := strings.Join(systemctlCalls, " | ")
	wantCalls := "user:daemon-reload | user:start demo.service"
	if gotCalls != wantCalls {
		t.Fatalf("systemctl calls = %q, want %q", gotCalls, wantCalls)
	}
	if !strings.Contains(stdout.String(), "started demo.service") {
		t.Fatalf("stdout missing start message: %q", stdout.String())
	}

	updated := mustReadManifest(t, manifestPath)
	if updated.Systemd == nil || !updated.Systemd.Started {
		t.Fatalf("updated manifest systemd.started = %#v, want true", updated.Systemd)
	}
}

func TestRunDeployStartRejectsNonSystemdDeployment(t *testing.T) {
	tempDir := t.TempDir()
	configDir := filepath.Join(tempDir, "xdg")
	manifestPath := filepath.Join(configDir, "workbuddy", "deployments", "demo.json")
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("HOME", filepath.Join(tempDir, "home"))

	manifest := &deploymentManifest{
		SchemaVersion:    deploymentManifestVer,
		Name:             "demo",
		Scope:            "user",
		BinaryPath:       "/tmp/workbuddy",
		WorkingDirectory: tempDir,
		Command:          []string{"serve"},
	}
	if err := writeDeploymentManifest(manifestPath, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	var stdout bytes.Buffer
	err := runDeployStartWithOpts(context.Background(), &deployLookupOpts{name: "demo", scope: "user"}, &stdout)
	if err == nil {
		t.Fatal("expected non-systemd deployment start to fail")
	}
	if !strings.Contains(err.Error(), "is not managed by systemd") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunDeployUpgradeDownloadsLatestReleaseAndRestartsService(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}

	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)

	deployedBinary := filepath.Join(homeDir, ".local", "bin", "workbuddy")
	if err := os.MkdirAll(filepath.Dir(deployedBinary), 0o755); err != nil {
		t.Fatalf("mkdir binary dir: %v", err)
	}
	if err := os.WriteFile(deployedBinary, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("write deployed binary: %v", err)
	}

	unitPath := filepath.Join(configDir, "systemd", "user", "demo.service")
	manifestPath := filepath.Join(configDir, "workbuddy", "deployments", "demo.json")
	manifest := &deploymentManifest{
		SchemaVersion:    deploymentManifestVer,
		Name:             "demo",
		Scope:            "user",
		BinaryPath:       deployedBinary,
		WorkingDirectory: repoDir,
		Command:          []string{"serve", "--config-dir", ".github/workbuddy", "--db-path", ".workbuddy/workbuddy.db", "--max-parallel-tasks", "2"},
		InstalledVersion: "old",
		InstalledAt:      time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC),
		Systemd: &deploymentSystemd{
			ServiceName: "demo",
			UnitPath:    unitPath,
			Description: "Workbuddy demo (serve)",
			Enabled:     true,
			Started:     true,
		},
	}
	if err := writeDeploymentManifest(manifestPath, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	archive := buildReleaseArchive(t, "release-binary")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/workbuddy/releases/latest":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"tag_name":"v1.2.3"}`)
		case "/acme/workbuddy/releases/download/v1.2.3/" + releaseArchiveName("1.2.3"):
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	restore := overrideDeployGlobals(t, filepath.Join(tempDir, "unused-current-binary"))
	defer restore()
	deployHTTPClient = server.Client()
	deployGitHubAPIBaseURL = server.URL
	deployGitHubDownloadBase = server.URL

	var systemctlCalls []string
	deployRunSystemctl = func(_ context.Context, scope string, args ...string) error {
		systemctlCalls = append(systemctlCalls, scope+":"+strings.Join(args, " "))
		return nil
	}
	deployNow = func() time.Time {
		return time.Date(2026, 4, 18, 14, 0, 0, 0, time.UTC)
	}

	var stdout bytes.Buffer
	err := runDeployUpgradeWithOpts(context.Background(), &deployUpgradeOpts{
		deployLookupOpts: deployLookupOpts{name: "demo", scope: "user"},
		version:          "latest",
		repository:       "acme/workbuddy",
	}, &stdout)
	if err != nil {
		t.Fatalf("runDeployUpgradeWithOpts: %v", err)
	}

	content, err := os.ReadFile(deployedBinary)
	if err != nil {
		t.Fatalf("read upgraded binary: %v", err)
	}
	if got, want := string(content), "release-binary"; got != want {
		t.Fatalf("upgraded binary content = %q, want %q", got, want)
	}

	updated := mustReadManifest(t, manifestPath)
	if got, want := updated.InstalledVersion, "1.2.3"; got != want {
		t.Fatalf("updated manifest version = %q, want %q", got, want)
	}

	gotCalls := strings.Join(systemctlCalls, " | ")
	wantCalls := "user:daemon-reload | user:enable demo.service | user:restart demo.service"
	if gotCalls != wantCalls {
		t.Fatalf("systemctl calls = %q, want %q", gotCalls, wantCalls)
	}
	if !strings.Contains(stdout.String(), "installed release 1.2.3") {
		t.Fatalf("stdout missing upgrade message: %q", stdout.String())
	}
}

func TestRunDeployUpgradeKeepsStoppedServiceStopped(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}

	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)

	deployedBinary := filepath.Join(homeDir, ".local", "bin", "workbuddy")
	if err := os.MkdirAll(filepath.Dir(deployedBinary), 0o755); err != nil {
		t.Fatalf("mkdir binary dir: %v", err)
	}
	if err := os.WriteFile(deployedBinary, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("write deployed binary: %v", err)
	}

	unitPath := filepath.Join(configDir, "systemd", "user", "demo.service")
	manifestPath := filepath.Join(configDir, "workbuddy", "deployments", "demo.json")
	manifest := &deploymentManifest{
		SchemaVersion:    deploymentManifestVer,
		Name:             "demo",
		Scope:            "user",
		BinaryPath:       deployedBinary,
		WorkingDirectory: repoDir,
		Command:          []string{"serve"},
		InstalledVersion: "old",
		InstalledAt:      time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC),
		Systemd: &deploymentSystemd{
			ServiceName: "demo",
			UnitPath:    unitPath,
			Description: "Workbuddy demo (serve)",
			Enabled:     true,
			Started:     false,
		},
	}
	if err := writeDeploymentManifest(manifestPath, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	archive := buildReleaseArchive(t, "release-binary")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/workbuddy/releases/latest":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"tag_name":"v1.2.3"}`)
		case "/acme/workbuddy/releases/download/v1.2.3/" + releaseArchiveName("1.2.3"):
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	restore := overrideDeployGlobals(t, filepath.Join(tempDir, "unused-current-binary"))
	defer restore()
	deployHTTPClient = server.Client()
	deployGitHubAPIBaseURL = server.URL
	deployGitHubDownloadBase = server.URL

	var systemctlCalls []string
	deployRunSystemctl = func(_ context.Context, scope string, args ...string) error {
		systemctlCalls = append(systemctlCalls, scope+":"+strings.Join(args, " "))
		return nil
	}

	var stdout bytes.Buffer
	err := runDeployUpgradeWithOpts(context.Background(), &deployUpgradeOpts{
		deployLookupOpts: deployLookupOpts{name: "demo", scope: "user"},
		version:          "latest",
		repository:       "acme/workbuddy",
	}, &stdout)
	if err != nil {
		t.Fatalf("runDeployUpgradeWithOpts: %v", err)
	}

	gotCalls := strings.Join(systemctlCalls, " | ")
	wantCalls := "user:daemon-reload | user:enable demo.service"
	if gotCalls != wantCalls {
		t.Fatalf("systemctl calls = %q, want %q", gotCalls, wantCalls)
	}
	if !strings.Contains(stdout.String(), "left demo.service stopped") {
		t.Fatalf("stdout missing stopped message: %q", stdout.String())
	}
}

func TestRunDeployDeleteRemovesSystemdDeploymentButKeepsBinary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}

	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)

	deployedBinary := filepath.Join(homeDir, ".local", "bin", "workbuddy")
	if err := os.MkdirAll(filepath.Dir(deployedBinary), 0o755); err != nil {
		t.Fatalf("mkdir binary dir: %v", err)
	}
	if err := os.WriteFile(deployedBinary, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write deployed binary: %v", err)
	}

	unitPath := filepath.Join(configDir, "systemd", "user", "demo.service")
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatalf("mkdir unit dir: %v", err)
	}
	if err := os.WriteFile(unitPath, []byte("[Unit]\nDescription=demo\n"), 0o644); err != nil {
		t.Fatalf("write unit: %v", err)
	}
	manifestPath := filepath.Join(configDir, "workbuddy", "deployments", "demo.json")
	manifest := &deploymentManifest{
		SchemaVersion:    deploymentManifestVer,
		Name:             "demo",
		Scope:            "user",
		BinaryPath:       deployedBinary,
		WorkingDirectory: repoDir,
		Command:          []string{"serve"},
		Systemd: &deploymentSystemd{
			ServiceName: "demo",
			UnitPath:    unitPath,
			Description: "Workbuddy demo (serve)",
			Enabled:     true,
			Started:     true,
		},
	}
	if err := writeDeploymentManifest(manifestPath, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	var systemctlCalls []string
	deployRunSystemctl = func(_ context.Context, scope string, args ...string) error {
		systemctlCalls = append(systemctlCalls, scope+":"+strings.Join(args, " "))
		return nil
	}

	var stdout bytes.Buffer
	err := runDeployDeleteWithOpts(context.Background(), &deployLookupOpts{name: "demo", scope: "user", force: true}, &stdout)
	if err != nil {
		t.Fatalf("runDeployDeleteWithOpts: %v", err)
	}

	gotCalls := strings.Join(systemctlCalls, " | ")
	wantCalls := "user:disable --now demo.service | user:reset-failed demo.service | user:daemon-reload"
	if gotCalls != wantCalls {
		t.Fatalf("systemctl calls = %q, want %q", gotCalls, wantCalls)
	}
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Fatalf("manifest still exists: err=%v", err)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Fatalf("unit still exists: err=%v", err)
	}
	content, err := os.ReadFile(deployedBinary)
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	if string(content) != "binary" {
		t.Fatalf("binary content = %q", string(content))
	}
	if !strings.Contains(stdout.String(), "left binary in place") {
		t.Fatalf("stdout missing binary message: %q", stdout.String())
	}
}

func TestRunDeployDeleteRemovesNonSystemdManifest(t *testing.T) {
	tempDir := t.TempDir()
	configDir := filepath.Join(tempDir, "xdg")
	manifestPath := filepath.Join(configDir, "workbuddy", "deployments", "demo.json")
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("HOME", filepath.Join(tempDir, "home"))

	manifest := &deploymentManifest{
		SchemaVersion:    deploymentManifestVer,
		Name:             "demo",
		Scope:            "user",
		BinaryPath:       "/tmp/workbuddy",
		WorkingDirectory: tempDir,
		Command:          []string{"serve"},
	}
	if err := writeDeploymentManifest(manifestPath, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	var stdout bytes.Buffer
	err := runDeployDeleteWithOpts(context.Background(), &deployLookupOpts{name: "demo", scope: "user", force: true}, &stdout)
	if err != nil {
		t.Fatalf("runDeployDeleteWithOpts: %v", err)
	}
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Fatalf("manifest still exists: err=%v", err)
	}
	if !strings.Contains(stdout.String(), "left binary in place at /tmp/workbuddy") {
		t.Fatalf("stdout missing binary path message: %q", stdout.String())
	}
}

func TestNormalizeDeployCommandArgsDefaultsAndStripsLeadingBinary(t *testing.T) {
	args, err := normalizeDeployCommandArgs([]string{"workbuddy", "serve", "--config-dir", ".github/workbuddy"})
	if err != nil {
		t.Fatalf("normalizeDeployCommandArgs: %v", err)
	}
	if got, want := strings.Join(args, " "), "serve --config-dir .github/workbuddy"; got != want {
		t.Fatalf("normalized args = %q, want %q", got, want)
	}

	defaults, err := normalizeDeployCommandArgs(nil)
	if err != nil {
		t.Fatalf("normalizeDeployCommandArgs(nil): %v", err)
	}
	if len(defaults) == 0 || defaults[0] != "serve" {
		t.Fatalf("default args = %v, want serve-based default", defaults)
	}
}

func TestParseDeployEnvRejectsNewlines(t *testing.T) {
	if _, err := parseDeployEnv([]string{"FOO=line1\nline2"}); err == nil {
		t.Fatal("expected newline-containing env value to be rejected")
	}
}

func TestReadDeploymentManifestRejectsUnsupportedSchemaVersion(t *testing.T) {
	tempDir := t.TempDir()
	manifestPath := filepath.Join(tempDir, "demo.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":2,"binary_path":"/tmp/workbuddy"}`), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	if _, err := readDeploymentManifest(manifestPath); err == nil {
		t.Fatal("expected unsupported schema_version to fail")
	}
}

func TestApplyGitHubAuthPrefersGHToken(t *testing.T) {
	t.Setenv("GH_TOKEN", "gh-token")
	t.Setenv("GITHUB_TOKEN", "github-token")
	t.Setenv("GITHUB_OAUTH", "oauth-token")

	req := httptest.NewRequest(http.MethodGet, "https://example.com", nil)
	applyGitHubAuth(req)

	if got, want := req.Header.Get("Authorization"), "Bearer gh-token"; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

func TestDeployHTTPClientHasTimeout(t *testing.T) {
	if deployHTTPClient == nil {
		t.Fatal("deployHTTPClient is nil")
	}
	if deployHTTPClient.Timeout <= 0 {
		t.Fatalf("deployHTTPClient timeout = %s, want > 0", deployHTTPClient.Timeout)
	}
}

func TestRunDeployListIncludesUserAndSystemDeployments(t *testing.T) {
	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	systemRoot := filepath.Join(tempDir, "system")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	restoreSystem := overrideDeploySystemPaths(t, systemRoot)
	defer restoreSystem()

	writeScopedManifest(t, "user", "workbuddy-worker", &deploymentManifest{
		BinaryPath:       "/home/demo/.local/bin/workbuddy",
		WorkingDirectory: repoDir,
		Command:          []string{"worker", "--repo", "owner/repo"},
	})
	writeScopedManifest(t, "system", "workbuddy-coordinator", &deploymentManifest{
		BinaryPath:       "/opt/workbuddy-coordinator",
		WorkingDirectory: repoDir,
		Command:          []string{"coordinator", "--db", "/srv/workbuddy.db"},
	})

	var stdout bytes.Buffer
	if err := runDeployListWithOpts(&deployListOpts{scope: "all", format: "text"}, &stdout); err != nil {
		t.Fatalf("runDeployListWithOpts: %v", err)
	}

	output := stdout.String()
	for _, want := range []string{
		"NAME",
		"SCOPE",
		"BINARY PATH",
		"COMMAND",
		"workbuddy-coordinator",
		"system",
		"/opt/workbuddy-coordinator",
		"coordinator --db /srv/workbuddy.db",
		"workbuddy-worker",
		"user",
		"/home/demo/.local/bin/workbuddy",
		"worker --repo owner/repo",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("list output missing %q:\n%s", want, output)
		}
	}
}

func TestRunDeployListJSONIsStable(t *testing.T) {
	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	systemRoot := filepath.Join(tempDir, "system")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	restoreSystem := overrideDeploySystemPaths(t, systemRoot)
	defer restoreSystem()

	writeScopedManifest(t, "user", "workbuddy-worker", &deploymentManifest{
		BinaryPath:       "/home/demo/.local/bin/workbuddy",
		WorkingDirectory: repoDir,
		Command:          []string{"worker", "--repo", "owner/repo"},
	})
	writeScopedManifest(t, "system", "workbuddy-coordinator", &deploymentManifest{
		BinaryPath:       "/opt/workbuddy-coordinator",
		WorkingDirectory: repoDir,
		Command:          []string{"coordinator", "--db", "/srv/workbuddy.db"},
	})

	var stdout bytes.Buffer
	if err := runDeployListWithOpts(&deployListOpts{scope: "all", format: "json"}, &stdout); err != nil {
		t.Fatalf("runDeployListWithOpts: %v", err)
	}

	const want = `{
  "deployments": [
    {
      "name": "workbuddy-coordinator",
      "scope": "system",
      "binary_path": "/opt/workbuddy-coordinator",
      "command": [
        "coordinator",
        "--db",
        "/srv/workbuddy.db"
      ]
    },
    {
      "name": "workbuddy-worker",
      "scope": "user",
      "binary_path": "/home/demo/.local/bin/workbuddy",
      "command": [
        "worker",
        "--repo",
        "owner/repo"
      ]
    }
  ]
}
`
	if got := stdout.String(); got != want {
		t.Fatalf("json output = %q, want %q", got, want)
	}
}

func TestRunDeployStopMissingNameListsInstalledDeployments(t *testing.T) {
	tempDir := t.TempDir()
	configDir := filepath.Join(tempDir, "xdg")
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("HOME", filepath.Join(tempDir, "home"))

	writeScopedManifest(t, "user", "workbuddy-coordinator", &deploymentManifest{
		BinaryPath:       "/opt/workbuddy-coordinator",
		WorkingDirectory: tempDir,
		Command:          []string{"coordinator"},
	})
	writeScopedManifest(t, "user", "workbuddy-worker", &deploymentManifest{
		BinaryPath:       "/opt/workbuddy-worker",
		WorkingDirectory: tempDir,
		Command:          []string{"worker"},
	})

	var stdout bytes.Buffer
	err := runDeployStopWithOpts(context.Background(), &deployLookupOpts{name: "missing", scope: "user"}, &stdout)
	if err == nil {
		t.Fatal("expected missing deployment to fail")
	}
	if !strings.Contains(err.Error(), `deployment "missing" not found in user scope`) {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "installed: workbuddy-coordinator, workbuddy-worker") {
		t.Fatalf("missing installed deployments in error: %v", err)
	}
}

func TestRunDeployStopAllStopsEveryDeploymentInScope(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}

	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)

	writeScopedManifest(t, "user", "alpha", &deploymentManifest{
		BinaryPath:       filepath.Join(homeDir, ".local", "bin", "alpha"),
		WorkingDirectory: repoDir,
		Command:          []string{"serve"},
		Systemd: &deploymentSystemd{
			Enabled: true,
			Started: true,
		},
	})
	writeScopedManifest(t, "user", "beta", &deploymentManifest{
		BinaryPath:       filepath.Join(homeDir, ".local", "bin", "beta"),
		WorkingDirectory: repoDir,
		Command:          []string{"worker"},
		Systemd: &deploymentSystemd{
			Enabled: true,
			Started: true,
		},
	})

	var systemctlCalls []string
	deployRunSystemctl = func(_ context.Context, scope string, args ...string) error {
		systemctlCalls = append(systemctlCalls, scope+":"+strings.Join(args, " "))
		return nil
	}
	defer func() { deployRunSystemctl = runSystemctl }()

	var stdout bytes.Buffer
	err := runDeployStopWithOpts(context.Background(), &deployLookupOpts{scope: "user", all: true, force: true}, &stdout)
	if err != nil {
		t.Fatalf("runDeployStopWithOpts: %v", err)
	}

	gotCalls := strings.Join(systemctlCalls, " | ")
	wantCalls := "user:stop alpha.service | user:reset-failed alpha.service | user:stop beta.service | user:reset-failed beta.service"
	if gotCalls != wantCalls {
		t.Fatalf("systemctl calls = %q, want %q", gotCalls, wantCalls)
	}
	for _, name := range []string{"alpha", "beta"} {
		manifestPath := filepath.Join(configDir, "workbuddy", "deployments", name+".json")
		if got := mustReadManifest(t, manifestPath).Systemd.Started; got {
			t.Fatalf("%s started = %v, want false", name, got)
		}
	}
}

func TestRunDeployStartAllStartsEveryDeploymentInScope(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}

	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)

	writeScopedManifest(t, "user", "alpha", &deploymentManifest{
		BinaryPath:       filepath.Join(homeDir, ".local", "bin", "alpha"),
		WorkingDirectory: repoDir,
		Command:          []string{"serve"},
		Systemd: &deploymentSystemd{
			Enabled: false,
			Started: false,
		},
	})
	writeScopedManifest(t, "user", "beta", &deploymentManifest{
		BinaryPath:       filepath.Join(homeDir, ".local", "bin", "beta"),
		WorkingDirectory: repoDir,
		Command:          []string{"worker"},
		Systemd: &deploymentSystemd{
			Enabled: false,
			Started: false,
		},
	})

	var systemctlCalls []string
	deployRunSystemctl = func(_ context.Context, scope string, args ...string) error {
		systemctlCalls = append(systemctlCalls, scope+":"+strings.Join(args, " "))
		return nil
	}
	defer func() { deployRunSystemctl = runSystemctl }()

	var stdout bytes.Buffer
	err := runDeployStartWithOpts(context.Background(), &deployLookupOpts{scope: "user", all: true}, &stdout)
	if err != nil {
		t.Fatalf("runDeployStartWithOpts: %v", err)
	}

	gotCalls := strings.Join(systemctlCalls, " | ")
	wantCalls := "user:daemon-reload | user:start alpha.service | user:daemon-reload | user:start beta.service"
	if gotCalls != wantCalls {
		t.Fatalf("systemctl calls = %q, want %q", gotCalls, wantCalls)
	}
	for _, name := range []string{"alpha", "beta"} {
		manifestPath := filepath.Join(configDir, "workbuddy", "deployments", name+".json")
		if got := mustReadManifest(t, manifestPath).Systemd.Started; !got {
			t.Fatalf("%s started = %v, want true", name, got)
		}
	}
}

func TestRunDeployRedeployAllReinstallsEveryDeploymentInScope(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}

	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)

	sourceBinary := filepath.Join(tempDir, "current-workbuddy")
	if err := os.WriteFile(sourceBinary, []byte("new-binary"), 0o755); err != nil {
		t.Fatalf("write source binary: %v", err)
	}
	restore := overrideDeployGlobals(t, sourceBinary)
	defer restore()

	for _, name := range []string{"alpha", "beta"} {
		binaryPath := filepath.Join(homeDir, ".local", "bin", name)
		if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
			t.Fatalf("mkdir binary dir: %v", err)
		}
		if err := os.WriteFile(binaryPath, []byte("old-binary"), 0o755); err != nil {
			t.Fatalf("write deployed binary: %v", err)
		}
		writeScopedManifest(t, "user", name, &deploymentManifest{
			BinaryPath:       binaryPath,
			WorkingDirectory: repoDir,
			Command:          []string{"serve"},
			InstalledVersion: "old",
			Systemd: &deploymentSystemd{
				Enabled: true,
				Started: true,
			},
		})
	}

	var systemctlCalls []string
	deployRunSystemctl = func(_ context.Context, scope string, args ...string) error {
		systemctlCalls = append(systemctlCalls, scope+":"+strings.Join(args, " "))
		return nil
	}
	deployNow = func() time.Time {
		return time.Date(2026, 4, 18, 15, 0, 0, 0, time.UTC)
	}

	var stdout bytes.Buffer
	err := runDeployRedeployWithOpts(context.Background(), &deployLookupOpts{scope: "user", all: true}, &stdout)
	if err != nil {
		t.Fatalf("runDeployRedeployWithOpts: %v", err)
	}

	gotCalls := strings.Join(systemctlCalls, " | ")
	wantCalls := "user:daemon-reload | user:enable alpha.service | user:restart alpha.service | user:daemon-reload | user:enable beta.service | user:restart beta.service"
	if gotCalls != wantCalls {
		t.Fatalf("systemctl calls = %q, want %q", gotCalls, wantCalls)
	}
	for _, name := range []string{"alpha", "beta"} {
		binaryPath := filepath.Join(homeDir, ".local", "bin", name)
		content, err := os.ReadFile(binaryPath)
		if err != nil {
			t.Fatalf("read binary %s: %v", name, err)
		}
		if got := string(content); got != "new-binary" {
			t.Fatalf("%s binary = %q, want %q", name, got, "new-binary")
		}
		manifestPath := filepath.Join(configDir, "workbuddy", "deployments", name+".json")
		if got := mustReadManifest(t, manifestPath).InstalledVersion; got != deployedVersionLabel() {
			t.Fatalf("%s installed version = %q, want %q", name, got, deployedVersionLabel())
		}
	}
}

func TestRunDeployUpgradeAllUpgradesEveryDeploymentInScope(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}

	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)

	for _, name := range []string{"alpha", "beta"} {
		binaryPath := filepath.Join(homeDir, ".local", "bin", name)
		if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
			t.Fatalf("mkdir binary dir: %v", err)
		}
		if err := os.WriteFile(binaryPath, []byte("old-binary"), 0o755); err != nil {
			t.Fatalf("write deployed binary: %v", err)
		}
		writeScopedManifest(t, "user", name, &deploymentManifest{
			BinaryPath:       binaryPath,
			WorkingDirectory: repoDir,
			Command:          []string{"serve"},
			InstalledVersion: "old",
			Systemd: &deploymentSystemd{
				Enabled: true,
				Started: true,
			},
		})
	}

	archive := buildReleaseArchive(t, "release-binary")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/workbuddy/releases/latest":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"tag_name":"v1.2.3"}`)
		case "/acme/workbuddy/releases/download/v1.2.3/" + releaseArchiveName("1.2.3"):
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	restore := overrideDeployGlobals(t, filepath.Join(tempDir, "unused-current-binary"))
	defer restore()
	deployHTTPClient = server.Client()
	deployGitHubAPIBaseURL = server.URL
	deployGitHubDownloadBase = server.URL

	var systemctlCalls []string
	deployRunSystemctl = func(_ context.Context, scope string, args ...string) error {
		systemctlCalls = append(systemctlCalls, scope+":"+strings.Join(args, " "))
		return nil
	}
	deployNow = func() time.Time {
		return time.Date(2026, 4, 18, 16, 0, 0, 0, time.UTC)
	}

	var stdout bytes.Buffer
	err := runDeployUpgradeWithOpts(context.Background(), &deployUpgradeOpts{
		deployLookupOpts: deployLookupOpts{scope: "user", all: true},
		version:          "latest",
		repository:       "acme/workbuddy",
	}, &stdout)
	if err != nil {
		t.Fatalf("runDeployUpgradeWithOpts: %v", err)
	}

	gotCalls := strings.Join(systemctlCalls, " | ")
	wantCalls := "user:daemon-reload | user:enable alpha.service | user:restart alpha.service | user:daemon-reload | user:enable beta.service | user:restart beta.service"
	if gotCalls != wantCalls {
		t.Fatalf("systemctl calls = %q, want %q", gotCalls, wantCalls)
	}
	for _, name := range []string{"alpha", "beta"} {
		binaryPath := filepath.Join(homeDir, ".local", "bin", name)
		content, err := os.ReadFile(binaryPath)
		if err != nil {
			t.Fatalf("read binary %s: %v", name, err)
		}
		if got := string(content); got != "release-binary" {
			t.Fatalf("%s binary = %q, want %q", name, got, "release-binary")
		}
		manifestPath := filepath.Join(configDir, "workbuddy", "deployments", name+".json")
		if got := mustReadManifest(t, manifestPath).InstalledVersion; got != "1.2.3" {
			t.Fatalf("%s installed version = %q, want %q", name, got, "1.2.3")
		}
	}
}

func TestRunDeployDeleteAllDeletesEveryDeploymentInScope(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}

	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)

	for _, name := range []string{"alpha", "beta"} {
		binaryPath := filepath.Join(homeDir, ".local", "bin", name)
		unitPath := filepath.Join(configDir, "systemd", "user", name+".service")
		if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
			t.Fatalf("mkdir binary dir: %v", err)
		}
		if err := os.WriteFile(binaryPath, []byte("binary"), 0o755); err != nil {
			t.Fatalf("write deployed binary: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
			t.Fatalf("mkdir unit dir: %v", err)
		}
		if err := os.WriteFile(unitPath, []byte("[Unit]\nDescription=demo\n"), 0o644); err != nil {
			t.Fatalf("write unit: %v", err)
		}
		writeScopedManifest(t, "user", name, &deploymentManifest{
			BinaryPath:       binaryPath,
			WorkingDirectory: repoDir,
			Command:          []string{"serve"},
			Systemd: &deploymentSystemd{
				Enabled:  true,
				Started:  true,
				UnitPath: unitPath,
			},
		})
	}

	var systemctlCalls []string
	deployRunSystemctl = func(_ context.Context, scope string, args ...string) error {
		systemctlCalls = append(systemctlCalls, scope+":"+strings.Join(args, " "))
		return nil
	}
	defer func() { deployRunSystemctl = runSystemctl }()

	var stdout bytes.Buffer
	err := runDeployDeleteWithOpts(context.Background(), &deployLookupOpts{scope: "user", all: true, force: true}, &stdout)
	if err != nil {
		t.Fatalf("runDeployDeleteWithOpts: %v", err)
	}

	gotCalls := strings.Join(systemctlCalls, " | ")
	wantCalls := "user:disable --now alpha.service | user:reset-failed alpha.service | user:daemon-reload | user:disable --now beta.service | user:reset-failed beta.service | user:daemon-reload"
	if gotCalls != wantCalls {
		t.Fatalf("systemctl calls = %q, want %q", gotCalls, wantCalls)
	}
	for _, name := range []string{"alpha", "beta"} {
		manifestPath := filepath.Join(configDir, "workbuddy", "deployments", name+".json")
		if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
			t.Fatalf("%s manifest still exists: err=%v", name, err)
		}
		unitPath := filepath.Join(configDir, "systemd", "user", name+".service")
		if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
			t.Fatalf("%s unit still exists: err=%v", name, err)
		}
		binaryPath := filepath.Join(homeDir, ".local", "bin", name)
		content, err := os.ReadFile(binaryPath)
		if err != nil {
			t.Fatalf("read binary %s: %v", name, err)
		}
		if got := string(content); got != "binary" {
			t.Fatalf("%s binary = %q, want %q", name, got, "binary")
		}
	}
}

func overrideDeployGlobals(t *testing.T, executablePath string) func() {
	t.Helper()
	oldExec := deployExecutablePath
	oldNow := deployNow
	oldSystemctl := deployRunSystemctl
	oldClient := deployHTTPClient
	oldAPIBase := deployGitHubAPIBaseURL
	oldDownloadBase := deployGitHubDownloadBase
	deployExecutablePath = func() (string, error) { return executablePath, nil }
	deployNow = time.Now
	deployRunSystemctl = runSystemctl
	deployHTTPClient = http.DefaultClient
	deployGitHubAPIBaseURL = "https://api.github.com"
	deployGitHubDownloadBase = "https://github.com"
	return func() {
		deployExecutablePath = oldExec
		deployNow = oldNow
		deployRunSystemctl = oldSystemctl
		deployHTTPClient = oldClient
		deployGitHubAPIBaseURL = oldAPIBase
		deployGitHubDownloadBase = oldDownloadBase
	}
}

func overrideDeploySystemPaths(t *testing.T, root string) func() {
	t.Helper()
	oldBinary := deploySystemBinaryPath
	oldManifestDir := deploySystemManifestDir
	oldUnitDir := deploySystemUnitDir
	deploySystemBinaryPath = filepath.Join(root, "bin", "workbuddy")
	deploySystemManifestDir = filepath.Join(root, "deployments")
	deploySystemUnitDir = filepath.Join(root, "systemd")
	return func() {
		deploySystemBinaryPath = oldBinary
		deploySystemManifestDir = oldManifestDir
		deploySystemUnitDir = oldUnitDir
	}
}

func writeScopedManifest(t *testing.T, scope, name string, manifest *deploymentManifest) string {
	t.Helper()
	_, scopePaths, err := resolveDeployScopePaths(scope)
	if err != nil {
		t.Fatalf("resolve scope paths: %v", err)
	}
	clone := *manifest
	if clone.SchemaVersion == 0 {
		clone.SchemaVersion = deploymentManifestVer
	}
	clone.Name = name
	clone.Scope = scope
	if clone.Systemd != nil {
		systemdClone := *clone.Systemd
		if systemdClone.ServiceName == "" {
			systemdClone.ServiceName = name
		}
		if systemdClone.UnitPath == "" {
			systemdClone.UnitPath = filepath.Join(scopePaths.unitDir, name+".service")
		}
		clone.Systemd = &systemdClone
	}
	manifestPath := deploymentManifestPath(scopePaths.manifestDir, name)
	if err := writeDeploymentManifest(manifestPath, &clone); err != nil {
		t.Fatalf("write manifest %s: %v", name, err)
	}
	return manifestPath
}

func overrideCommandIsInteractiveTerminal(t *testing.T, interactive bool) func() {
	t.Helper()
	old := commandIsInteractiveTerminal
	commandIsInteractiveTerminal = func() bool { return interactive }
	return func() {
		commandIsInteractiveTerminal = old
	}
}


func mustReadManifest(t *testing.T, path string) *deploymentManifest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest deploymentManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	return &manifest
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func buildReleaseArchive(t *testing.T, binaryContent string) []byte {
	t.Helper()
	var archive bytes.Buffer
	gzipWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzipWriter)
	content := []byte(binaryContent)
	header := &tar.Header{
		Name: "workbuddy",
		Mode: 0o755,
		Size: int64(len(content)),
	}
	if err := tarWriter.WriteHeader(header); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tarWriter.Write(content); err != nil {
		t.Fatalf("write tar body: %v", err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return archive.Bytes()
}

func TestRunDeployInstallEnableUpdaterWritesUnitAndEnables(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}

	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Chdir(repoDir)

	sourceBinary := filepath.Join(tempDir, "current-workbuddy")
	if err := os.WriteFile(sourceBinary, []byte("binary-v1"), 0o755); err != nil {
		t.Fatalf("write source binary: %v", err)
	}

	restore := overrideDeployGlobals(t, sourceBinary)
	defer restore()

	var systemctlCalls []string
	deployRunSystemctl = func(_ context.Context, scope string, args ...string) error {
		systemctlCalls = append(systemctlCalls, scope+":"+strings.Join(args, " "))
		return nil
	}

	var stdout bytes.Buffer
	err := runDeployInstallWithOpts(context.Background(), &deployInstallOpts{
		name:                "workbuddy",
		scope:               "user",
		systemd:             false,
		envFiles:            []string{"/home/ddq/.config/workbuddy/worker.env"},
		enableUpdater:       true,
		updaterRepo:         "Lincyaw/workbuddy",
		updaterInterval:     "5m",
		updaterRestartUnits: []string{"workbuddy-coordinator.service", "workbuddy-worker.service"},
		updaterName:         "workbuddy-updater",
	}, &stdout)
	if err != nil {
		t.Fatalf("runDeployInstallWithOpts: %v", err)
	}

	updaterPath := filepath.Join(configDir, "systemd", "user", "workbuddy-updater.service")
	unitBytes, err := os.ReadFile(updaterPath)
	if err != nil {
		t.Fatalf("read updater unit: %v", err)
	}
	unit := string(unitBytes)
	for _, want := range []string{
		"Type=simple\n",
		"Restart=always\n",
		"RestartSec=30s\n",
		"WantedBy=default.target\n",
		"EnvironmentFile=/home/ddq/.config/workbuddy/worker.env\n",
		`"deploy" "watch" "--repo" "Lincyaw/workbuddy" "--interval" "5m" "--systemctl-scope" "user" "--restart-units" "workbuddy-coordinator.service,workbuddy-worker.service"`,
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("updater unit missing %q:\n%s", want, unit)
		}
	}

	gotCalls := strings.Join(systemctlCalls, " | ")
	wantCalls := "user:daemon-reload | user:enable workbuddy-updater.service | user:start workbuddy-updater.service"
	if gotCalls != wantCalls {
		t.Fatalf("systemctl calls = %q, want %q", gotCalls, wantCalls)
	}
	if !strings.Contains(stdout.String(), "wrote updater unit ") {
		t.Fatalf("stdout missing updater unit message: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "enabled workbuddy-updater.service") {
		t.Fatalf("stdout missing enabled message: %q", stdout.String())
	}
}

func TestParseDeployInstallFlagsEnableUpdaterDefaults(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().AddFlagSet(deployInstallCmd.Flags())
	if err := cmd.ParseFlags([]string{"--enable-updater"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	opts, err := parseDeployInstallFlags(cmd, nil)
	if err != nil {
		t.Fatalf("parseDeployInstallFlags: %v", err)
	}
	if !opts.enableUpdater {
		t.Fatal("expected enableUpdater true")
	}
	if got, want := opts.updaterRepo, "Lincyaw/workbuddy"; got != want {
		t.Fatalf("updaterRepo = %q, want %q", got, want)
	}
	if got, want := opts.updaterInterval, "5m"; got != want {
		t.Fatalf("updaterInterval = %q, want %q", got, want)
	}
	wantUnits := []string{"workbuddy-coordinator.service", "workbuddy-worker.service"}
	if !reflect.DeepEqual(opts.updaterRestartUnits, wantUnits) {
		t.Fatalf("updaterRestartUnits = %v, want %v", opts.updaterRestartUnits, wantUnits)
	}
	if got, want := opts.updaterName, "workbuddy-updater"; got != want {
		t.Fatalf("updaterName = %q, want %q", got, want)
	}
}

// newDeployInstallTestCmd builds a fresh cobra.Command with an independent
// flag set that mirrors the production `deploy install` flag schema, so tests
// can drive runDeployInstallCmd without mutating the global deployInstallCmd
// flag state across cases. (pflag's AddFlagSet shares Flag pointers, which
// causes value bleed between tests.)
func newDeployInstallTestCmd() *cobra.Command {
	c := &cobra.Command{Use: "install", RunE: runDeployInstallCmd}
	deployInstallCmd.Flags().VisitAll(func(f *pflag.Flag) {
		switch f.Value.Type() {
		case "bool":
			c.Flags().Bool(f.Name, f.DefValue == "true", f.Usage)
		case "string":
			c.Flags().String(f.Name, f.DefValue, f.Usage)
		case "stringArray":
			c.Flags().StringArray(f.Name, nil, f.Usage)
		case "stringSlice":
			c.Flags().StringSlice(f.Name, nil, f.Usage)
		default:
			c.Flags().String(f.Name, f.DefValue, f.Usage)
		}
	})
	return c
}

func newDeployUpgradeTestCmd() *cobra.Command {
	c := &cobra.Command{Use: "upgrade", RunE: runDeployUpgradeCmd}
	deployUpgradeCmd.Flags().VisitAll(func(f *pflag.Flag) {
		switch f.Value.Type() {
		case "bool":
			c.Flags().Bool(f.Name, f.DefValue == "true", f.Usage)
		case "string":
			c.Flags().String(f.Name, f.DefValue, f.Usage)
		case "stringArray":
			c.Flags().StringArray(f.Name, nil, f.Usage)
		case "stringSlice":
			c.Flags().StringSlice(f.Name, nil, f.Usage)
		default:
			c.Flags().String(f.Name, f.DefValue, f.Usage)
		}
	})
	return c
}

// TestRunDeployInstallCmdDefaultsToBundle verifies that `workbuddy deploy
// install` with no flags installs the supervisor + coordinator + worker
// bundle layout, satisfying issue #281's AC-1 (bundle is the default and
// the headline path).
func TestRunDeployInstallCmdDefaultsToBundle(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}

	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Chdir(repoDir)

	sourceBinary := filepath.Join(tempDir, "current-workbuddy")
	if err := os.WriteFile(sourceBinary, []byte("binary-default"), 0o755); err != nil {
		t.Fatalf("write source binary: %v", err)
	}
	restore := overrideDeployGlobals(t, sourceBinary)
	defer restore()
	deployRunSystemctl = func(_ context.Context, _ string, _ ...string) error { return nil }

	c := newDeployInstallTestCmd()
	if err := c.ParseFlags([]string{"--scope=user"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	var stdout bytes.Buffer
	c.SetOut(&stdout)
	if err := runDeployInstallCmd(c, c.Flags().Args()); err != nil {
		t.Fatalf("runDeployInstallCmd: %v", err)
	}

	for _, name := range []string{bundleSupervisorName, bundleCoordinatorName, bundleWorkerName} {
		manifestPath := filepath.Join(configDir, "workbuddy", "deployments", name+".json")
		if _, err := os.Stat(manifestPath); err != nil {
			t.Errorf("expected bundle manifest %s, got: %v", manifestPath, err)
		}
		unitPath := filepath.Join(configDir, "systemd", "user", name+".service")
		if _, err := os.Stat(unitPath); err != nil {
			t.Errorf("expected bundle unit %s, got: %v", unitPath, err)
		}
	}
	if !strings.Contains(stdout.String(), "bundle install complete") {
		t.Errorf("stdout missing bundle summary: %q", stdout.String())
	}
	// And the legacy single-process unit must NOT have been written.
	if _, err := os.Stat(filepath.Join(configDir, "workbuddy", "deployments", "workbuddy.json")); !os.IsNotExist(err) {
		t.Errorf("legacy serve manifest should not exist by default: err=%v", err)
	}
}

// TestRunDeployInstallCmdLegacyServeOptOut covers AC-1's opt-out path:
// `--legacy-serve` keeps the old single-process install reachable and emits a
// `serve` ExecStart, while honoring the explicit `--name`.
func TestRunDeployInstallCmdLegacyServeOptOut(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}

	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Chdir(repoDir)

	sourceBinary := filepath.Join(tempDir, "current-workbuddy")
	if err := os.WriteFile(sourceBinary, []byte("binary-legacy"), 0o755); err != nil {
		t.Fatalf("write source binary: %v", err)
	}
	restore := overrideDeployGlobals(t, sourceBinary)
	defer restore()
	deployRunSystemctl = func(_ context.Context, _ string, _ ...string) error { return nil }

	c := newDeployInstallTestCmd()
	if err := c.ParseFlags([]string{"--legacy-serve", "--scope=user", "--systemd", "--name=workbuddy"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	var stdout bytes.Buffer
	c.SetOut(&stdout)
	if err := runDeployInstallCmd(c, c.Flags().Args()); err != nil {
		t.Fatalf("runDeployInstallCmd: %v", err)
	}

	manifestPath := filepath.Join(configDir, "workbuddy", "deployments", "workbuddy.json")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("expected legacy manifest %s, err=%v", manifestPath, err)
	}
	unitPath := filepath.Join(configDir, "systemd", "user", "workbuddy.service")
	unitBytes, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read legacy unit: %v", err)
	}
	if !strings.Contains(string(unitBytes), `"serve"`) {
		t.Errorf("legacy unit missing serve ExecStart fragment:\n%s", string(unitBytes))
	}
	// And the bundle units must NOT exist.
	for _, name := range []string{bundleSupervisorName, bundleCoordinatorName, bundleWorkerName} {
		manifestPath := filepath.Join(configDir, "workbuddy", "deployments", name+".json")
		if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
			t.Errorf("bundle manifest %s should not exist with --legacy-serve: err=%v", manifestPath, err)
		}
	}
}

// TestRunDeployInstallCmdRejectsTrailingArgsByDefault makes sure that the new
// default install path refuses trailing -- args (which only made sense for
// the legacy single-process install) instead of silently ignoring them.
func TestRunDeployInstallCmdRejectsTrailingArgsByDefault(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}
	tempDir := t.TempDir()
	t.Setenv("HOME", filepath.Join(tempDir, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "xdg"))
	t.Chdir(tempDir)

	sourceBinary := filepath.Join(tempDir, "current-workbuddy")
	if err := os.WriteFile(sourceBinary, []byte("x"), 0o755); err != nil {
		t.Fatalf("write source binary: %v", err)
	}
	restore := overrideDeployGlobals(t, sourceBinary)
	defer restore()
	deployRunSystemctl = func(_ context.Context, _ string, _ ...string) error { return nil }

	c := newDeployInstallTestCmd()
	if err := c.ParseFlags([]string{"--scope=user"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	// runDeployInstallCmd takes trailing -- args as the second argument
	// directly (cobra normally extracts them via Args). Pass them in
	// explicitly to exercise the trailing-args guard.
	err := runDeployInstallCmd(c, []string{"coordinator", "--listen", "127.0.0.1:8081"})
	if err == nil || !strings.Contains(err.Error(), "trailing -- args are not supported") {
		t.Fatalf("expected trailing-args error, got %v", err)
	}
}

// TestRunDeployUpgradeCmdRefusesLegacyServeWithoutOptIn covers AC-4: deploy
// upgrade against a legacy single-process serve install must error out unless
// the caller passes --legacy-serve, so layout drift is always explicit.
func TestRunDeployUpgradeCmdRefusesLegacyServeWithoutOptIn(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}
	tempDir := t.TempDir()
	homeDir := filepath.Join(tempDir, "home")
	configDir := filepath.Join(tempDir, "xdg")
	repoDir := filepath.Join(tempDir, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Chdir(repoDir)

	deployedBinary := filepath.Join(homeDir, ".local", "bin", "workbuddy")
	if err := os.MkdirAll(filepath.Dir(deployedBinary), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(deployedBinary, []byte("legacy"), 0o755); err != nil {
		t.Fatalf("write deployed binary: %v", err)
	}
	writeScopedManifest(t, "user", "workbuddy", &deploymentManifest{
		BinaryPath:       deployedBinary,
		WorkingDirectory: repoDir,
		Command:          []string{"serve"},
		Systemd: &deploymentSystemd{
			ServiceName: "workbuddy",
			Description: "legacy serve",
		},
	})

	c := newDeployUpgradeTestCmd()
	if err := c.ParseFlags([]string{"--name=workbuddy", "--scope=user", "--version=v0.5.0"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	err := runDeployUpgradeCmd(c, nil)
	if err == nil || !strings.Contains(err.Error(), "legacy single-process") {
		t.Fatalf("expected legacy-serve refusal, got %v", err)
	}
}

func TestRunDeployInstallEnableUpdaterRejectsBadInterval(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd deployment is only supported on Linux")
	}
	tempDir := t.TempDir()
	t.Setenv("HOME", filepath.Join(tempDir, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "xdg"))
	t.Chdir(tempDir)

	sourceBinary := filepath.Join(tempDir, "current-workbuddy")
	if err := os.WriteFile(sourceBinary, []byte("x"), 0o755); err != nil {
		t.Fatalf("write source binary: %v", err)
	}
	restore := overrideDeployGlobals(t, sourceBinary)
	defer restore()
	deployRunSystemctl = func(_ context.Context, _ string, _ ...string) error { return nil }

	err := runDeployInstallWithOpts(context.Background(), &deployInstallOpts{
		name:            "workbuddy",
		scope:           "user",
		enableUpdater:   true,
		updaterRepo:     "Lincyaw/workbuddy",
		updaterInterval: "bogus",
		updaterName:     "workbuddy-updater",
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "invalid --updater-interval") {
		t.Fatalf("expected invalid --updater-interval error, got %v", err)
	}
}
