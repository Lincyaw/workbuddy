package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeChecksums(entries map[string][]byte) []byte {
	var b strings.Builder
	for name, data := range entries {
		sum := sha256.Sum256(data)
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	return []byte(b.String())
}

func newReleaseServer(t *testing.T, repo, version string, archive, checksums []byte) *httptest.Server {
	t.Helper()
	archiveName := releaseArchiveName(version)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + repo + "/releases/latest":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"tag_name":"v%s"}`, version)
		case "/" + repo + "/releases/download/v" + version + "/" + archiveName:
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(archive)
		case "/" + repo + "/releases/download/v" + version + "/" + updaterChecksumName:
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write(checksums)
		default:
			http.NotFound(w, r)
		}
	}))
	return server
}

func setupWatchTestEnv(t *testing.T, server *httptest.Server) func() {
	t.Helper()
	oldClient := deployHTTPClient
	oldAPI := deployGitHubAPIBaseURL
	oldDownload := deployGitHubDownloadBase
	oldSystemctl := deployRunSystemctl
	deployHTTPClient = server.Client()
	deployGitHubAPIBaseURL = server.URL
	deployGitHubDownloadBase = server.URL
	return func() {
		deployHTTPClient = oldClient
		deployGitHubAPIBaseURL = oldAPI
		deployGitHubDownloadBase = oldDownload
		deployRunSystemctl = oldSystemctl
	}
}

func TestSemverGreater(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"1.2.3", "", true},
		{"1.2.3", "v1.2.2", true},
		{"1.2.3", "v1.2.3", false},
		{"1.2.3", "v1.3.0", false},
		{"1.10.0", "v1.9.9", true},
		{"2.0.0", "v1.99.99", true},
		{"1.2.3-rc1", "v1.2.3", false},
	}
	for _, c := range cases {
		if got := semverGreater(c.latest, c.current); got != c.want {
			t.Errorf("semverGreater(%q,%q)=%v want %v", c.latest, c.current, got, c.want)
		}
	}
}

func TestLookupChecksum(t *testing.T) {
	data := []byte("aabbcc  workbuddy_1.2.3_linux_amd64.tar.gz\n" +
		"ffeedd  *workbuddy_1.2.3_linux_arm64.tar.gz\n" +
		"\n" +
		"# comment\n")
	if sum, err := lookupChecksum(data, "workbuddy_1.2.3_linux_amd64.tar.gz"); err != nil || sum != "aabbcc" {
		t.Fatalf("amd64: sum=%q err=%v", sum, err)
	}
	if sum, err := lookupChecksum(data, "workbuddy_1.2.3_linux_arm64.tar.gz"); err != nil || sum != "ffeedd" {
		t.Fatalf("arm64: sum=%q err=%v", sum, err)
	}
	if _, err := lookupChecksum(data, "missing.tar.gz"); err == nil {
		t.Fatalf("expected error for missing entry")
	}
}

func TestRunDeployWatchOnce_NoOpWhenAtLatest(t *testing.T) {
	tempDir := t.TempDir()
	stateDir := filepath.Join(tempDir, "updater")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, updaterCurrentVersionFile), []byte("v1.2.3\n"), 0o644); err != nil {
		t.Fatalf("write current: %v", err)
	}

	archive := buildReleaseArchive(t, "ignored")
	server := newReleaseServer(t, "acme/workbuddy", "1.2.3", archive,
		writeChecksums(map[string][]byte{releaseArchiveName("1.2.3"): archive}))
	defer server.Close()
	defer setupWatchTestEnv(t, server)()

	deployRunSystemctl = func(_ context.Context, _ string, _ ...string) error {
		t.Fatal("systemctl should not be invoked when up-to-date")
		return nil
	}

	binaryPath := filepath.Join(tempDir, "bin", "workbuddy")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(binaryPath, []byte("old"), 0o755); err != nil {
		t.Fatalf("write old: %v", err)
	}

	var stdout bytes.Buffer
	err := runDeployWatchOnce(context.Background(), &deployWatchOpts{
		repository:     "acme/workbuddy",
		stateDir:       stateDir,
		binaryPath:     binaryPath,
		systemctlScope: "user",
	}, &stdout)
	if err != nil {
		t.Fatalf("runDeployWatchOnce: %v", err)
	}
	if !strings.Contains(stdout.String(), "up-to-date") {
		t.Fatalf("expected up-to-date message, got %q", stdout.String())
	}
	if got, _ := os.ReadFile(binaryPath); string(got) != "old" {
		t.Fatalf("binary unexpectedly modified: %q", got)
	}
}

func TestRunDeployWatchOnce_DownloadsAndRestarts(t *testing.T) {
	tempDir := t.TempDir()
	stateDir := filepath.Join(tempDir, "updater")
	binaryPath := filepath.Join(tempDir, "bin", "workbuddy")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(binaryPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("write old: %v", err)
	}

	archive := buildReleaseArchive(t, "release-binary")
	server := newReleaseServer(t, "acme/workbuddy", "1.2.3", archive,
		writeChecksums(map[string][]byte{releaseArchiveName("1.2.3"): archive}))
	defer server.Close()
	defer setupWatchTestEnv(t, server)()

	var systemctlCalls []string
	deployRunSystemctl = func(_ context.Context, scope string, args ...string) error {
		systemctlCalls = append(systemctlCalls, scope+":"+strings.Join(args, " "))
		return nil
	}

	var stdout bytes.Buffer
	err := runDeployWatchOnce(context.Background(), &deployWatchOpts{
		repository:     "acme/workbuddy",
		stateDir:       stateDir,
		binaryPath:     binaryPath,
		systemctlScope: "user",
		restartUnits:   []string{"workbuddy-coordinator.service", "workbuddy-worker.service"},
	}, &stdout)
	if err != nil {
		t.Fatalf("runDeployWatchOnce: %v", err)
	}

	if got, _ := os.ReadFile(binaryPath); string(got) != "release-binary" {
		t.Fatalf("binary not upgraded: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(stateDir, updaterPreviousBinaryFile)); string(got) != "old-binary" {
		t.Fatalf("previous-binary backup wrong: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(stateDir, updaterCurrentVersionFile)); strings.TrimSpace(string(got)) != "v1.2.3" {
		t.Fatalf("current-version not updated: %q", got)
	}
	want := "user:restart workbuddy-coordinator.service | user:restart workbuddy-worker.service"
	if got := strings.Join(systemctlCalls, " | "); got != want {
		t.Fatalf("systemctl calls = %q, want %q", got, want)
	}
}

func TestRunDeployWatchOnce_ChecksumMismatchPreservesBinary(t *testing.T) {
	tempDir := t.TempDir()
	stateDir := filepath.Join(tempDir, "updater")
	binaryPath := filepath.Join(tempDir, "bin", "workbuddy")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(binaryPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("write old: %v", err)
	}

	archive := buildReleaseArchive(t, "release-binary")
	bogus := []byte(strings.Repeat("00", 32) + "  " + releaseArchiveName("1.2.3") + "\n")
	server := newReleaseServer(t, "acme/workbuddy", "1.2.3", archive, bogus)
	defer server.Close()
	defer setupWatchTestEnv(t, server)()

	deployRunSystemctl = func(_ context.Context, _ string, _ ...string) error {
		t.Fatal("systemctl should not run on checksum mismatch")
		return nil
	}

	var stdout bytes.Buffer
	err := runDeployWatchOnce(context.Background(), &deployWatchOpts{
		repository:     "acme/workbuddy",
		stateDir:       stateDir,
		binaryPath:     binaryPath,
		systemctlScope: "user",
	}, &stdout)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch error, got %v", err)
	}
	if got, _ := os.ReadFile(binaryPath); string(got) != "old-binary" {
		t.Fatalf("binary should be untouched, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(stateDir, updaterCurrentVersionFile)); !os.IsNotExist(err) {
		t.Fatalf("current-version should not be written on failure: err=%v", err)
	}
}

func TestRunDeployWatchOnce_DryRun(t *testing.T) {
	tempDir := t.TempDir()
	stateDir := filepath.Join(tempDir, "updater")
	binaryPath := filepath.Join(tempDir, "bin", "workbuddy")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(binaryPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("write old: %v", err)
	}

	archive := buildReleaseArchive(t, "release-binary")
	server := newReleaseServer(t, "acme/workbuddy", "1.2.3", archive,
		writeChecksums(map[string][]byte{releaseArchiveName("1.2.3"): archive}))
	defer server.Close()
	defer setupWatchTestEnv(t, server)()

	deployRunSystemctl = func(_ context.Context, _ string, _ ...string) error {
		t.Fatal("dry-run must not invoke systemctl")
		return nil
	}

	var stdout bytes.Buffer
	err := runDeployWatchOnce(context.Background(), &deployWatchOpts{
		repository:     "acme/workbuddy",
		stateDir:       stateDir,
		binaryPath:     binaryPath,
		systemctlScope: "user",
		dryRun:         true,
	}, &stdout)
	if err != nil {
		t.Fatalf("runDeployWatchOnce: %v", err)
	}
	if !strings.Contains(stdout.String(), "dry-run") {
		t.Fatalf("expected dry-run log, got %q", stdout.String())
	}
	if got, _ := os.ReadFile(binaryPath); string(got) != "old-binary" {
		t.Fatalf("binary should be untouched in dry-run, got %q", got)
	}
}

func TestRunDeployRollback_SwapsBinaryAndRestarts(t *testing.T) {
	tempDir := t.TempDir()
	stateDir := filepath.Join(tempDir, "updater")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	binaryPath := filepath.Join(tempDir, "bin", "workbuddy")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(binaryPath, []byte("current-binary"), 0o755); err != nil {
		t.Fatalf("write current: %v", err)
	}
	previous := filepath.Join(stateDir, updaterPreviousBinaryFile)
	if err := os.WriteFile(previous, []byte("previous-binary"), 0o755); err != nil {
		t.Fatalf("write previous: %v", err)
	}

	oldSystemctl := deployRunSystemctl
	t.Cleanup(func() { deployRunSystemctl = oldSystemctl })
	var calls []string
	deployRunSystemctl = func(_ context.Context, scope string, args ...string) error {
		calls = append(calls, scope+":"+strings.Join(args, " "))
		return nil
	}

	var stdout bytes.Buffer
	err := runDeployRollback(context.Background(), &deployRollbackOpts{
		stateDir:       stateDir,
		binaryPath:     binaryPath,
		systemctlScope: "user",
		restartUnits:   []string{"workbuddy-coordinator.service"},
	}, &stdout)
	if err != nil {
		t.Fatalf("runDeployRollback: %v", err)
	}
	if got, _ := os.ReadFile(binaryPath); string(got) != "previous-binary" {
		t.Fatalf("binary should hold previous-binary after rollback, got %q", got)
	}
	if got, _ := os.ReadFile(previous); string(got) != "current-binary" {
		t.Fatalf("previous-binary should now hold prior current, got %q", got)
	}
	want := "user:restart workbuddy-coordinator.service"
	if got := strings.Join(calls, " | "); got != want {
		t.Fatalf("systemctl calls = %q, want %q", got, want)
	}
}

func TestRunDeployRollback_NoBackup(t *testing.T) {
	tempDir := t.TempDir()
	stateDir := filepath.Join(tempDir, "updater")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	err := runDeployRollback(context.Background(), &deployRollbackOpts{
		stateDir:       stateDir,
		binaryPath:     filepath.Join(tempDir, "bin", "workbuddy"),
		systemctlScope: "user",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no previous binary") {
		t.Fatalf("expected no previous binary error, got %v", err)
	}
}

func TestParseDeployWatchFlags_Validation(t *testing.T) {
	cmd := deployWatchCmd
	// Restore flag values after the subtest.
	t.Cleanup(func() {
		_ = cmd.Flags().Set("repo", "")
		_ = cmd.Flags().Set("interval", defaultWatchInterval.String())
	})
	_ = cmd.Flags().Set("repo", "")
	if _, err := parseDeployWatchFlags(cmd); err == nil {
		t.Fatalf("expected --repo required error")
	}
	_ = cmd.Flags().Set("repo", "no-slash")
	if _, err := parseDeployWatchFlags(cmd); err == nil {
		t.Fatalf("expected owner/name error")
	}
	_ = cmd.Flags().Set("repo", "acme/workbuddy")
	_ = cmd.Flags().Set("interval", "0s")
	if _, err := parseDeployWatchFlags(cmd); err == nil {
		t.Fatalf("expected positive interval error")
	}
}

// Sanity: assert releaseArchiveName follows the expected layout the watcher
// computes URLs from. If goreleaser config diverges, this guards us.
func TestReleaseArchiveNameLayout(t *testing.T) {
	want := fmt.Sprintf("workbuddy_1.2.3_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	if got := releaseArchiveName("1.2.3"); got != want {
		t.Fatalf("releaseArchiveName = %q, want %q", got, want)
	}
}
