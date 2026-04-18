package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestParseRepoListFlags(t *testing.T) {
	cmd := newRepoListFlagCommand()
	_ = cmd.Flags().Set("coordinator", "http://127.0.0.1:8091")
	_ = cmd.Flags().Set("token", "my-token")
	_ = cmd.Flags().Set("json", "true")

	opts, err := parseRepoListFlags(cmd)
	if err != nil {
		t.Fatalf("parseRepoListFlags: %v", err)
	}
	if opts.coordinator != "http://127.0.0.1:8091" {
		t.Fatalf("coordinator = %q", opts.coordinator)
	}
	if opts.token != "my-token" {
		t.Fatalf("token = %q", opts.token)
	}
	if !opts.jsonOut {
		t.Fatal("jsonOut should be true")
	}
}

func TestParseRepoListFlags_TokenFromEnv(t *testing.T) {
	t.Setenv("WORKBUDDY_AUTH_TOKEN", "env-token")

	cmd := newRepoListFlagCommand()
	_ = cmd.Flags().Set("coordinator", "http://127.0.0.1:8091")

	opts, err := parseRepoListFlags(cmd)
	if err != nil {
		t.Fatalf("parseRepoListFlags: %v", err)
	}
	if opts.token != "env-token" {
		t.Fatalf("token = %q", opts.token)
	}
}

func TestParseRepoListFlags_RequiresCoordinator(t *testing.T) {
	cmd := newRepoListFlagCommand()
	_, err := parseRepoListFlags(cmd)
	if err == nil || !strings.Contains(err.Error(), "--coordinator is required") {
		t.Fatalf("expected coordinator required error, got %v", err)
	}
}

func TestParseRepoRegisterFlags_TokenFromEnv(t *testing.T) {
	t.Setenv("WORKBUDDY_AUTH_TOKEN", "env-token")

	cmd := &cobra.Command{Use: "register"}
	cmd.Flags().String("coordinator", "", "")
	cmd.Flags().StringP("token", "t", "", "")
	cmd.Flags().String("config-dir", ".github/workbuddy", "")
	cmd.Flags().Duration("timeout", 15*time.Second, "")
	_ = cmd.Flags().Set("coordinator", "http://127.0.0.1:8091")

	opts, err := parseRepoRegisterFlags(cmd)
	if err != nil {
		t.Fatalf("parseRepoRegisterFlags: %v", err)
	}
	if opts.token != "env-token" {
		t.Fatalf("token = %q", opts.token)
	}
}

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
		jsonOut:     true,
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

func newRepoListFlagCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "list"}
	cmd.Flags().String("coordinator", "", "")
	cmd.Flags().StringP("token", "t", "", "")
	cmd.Flags().Bool("json", false, "")
	cmd.Flags().Duration("timeout", 15*time.Second, "")
	return cmd
}
