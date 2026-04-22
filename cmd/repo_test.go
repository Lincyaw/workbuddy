package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestRunRepoList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			t.Fatalf("authorization = %q", auth)
		}
		_, _ = fmt.Fprint(w, mustJSON(t, []repoStatusResponse{
			{Repo: "owner/a", Environment: "prod", Status: "active", PollerStatus: "running", RegisteredAt: time.Now(), UpdatedAt: time.Now()},
			{Repo: "owner/b", Environment: "dev", Status: "active", PollerStatus: "stopped", RegisteredAt: time.Now(), UpdatedAt: time.Now()},
		}))
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := runRepoList(context.Background(), &repoListOpts{
		coordinator: srv.URL,
		token:       "test-token",
	}, &out)
	if err != nil {
		t.Fatalf("runRepoList: %v", err)
	}
	got := out.String()
	for _, want := range []string{"owner/a", "owner/b", "prod", "dev", "running", "stopped"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRunRepoList_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `[{"repo":"owner/a","status":"active","poller_status":"running"}]`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := runRepoList(context.Background(), &repoListOpts{
		coordinator: srv.URL,
		format:      outputFormatJSON,
	}, &out)
	if err != nil {
		t.Fatalf("runRepoList: %v", err)
	}
	var repos []repoStatusResponse
	if err := json.Unmarshal(out.Bytes(), &repos); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(repos) != 1 || repos[0].Repo != "owner/a" {
		t.Fatalf("unexpected repos: %+v", repos)
	}
}

func TestRunRepoList_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"error":"forbidden"}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := runRepoList(context.Background(), &repoListOpts{
		coordinator: srv.URL,
	}, &out)
	var exitErr *cliExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("expected exit error with code 1, got %v", err)
	}
	if !strings.Contains(exitErr.Error(), "403") {
		t.Fatalf("expected 403 in error, got %q", exitErr.Error())
	}
}

func TestRunRepoList_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := runRepoList(context.Background(), &repoListOpts{
		coordinator: srv.URL,
	}, &out)
	if err != nil {
		t.Fatalf("runRepoList: %v", err)
	}
	if strings.TrimSpace(out.String()) != "No repos found." {
		t.Fatalf("unexpected empty output: %q", out.String())
	}
}

func TestParseRepoListFlags_TokenFile(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token.txt")
	if err := os.WriteFile(tokenPath, []byte("file-token\n"), 0o644); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	cmd := &cobra.Command{Use: "list"}
	cmd.Flags().String("coordinator", "", "")
	addCoordinatorAuthFlags(cmd.Flags(), "t", "Bearer token for coordinator auth")
	addOutputFormatFlag(cmd)
	addDeprecatedJSONAliasFlag(cmd)
	cmd.Flags().Duration("timeout", 15*time.Second, "")
	if err := cmd.Flags().Set("coordinator", "http://coord:8081"); err != nil {
		t.Fatalf("set coordinator: %v", err)
	}
	if err := cmd.Flags().Set("token-file", tokenPath); err != nil {
		t.Fatalf("set token-file: %v", err)
	}

	opts, err := parseRepoListFlags(cmd)
	if err != nil {
		t.Fatalf("parseRepoListFlags: %v", err)
	}
	if got, want := opts.token, "file-token"; got != want {
		t.Fatalf("token = %q, want %q", got, want)
	}
}

func TestRunRepoRegisterCmd_JSON(t *testing.T) {
	configDir := writeValidateFixture(t, validateFixtureFiles{
		"config.yaml":            "repo: octo/workbuddy\nenvironment: dev\npoll_interval: 45s\n",
		"agents/dev-agent.md":    validateAgentFixture("dev-agent", "status:developing"),
		"agents/review-agent.md": validateAgentFixture("review-agent", "status:reviewing"),
		"workflows/default.md":   validateWorkflowFixture(),
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/repos/register" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var out bytes.Buffer
	repoRegisterCmd.SetOut(&out)
	repoRegisterCmd.SetErr(&out)
	repoRegisterCmd.SetContext(context.Background())
	for _, setting := range []struct {
		name  string
		value string
	}{
		{name: "coordinator", value: srv.URL},
		{name: "config-dir", value: configDir},
		{name: "format", value: outputFormatJSON},
		{name: "timeout", value: time.Second.String()},
	} {
		if err := repoRegisterCmd.Flags().Set(setting.name, setting.value); err != nil {
			t.Fatalf("set %s: %v", setting.name, err)
		}
	}
	t.Cleanup(func() {
		_ = repoRegisterCmd.Flags().Set("coordinator", "")
		_ = repoRegisterCmd.Flags().Set("config-dir", ".github/workbuddy")
		_ = repoRegisterCmd.Flags().Set("format", outputFormatText)
		_ = repoRegisterCmd.Flags().Set("timeout", "15s")
	})

	if err := runRepoRegisterCmd(repoRegisterCmd, nil); err != nil {
		t.Fatalf("runRepoRegisterCmd: %v", err)
	}

	var result repoRegisterResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if result.Repo != "octo/workbuddy" || result.Environment != "dev" || result.PollInterval != "45s" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.AgentCount != 2 || result.WorkflowCount != 1 {
		t.Fatalf("unexpected counts: %+v", result)
	}
}

func TestParseRepoListFlags_JSONAlias(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().String("coordinator", "", "")
	addCoordinatorAuthFlags(cmd.Flags(), "t", "Bearer token for coordinator auth")
	cmd.Flags().Duration("timeout", 15*time.Second, "")
	addOutputFormatFlag(cmd)
	addDeprecatedJSONAliasFlag(cmd)
	if err := cmd.Flags().Parse([]string{"--coordinator", "http://coord:8081", "--json"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	opts, err := parseRepoListFlags(cmd)
	if err != nil {
		t.Fatalf("parseRepoListFlags: %v", err)
	}
	if opts.format != outputFormatJSON {
		t.Fatalf("format = %q, want %q", opts.format, outputFormatJSON)
	}
}
