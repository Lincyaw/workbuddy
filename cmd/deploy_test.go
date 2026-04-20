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
	"runtime"
	"strings"
	"testing"
	"time"
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
	err := runDeployStopWithOpts(context.Background(), &deployLookupOpts{name: "demo", scope: "user"}, &stdout)
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
	err := runDeployStopWithOpts(context.Background(), &deployLookupOpts{name: "demo", scope: "user"}, &stdout)
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
	err := runDeployDeleteWithOpts(context.Background(), &deployLookupOpts{name: "demo", scope: "user"}, &stdout)
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
	err := runDeployDeleteWithOpts(context.Background(), &deployLookupOpts{name: "demo", scope: "user"}, &stdout)
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
