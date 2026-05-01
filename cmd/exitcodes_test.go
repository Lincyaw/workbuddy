package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExitCodeValues(t *testing.T) {
	tests := []struct {
		name string
		code ExitCode
		want int
	}{
		{name: "success", code: ExitCodeSuccess, want: 0},
		{name: "usage", code: ExitCodeUsage, want: 2},
		{name: "not found", code: ExitCodeNotFound, want: 3},
		{name: "unauthorized", code: ExitCodeUnauthorized, want: 4},
		{name: "conflict", code: ExitCodeConflict, want: 5},
		{name: "cancelled", code: ExitCodeCancelled, want: 6},
		{name: "missing dependency", code: ExitCodeMissingDependency, want: 7},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := int(tc.code); got != tc.want {
				t.Fatalf("int(%s) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

func TestExecuteUnknownSubcommandReturnsUsageExitCode(t *testing.T) {
	prevOut := rootCmd.OutOrStdout()
	prevErr := rootCmd.ErrOrStderr()
	t.Cleanup(func() {
		rootCmd.SetOut(prevOut)
		rootCmd.SetErr(prevErr)
		rootCmd.SetArgs(nil)
	})

	var output bytes.Buffer
	rootCmd.SetOut(&output)
	rootCmd.SetErr(&output)
	rootCmd.SetArgs([]string{"definitely-not-a-subcommand"})

	err := Execute()
	if err == nil {
		t.Fatal("expected unknown subcommand error")
	}

	var exitErr *cliExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected cliExitError, got %T", err)
	}
	if got, want := exitErr.ExitCode(), int(ExitCodeUsage); got != want {
		t.Fatalf("exit code = %d, want %d", got, want)
	}
	if !strings.Contains(exitErr.Error(), "unknown command") {
		t.Fatalf("error = %q, want unknown command", exitErr.Error())
	}
}

func TestRunDeployStopWithOptsMissingManifestReturnsNotFoundExitCode(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", filepath.Join(tempDir, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempDir, "xdg"))

	err := runDeployStopWithOpts(context.Background(), &deployLookupOpts{name: "missing", scope: "user"}, io.Discard)
	if err == nil {
		t.Fatal("expected missing manifest error")
	}

	var exitErr interface{ ExitCode() int }
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected error with ExitCode(), got %T", err)
	}
	if got, want := exitErr.ExitCode(), int(ExitCodeNotFound); got != want {
		t.Fatalf("exit code = %d, want %d", got, want)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %q, want not-found detail", err.Error())
	}
}

func TestRunWorkerWithOptsUnauthorizedReturnsAuthExitCode(t *testing.T) {
	repo := "owner/test-repo"
	configDir := setupTestConfigDir(t, repo)
	workDir := initGitRepo(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/workers/register" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	err := runWorkerWithOpts(&workerOpts{
		coordinatorURL:    srv.URL,
		token:             "bad-token",
		runtime:           "claude-code",
		configDir:         configDir,
		workDir:           workDir,
		dbPath:            filepath.Join(t.TempDir(), "worker.db"),
		sessionsDir:       filepath.Join(t.TempDir(), "sessions"),
		pollTimeout:       50 * time.Millisecond,
		heartbeatInterval: 25 * time.Millisecond,
		shutdownTimeout:   time.Second,
	}, nil, &mockGHReader{})
	if err == nil {
		t.Fatal("expected unauthorized worker error")
	}

	var exitErr *cliExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected cliExitError, got %T", err)
	}
	if got, want := exitErr.ExitCode(), int(ExitCodeUnauthorized); got != want {
		t.Fatalf("exit code = %d, want %d", got, want)
	}
	if !strings.Contains(exitErr.Error(), "rejected the provided token") {
		t.Fatalf("error = %q, want rejected token detail", exitErr.Error())
	}
}
