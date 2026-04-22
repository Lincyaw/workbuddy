package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/operatorwatch"
	"github.com/spf13/cobra"
)

func TestParseOperatorWatchFlagsCustom(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	chdirTemp(t, t.TempDir())

	cmd := newOperatorWatchFlagCommand()
	globalRoot := filepath.Join(homeDir, ".workbuddy")
	if err := cmd.Flags().Set("inbox", " "+filepath.Join(globalRoot, "operator", "inbox")+" "); err != nil {
		t.Fatalf("set inbox: %v", err)
	}
	if err := cmd.Flags().Set("db", " "+filepath.Join(globalRoot, "workbuddy.db")+" "); err != nil {
		t.Fatalf("set db: %v", err)
	}
	if err := cmd.Flags().Set("config-dir", " "+filepath.Join(globalRoot, ".github", "workbuddy")+" "); err != nil {
		t.Fatalf("set config-dir: %v", err)
	}
	if err := cmd.Flags().Set("claude", " /usr/local/bin/claude "); err != nil {
		t.Fatalf("set claude: %v", err)
	}
	if err := cmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatalf("set dry-run: %v", err)
	}

	opts, err := parseOperatorWatchFlags(cmd)
	if err != nil {
		t.Fatalf("parseOperatorWatchFlags: %v", err)
	}
	if opts.inboxDir != filepath.Join(globalRoot, "operator", "inbox") {
		t.Fatalf("inboxDir = %q, want %q", opts.inboxDir, filepath.Join(globalRoot, "operator", "inbox"))
	}
	if opts.dbPath != filepath.Join(globalRoot, "workbuddy.db") {
		t.Fatalf("dbPath = %q, want %q", opts.dbPath, filepath.Join(globalRoot, "workbuddy.db"))
	}
	if opts.configDir != filepath.Join(globalRoot, ".github", "workbuddy") {
		t.Fatalf("configDir = %q, want %q", opts.configDir, filepath.Join(globalRoot, ".github", "workbuddy"))
	}
	if opts.claudePath != "/usr/local/bin/claude" {
		t.Fatalf("claudePath = %q, want /usr/local/bin/claude", opts.claudePath)
	}
	if !opts.dryRun {
		t.Fatal("dryRun = false, want true")
	}
}

func TestParseOperatorWatchFlagsRejectsBlankInbox(t *testing.T) {
	cmd := newOperatorWatchFlagCommand()
	if err := cmd.Flags().Set("inbox", "   "); err != nil {
		t.Fatalf("set inbox: %v", err)
	}

	if _, err := parseOperatorWatchFlags(cmd); err == nil {
		t.Fatal("expected blank inbox error")
	}
}

func TestParseOperatorWatchFlagsRejectsMixedDefaultScopes(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	chdirTemp(t, t.TempDir())

	cmd := newOperatorWatchFlagCommand()
	_, err := parseOperatorWatchFlags(cmd)
	if err == nil {
		t.Fatal("expected scope mismatch error")
	}
	if !strings.Contains(err.Error(), "scope mismatch") {
		t.Fatalf("error = %q, want scope mismatch", err)
	}
	if !strings.Contains(err.Error(), "--inbox=\""+operatorwatch.DefaultInboxPath+"\" (global)") {
		t.Fatalf("error = %q, want inbox scope detail", err)
	}
	if !strings.Contains(err.Error(), "--db=\""+operatorwatch.DefaultDBPath+"\" (repo-local)") {
		t.Fatalf("error = %q, want db scope detail", err)
	}
	if !strings.Contains(err.Error(), "--config-dir=\""+operatorwatch.DefaultConfigDir+"\" (repo-local)") {
		t.Fatalf("error = %q, want config-dir scope detail", err)
	}
}

func TestParseOperatorWatchFlagsAcceptsGlobalScopeOverrides(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	chdirTemp(t, t.TempDir())

	cmd := newOperatorWatchFlagCommand()
	globalRoot := filepath.Join(homeDir, ".workbuddy")
	if err := cmd.Flags().Set("inbox", filepath.Join(globalRoot, "operator", "inbox")); err != nil {
		t.Fatalf("set inbox: %v", err)
	}
	if err := cmd.Flags().Set("db", filepath.Join(globalRoot, "workbuddy.db")); err != nil {
		t.Fatalf("set db: %v", err)
	}
	if err := cmd.Flags().Set("config-dir", filepath.Join(globalRoot, ".github", "workbuddy")); err != nil {
		t.Fatalf("set config-dir: %v", err)
	}

	opts, err := parseOperatorWatchFlags(cmd)
	if err != nil {
		t.Fatalf("parseOperatorWatchFlags: %v", err)
	}
	if opts.inboxDir != filepath.Join(globalRoot, "operator", "inbox") {
		t.Fatalf("inboxDir = %q", opts.inboxDir)
	}
	if opts.dbPath != filepath.Join(globalRoot, "workbuddy.db") {
		t.Fatalf("dbPath = %q", opts.dbPath)
	}
	if opts.configDir != filepath.Join(globalRoot, ".github", "workbuddy") {
		t.Fatalf("configDir = %q", opts.configDir)
	}
}

func TestParseOperatorWatchFlagsAcceptsRepoLocalScopeOverrides(t *testing.T) {
	repoRoot := t.TempDir()
	chdirTemp(t, repoRoot)

	cmd := newOperatorWatchFlagCommand()
	if err := cmd.Flags().Set("inbox", ".workbuddy/operator/inbox"); err != nil {
		t.Fatalf("set inbox: %v", err)
	}
	if err := cmd.Flags().Set("db", ".workbuddy/workbuddy.db"); err != nil {
		t.Fatalf("set db: %v", err)
	}
	if err := cmd.Flags().Set("config-dir", ".github/workbuddy"); err != nil {
		t.Fatalf("set config-dir: %v", err)
	}

	opts, err := parseOperatorWatchFlags(cmd)
	if err != nil {
		t.Fatalf("parseOperatorWatchFlags: %v", err)
	}
	if opts.inboxDir != ".workbuddy/operator/inbox" {
		t.Fatalf("inboxDir = %q", opts.inboxDir)
	}
	if opts.dbPath != ".workbuddy/workbuddy.db" {
		t.Fatalf("dbPath = %q", opts.dbPath)
	}
	if opts.configDir != ".github/workbuddy" {
		t.Fatalf("configDir = %q", opts.configDir)
	}
}

func newOperatorWatchFlagCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "operator-watch"}
	cmd.Flags().String("inbox", operatorwatch.DefaultInboxPath, "")
	cmd.Flags().String("db", operatorwatch.DefaultDBPath, "")
	cmd.Flags().String("config-dir", operatorwatch.DefaultConfigDir, "")
	cmd.Flags().String("claude", "", "")
	cmd.Flags().Bool("dry-run", false, "")
	return cmd
}

func chdirTemp(t *testing.T, dir string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir(%q): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})
}
