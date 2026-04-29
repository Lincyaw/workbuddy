package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/spf13/cobra"
)

func TestRootFlagNoColor_StripsANSIFromSubcommandOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `[{"repo":"\u001b[31mowner/a\u001b[0m","environment":"prod","status":"active","poller_status":"running"}]`)
	}))
	defer srv.Close()

	stdout, _, err := executeRootContractTest(t, "--no-color", "repo", "list", "--coordinator", srv.URL)
	if err != nil {
		t.Fatalf("execute root: %v", err)
	}
	if strings.Contains(stdout, "\x1b[") {
		t.Fatalf("expected ANSI escapes to be stripped, got %q", stdout)
	}
	if !strings.Contains(stdout, "owner/a") {
		t.Fatalf("expected stripped repo name in output, got %q", stdout)
	}
}

func TestRootFlagNoColor_StripsANSIFromServeBanner(t *testing.T) {
	resetRootContractFlagsForTest(t)
	_ = rootCmd.PersistentFlags().Set(flagNoColor, "true")

	var stdout bytes.Buffer
	serveCmd.SetOut(&stdout)

	writeServeBanner(cmdStdout(serveCmd), &config.FullConfig{
		Global: config.GlobalConfig{
			Repo:         "\x1b[31mowner/repo\x1b[0m",
			PollInterval: 2 * time.Second,
			Port:         8080,
		},
	}, &serveOpts{
		roles: []string{"\x1b[32mdev\x1b[0m"},
	}, "127.0.0.1:8080")

	got := stdout.String()
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("expected ANSI escapes to be stripped from serve banner, got %q", got)
	}
	if !strings.Contains(got, "owner/repo") {
		t.Fatalf("expected stripped repo name in output, got %q", got)
	}
}

func TestServeFlagCoordinatorAPIRemoved(t *testing.T) {
	_, _, err := executeRootContractTest(t, "serve", "--coordinator-api")
	if err == nil {
		t.Fatal("expected serve to reject removed --coordinator-api flag")
	}
	if !strings.Contains(err.Error(), "unknown flag: --coordinator-api") {
		t.Fatalf("expected unknown flag error, got %v", err)
	}
}

func TestRootFlagNoColor_StripsANSIFromLogOutput(t *testing.T) {
	resetRootContractFlagsForTest(t)

	logCmd := &cobra.Command{
		Use: "test-log",
		Run: func(_ *cobra.Command, _ []string) {
			log.Print("\x1b[31mlog-ansi\x1b[0m")
		},
	}
	rootCmd.AddCommand(logCmd)
	t.Cleanup(func() {
		rootCmd.RemoveCommand(logCmd)
		log.SetOutput(os.Stderr)
	})

	_, stderr, err := executeRootContractTest(t, "--no-color", "test-log")
	if err != nil {
		t.Fatalf("execute root: %v", err)
	}
	if strings.Contains(stderr, "\x1b[") {
		t.Fatalf("expected ANSI escapes to be stripped from log output, got %q", stderr)
	}
	if !strings.Contains(stderr, "log-ansi") {
		t.Fatalf("expected stripped log output, got %q", stderr)
	}
}

func TestRootFlagNoColor_StripsANSIFromEventlogStderr(t *testing.T) {
	resetRootContractFlagsForTest(t)

	eventlogCmd := &cobra.Command{
		Use: "test-eventlog",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dbPath := filepath.Join(t.TempDir(), "workbuddy.db")
			st, err := store.NewStore(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			eventlog.NewEventLoggerWithWriter(st, cmdStderr(cmd)).Log(eventlog.TypeError, "owner/repo", 1, ansiMarshalPayload{})
			return nil
		},
	}
	rootCmd.AddCommand(eventlogCmd)
	t.Cleanup(func() {
		rootCmd.RemoveCommand(eventlogCmd)
		log.SetOutput(os.Stderr)
	})

	_, stderr, err := executeRootContractTest(t, "--no-color", "test-eventlog")
	if err != nil {
		t.Fatalf("execute root: %v", err)
	}
	if strings.Contains(stderr, "\x1b[") {
		t.Fatalf("expected ANSI escapes to be stripped from eventlog stderr, got %q", stderr)
	}
	if !strings.Contains(stderr, "eventlog: marshal payload: json: error calling MarshalJSON") || !strings.Contains(stderr, "boom") {
		t.Fatalf("expected stripped eventlog stderr, got %q", stderr)
	}
}

func TestRootFlagNonInteractive_RecoverFailsFast(t *testing.T) {
	_, _, err := executeRootContractTest(t, "--non-interactive", "recover", "--prune-remote-branches")
	if err == nil {
		t.Fatal("expected recover to fail in non-interactive mode")
	}
	if !strings.Contains(err.Error(), "--non-interactive") {
		t.Fatalf("expected non-interactive error, got %v", err)
	}
}

func TestRootFlagReadOnly_RepoRegisterFailsBeforeSideEffects(t *testing.T) {
	t.Chdir(t.TempDir())

	_, _, err := executeRootContractTest(t, "--read-only", "repo", "register", "--coordinator", "http://127.0.0.1:8081")
	if err == nil {
		t.Fatal("expected repo register to fail in read-only mode")
	}
	if !strings.Contains(err.Error(), "--read-only") {
		t.Fatalf("expected read-only error, got %v", err)
	}
	if strings.Contains(err.Error(), ".github/workbuddy") {
		t.Fatalf("expected read-only guard before config load, got %v", err)
	}
}

func executeRootContractTest(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	resetRootContractFlagsForTest(t)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	rootCmd.SetIn(strings.NewReader(""))
	rootCmd.SetArgs(args)

	err := rootCmd.Execute()
	return stdout.String(), stderr.String(), err
}

type ansiMarshalPayload struct{}

func (ansiMarshalPayload) MarshalJSON() ([]byte, error) {
	return nil, errors.New("\x1b[31mboom\x1b[0m")
}

func resetRootContractFlagsForTest(t *testing.T) {
	t.Helper()

	t.Cleanup(func() {
		_ = rootCmd.PersistentFlags().Set(flagNoColor, "false")
		_ = rootCmd.PersistentFlags().Set(flagNonInteractive, "false")
		_ = rootCmd.PersistentFlags().Set(flagReadOnly, "false")
	})

	_ = rootCmd.PersistentFlags().Set(flagNoColor, "false")
	_ = rootCmd.PersistentFlags().Set(flagNonInteractive, "false")
	_ = rootCmd.PersistentFlags().Set(flagReadOnly, "false")
}
