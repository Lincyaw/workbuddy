package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestRunDeployBundleInstallWritesAllThreeUnits exercises
// `deploy install --bundle` end-to-end against a temp HOME and verifies all
// three units are written, the supervisor uses Type=notify+KillMode=process,
// and systemctl daemon-reload/enable/start runs for each.
func TestRunDeployBundleInstallWritesAllThreeUnits(t *testing.T) {
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
	if err := os.WriteFile(sourceBinary, []byte("binary-v05"), 0o755); err != nil {
		t.Fatalf("write source binary: %v", err)
	}
	restore := overrideDeployGlobals(t, sourceBinary)
	defer restore()

	var systemctlCalls []string
	deployRunSystemctl = func(_ context.Context, scope string, args ...string) error {
		systemctlCalls = append(systemctlCalls, scope+":"+strings.Join(args, " "))
		return nil
	}
	deployNow = func() time.Time { return time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC) }

	var stdout bytes.Buffer
	err := runDeployBundleInstallWithOpts(context.Background(), &bundleInstallOpts{
		scope:           "user",
		envFiles:        []string{"/etc/workbuddy/bundle.env"},
		enable:          true,
		start:           true,
		coordinatorArgs: []string{"--listen", "127.0.0.1:8081"},
		workerArgs:      []string{"--coordinator", "http://127.0.0.1:8081", "--token", "secret", "--role", "dev"},
	}, &stdout)
	if err != nil {
		t.Fatalf("runDeployBundleInstallWithOpts: %v", err)
	}

	type unitExpect struct {
		name           string
		typeLine       string
		killModeLine   string
		restartLine    string
		afterContains  string
		execContains   string
	}
	expectations := []unitExpect{
		{
			name:         bundleSupervisorName,
			typeLine:     "Type=notify",
			killModeLine: "KillMode=process",
			restartLine:  "Restart=always",
			execContains: `"supervisor"`,
		},
		{
			name:          bundleCoordinatorName,
			typeLine:      "Type=simple",
			restartLine:   "Restart=on-failure",
			afterContains: "workbuddy-supervisor.service",
			execContains:  `"coordinator" "--listen" "127.0.0.1:8081"`,
		},
		{
			name:          bundleWorkerName,
			typeLine:      "Type=simple",
			restartLine:   "Restart=on-failure",
			afterContains: "workbuddy-supervisor.service",
			execContains:  `"worker" "--coordinator"`,
		},
	}
	for _, exp := range expectations {
		unitPath := filepath.Join(configDir, "systemd", "user", exp.name+".service")
		raw, err := os.ReadFile(unitPath)
		if err != nil {
			t.Fatalf("read %s: %v", unitPath, err)
		}
		unit := string(raw)
		if !strings.Contains(unit, exp.typeLine) {
			t.Errorf("%s missing %q:\n%s", exp.name, exp.typeLine, unit)
		}
		if !strings.Contains(unit, exp.restartLine) {
			t.Errorf("%s missing %q:\n%s", exp.name, exp.restartLine, unit)
		}
		if exp.killModeLine != "" && !strings.Contains(unit, exp.killModeLine) {
			t.Errorf("%s missing %q:\n%s", exp.name, exp.killModeLine, unit)
		}
		if exp.killModeLine == "" && strings.Contains(unit, "KillMode=") {
			t.Errorf("%s should not have KillMode=:\n%s", exp.name, unit)
		}
		if exp.afterContains != "" && !strings.Contains(unit, exp.afterContains) {
			t.Errorf("%s missing After= ordering %q:\n%s", exp.name, exp.afterContains, unit)
		}
		if !strings.Contains(unit, exp.execContains) {
			t.Errorf("%s missing ExecStart fragment %q:\n%s", exp.name, exp.execContains, unit)
		}
		if !strings.Contains(unit, "EnvironmentFile=/etc/workbuddy/bundle.env") {
			t.Errorf("%s missing EnvironmentFile:\n%s", exp.name, unit)
		}
		manifestPath := filepath.Join(configDir, "workbuddy", "deployments", exp.name+".json")
		if _, err := os.Stat(manifestPath); err != nil {
			t.Errorf("%s manifest missing: %v", exp.name, err)
		}
	}

	calls := strings.Join(systemctlCalls, " | ")
	for _, name := range []string{bundleSupervisorName, bundleCoordinatorName, bundleWorkerName} {
		if !strings.Contains(calls, "user:enable "+name+".service") {
			t.Errorf("missing enable call for %s; calls=%s", name, calls)
		}
		if !strings.Contains(calls, "user:start "+name+".service") {
			t.Errorf("missing start call for %s; calls=%s", name, calls)
		}
	}
	if !strings.Contains(stdout.String(), "bundle install complete (3 units)") {
		t.Errorf("stdout missing summary: %q", stdout.String())
	}
}

func TestRunDeployBundleUninstallRemovesUnitsAndState(t *testing.T) {
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
	if err := os.WriteFile(sourceBinary, []byte("binary-v05"), 0o755); err != nil {
		t.Fatalf("write source binary: %v", err)
	}
	restore := overrideDeployGlobals(t, sourceBinary)
	defer restore()
	deployRunSystemctl = func(_ context.Context, _ string, _ ...string) error { return nil }

	var stdout bytes.Buffer
	if err := runDeployBundleInstallWithOpts(context.Background(), &bundleInstallOpts{
		scope:  "user",
		enable: true,
		start:  true,
	}, &stdout); err != nil {
		t.Fatalf("install: %v", err)
	}

	stateDir := filepath.Join(tempDir, "state")
	if err := os.MkdirAll(filepath.Join(stateDir, "agents"), 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	socketPath := filepath.Join(tempDir, "supervisor.sock")
	if err := os.WriteFile(socketPath, []byte{}, 0o600); err != nil {
		t.Fatalf("write socket placeholder: %v", err)
	}

	stdout.Reset()
	err := runDeployBundleUninstallWithOpts(context.Background(), &bundleUninstallOpts{
		scope:    "user",
		force:    true,
		stateDir: stateDir,
		socket:   socketPath,
	}, &stdout)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	for _, name := range []string{bundleSupervisorName, bundleCoordinatorName, bundleWorkerName} {
		manifestPath := filepath.Join(configDir, "workbuddy", "deployments", name+".json")
		if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
			t.Errorf("manifest %s should be removed: err=%v", manifestPath, err)
		}
		unitPath := filepath.Join(configDir, "systemd", "user", name+".service")
		if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
			t.Errorf("unit %s should be removed: err=%v", unitPath, err)
		}
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Errorf("state dir should be removed: err=%v", err)
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Errorf("socket should be removed: err=%v", err)
	}
}

func TestEnsureSupervisorUnitForUpgradeAddsMissingSupervisor(t *testing.T) {
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
	if err := os.WriteFile(sourceBinary, []byte("binary-v05"), 0o755); err != nil {
		t.Fatalf("write source binary: %v", err)
	}
	restore := overrideDeployGlobals(t, sourceBinary)
	defer restore()
	deployRunSystemctl = func(_ context.Context, _ string, _ ...string) error { return nil }

	// Seed a v0.4-style install: only coordinator + worker manifests.
	deployedBinary := filepath.Join(homeDir, ".local", "bin", "workbuddy")
	if err := os.MkdirAll(filepath.Dir(deployedBinary), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(deployedBinary, []byte("binary-v04"), 0o755); err != nil {
		t.Fatalf("write deployed binary: %v", err)
	}
	for _, name := range []string{bundleCoordinatorName, bundleWorkerName} {
		writeScopedManifest(t, "user", name, &deploymentManifest{
			BinaryPath:       deployedBinary,
			WorkingDirectory: repoDir,
			Command:          []string{strings.TrimPrefix(name, "workbuddy-")},
			Systemd: &deploymentSystemd{
				ServiceName:      name,
				Description:      "legacy",
				EnvironmentFiles: []string{"/etc/workbuddy/bundle.env"},
			},
		})
	}

	var stdout bytes.Buffer
	if err := ensureSupervisorUnitForUpgrade(context.Background(), "user", &stdout); err != nil {
		t.Fatalf("ensureSupervisorUnitForUpgrade: %v", err)
	}

	supervisorManifest := filepath.Join(configDir, "workbuddy", "deployments", bundleSupervisorName+".json")
	if _, err := os.Stat(supervisorManifest); err != nil {
		t.Fatalf("supervisor manifest not created: %v", err)
	}
	supervisorUnit := filepath.Join(configDir, "systemd", "user", bundleSupervisorName+".service")
	raw, err := os.ReadFile(supervisorUnit)
	if err != nil {
		t.Fatalf("supervisor unit not created: %v", err)
	}
	if !strings.Contains(string(raw), "Type=notify") {
		t.Errorf("supervisor unit missing Type=notify:\n%s", string(raw))
	}
	if !strings.Contains(string(raw), "KillMode=process") {
		t.Errorf("supervisor unit missing KillMode=process:\n%s", string(raw))
	}
	if !strings.Contains(string(raw), "EnvironmentFile=/etc/workbuddy/bundle.env") {
		t.Errorf("supervisor unit missing seeded EnvironmentFile:\n%s", string(raw))
	}
	if !strings.Contains(stdout.String(), "v0.4 install detected") {
		t.Errorf("stdout missing detection message: %q", stdout.String())
	}

	// Calling again is a no-op (supervisor already present).
	stdout.Reset()
	if err := ensureSupervisorUnitForUpgrade(context.Background(), "user", &stdout); err != nil {
		t.Fatalf("idempotent call: %v", err)
	}
	if strings.Contains(stdout.String(), "v0.4 install detected") {
		t.Errorf("expected no-op on idempotent call; got: %q", stdout.String())
	}
}
