package cmd

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/app"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/spf13/cobra"
)

func setupTestConfigDir(t *testing.T, repo string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), fmt.Sprintf("repo: %s\npoll_interval: 1s\nport: 0\n", repo))
	writeFile(t, filepath.Join(dir, "agents", "dev-agent.md"), `---
name: dev-agent
description: Dev agent
triggers:
  - state: developing
role: dev
runtime: claude-code
command: echo "hello"
timeout: 30s
context:
  - Repo
---
Repo: {{.Repo}}
`)
	writeFile(t, filepath.Join(dir, "workflows", "dev-workflow.md"), `---
name: dev-workflow
description: Dev workflow
trigger:
  issue_label: "workbuddy"
max_retries: 3
---
# Dev Workflow

`+"```yaml\nstates:\n  triage:\n    enter_label: \"status:triage\"\n    transitions:\n      \"status:developing\": developing\n  developing:\n    enter_label: \"status:developing\"\n    agent: dev-agent\n    transitions:\n      \"status:done\": done\n  done:\n    enter_label: \"status:done\"\n```\n")
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitForHealth(t *testing.T, port int) {
	t.Helper()
	addr := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(addr)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", addr)
}

func getFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

type mockGHReader struct {
	mu             sync.Mutex
	issues         []poller.Issue
	prs            []poller.PR
	labelSnapshots [][]string
	labelCalls     int
}

func (m *mockGHReader) ListIssues(string) ([]poller.Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]poller.Issue(nil), m.issues...), nil
}

func (m *mockGHReader) ListPRs(string) ([]poller.PR, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]poller.PR(nil), m.prs...), nil
}

func (m *mockGHReader) CheckRepoAccess(string) error { return nil }

func (m *mockGHReader) ReadIssueLabels(_ string, _ int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.labelSnapshots) == 0 {
		return []string{"workbuddy", "status:developing"}, nil
	}
	idx := m.labelCalls
	if idx >= len(m.labelSnapshots) {
		idx = len(m.labelSnapshots) - 1
	}
	m.labelCalls++
	return append([]string(nil), m.labelSnapshots[idx]...), nil
}

func (m *mockGHReader) ReadIssue(_ string, issueNum int) (poller.IssueDetails, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, issue := range m.issues {
		if issue.Number == issueNum {
			return poller.IssueDetails{Number: issueNum, State: issue.State, Body: issue.Body, Labels: append([]string(nil), issue.Labels...)}, nil
		}
	}
	return poller.IssueDetails{Number: issueNum, State: "open", Labels: []string{"workbuddy", "status:developing"}}, nil
}

func setupFakeGHCLI(t *testing.T) {
	t.Helper()
	fakeBin := t.TempDir()
	ghPath := filepath.Join(fakeBin, "gh")
	writeFile(t, ghPath, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(ghPath, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func newServeFlagCommand(t *testing.T, configDir string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "serve"}
	cmd.Flags().String("listen", "", "")
	cmd.Flags().Int("port", defaultPort, "")
	cmd.Flags().Duration("poll-interval", defaultPollInterval, "")
	cmd.Flags().Int("max-parallel-tasks", 0, "")
	cmd.Flags().StringSlice("roles", []string{"dev"}, "")
	cmd.Flags().String("config-dir", configDir, "")
	cmd.Flags().String("db-path", ".workbuddy/workbuddy.db", "")
	cmd.Flags().Bool("loopback-only", false, "")
	cmd.Flags().Bool("auth", false, "")
	cmd.Flags().String("trusted-authors", "", "")
	cmd.Flags().String("report-base-url", "", "")
	return cmd
}

func TestParseServeFlagsRejectsNonLoopbackListenWithoutAuth(t *testing.T) {
	configDir := setupTestConfigDir(t, "owner/repo")
	cmd := newServeFlagCommand(t, configDir)
	_ = cmd.Flags().Set("listen", "0.0.0.0:8090")

	opts, err := parseServeFlags(cmd)
	if err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	cfg, err := loadServeConfig(opts)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	listenAddr, err := resolveServeListenAddr(opts, cfg)
	if err != nil {
		t.Fatalf("resolve listen addr: %v", err)
	}
	if err := validateServeListenSecurity(listenAddr, opts.auth, opts.loopbackOnly); err == nil {
		t.Fatal("expected non-loopback listen without auth to fail")
	}
}

func TestServeRejectsNonLoopbackWithoutBaseURL(t *testing.T) {
	t.Setenv("WORKBUDDY_REPORT_BASE_URL", "")
	configDir := setupTestConfigDir(t, "owner/repo")
	cmd := newServeFlagCommand(t, configDir)
	_ = cmd.Flags().Set("listen", "0.0.0.0:8090")

	opts, err := parseServeFlags(cmd)
	if err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	cfg, err := loadServeConfig(opts)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	listenAddr, err := resolveServeListenAddr(opts, cfg)
	if err != nil {
		t.Fatalf("resolve listen addr: %v", err)
	}
	_, err = resolveReportBaseURL("serve", listenAddr, opts.reportBaseURL, "")
	if err == nil {
		t.Fatal("expected non-loopback bind without --report-base-url to fail")
	}
	if !strings.Contains(err.Error(), "--report-base-url is missing") {
		t.Fatalf("error = %q, want missing-url diagnostic", err.Error())
	}
}

func TestServeAcceptsLoopbackWithoutBaseURL(t *testing.T) {
	t.Setenv("WORKBUDDY_REPORT_BASE_URL", "")
	configDir := setupTestConfigDir(t, "owner/repo")
	cmd := newServeFlagCommand(t, configDir)
	_ = cmd.Flags().Set("listen", "127.0.0.1:8090")

	opts, err := parseServeFlags(cmd)
	if err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	cfg, err := loadServeConfig(opts)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	listenAddr, err := resolveServeListenAddr(opts, cfg)
	if err != nil {
		t.Fatalf("resolve listen addr: %v", err)
	}
	got, err := resolveReportBaseURL("serve", listenAddr, opts.reportBaseURL, "")
	if err != nil {
		t.Fatalf("loopback bind without --report-base-url rejected: %v", err)
	}
	if want := "http://" + listenAddr; got != want {
		t.Fatalf("got = %q, want default %q", got, want)
	}
}

func TestServeAcceptsNonLoopbackWithExternalBaseURL(t *testing.T) {
	t.Setenv("WORKBUDDY_REPORT_BASE_URL", "")
	configDir := setupTestConfigDir(t, "owner/repo")
	cmd := newServeFlagCommand(t, configDir)
	_ = cmd.Flags().Set("listen", "0.0.0.0:8090")
	_ = cmd.Flags().Set("report-base-url", "http://example.com:8090")

	opts, err := parseServeFlags(cmd)
	if err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	cfg, err := loadServeConfig(opts)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	listenAddr, err := resolveServeListenAddr(opts, cfg)
	if err != nil {
		t.Fatalf("resolve listen addr: %v", err)
	}
	got, err := resolveReportBaseURL("serve", listenAddr, opts.reportBaseURL, "")
	if err != nil {
		t.Fatalf("non-loopback bind with external --report-base-url rejected: %v", err)
	}
	if want := "http://example.com:8090"; got != want {
		t.Fatalf("got = %q, want %q", got, want)
	}
}

func TestServeAuthWrapsNonHealthRoutes(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "serve.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	api := &app.FullCoordinatorServer{
		AuthEnabled: true,
		AuthToken:   "serve-secret",
	}
	mux := buildCoordinatorMux(api, st, eventlog.NewEventLogger(st), filepath.Join(t.TempDir(), "serve.db"), nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := srv.Client()
	resp, err := client.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("get metrics: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("metrics status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	_ = resp.Body.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/metrics", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer serve-secret")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("authorized metrics: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorized metrics status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	_ = resp.Body.Close()

	sessReq, err := http.NewRequest(http.MethodGet, srv.URL+"/sessions", nil)
	if err != nil {
		t.Fatalf("new sessions request: %v", err)
	}
	resp, err = client.Do(sessReq)
	if err != nil {
		t.Fatalf("sessions request: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sessions status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	_ = resp.Body.Close()
}
