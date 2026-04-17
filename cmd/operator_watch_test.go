package cmd

import (
	"testing"

	"github.com/Lincyaw/workbuddy/internal/operatorwatch"
	"github.com/spf13/cobra"
)

func TestParseOperatorWatchFlagsDefaults(t *testing.T) {
	cmd := newOperatorWatchFlagCommand()

	opts, err := parseOperatorWatchFlags(cmd)
	if err != nil {
		t.Fatalf("parseOperatorWatchFlags: %v", err)
	}
	if opts.inboxDir != operatorwatch.DefaultInboxPath {
		t.Fatalf("inboxDir = %q, want %q", opts.inboxDir, operatorwatch.DefaultInboxPath)
	}
	if opts.claudePath != "" {
		t.Fatalf("claudePath = %q, want empty", opts.claudePath)
	}
	if opts.dryRun {
		t.Fatal("dryRun = true, want false")
	}
}

func TestParseOperatorWatchFlagsCustom(t *testing.T) {
	cmd := newOperatorWatchFlagCommand()
	if err := cmd.Flags().Set("inbox", " /tmp/inbox "); err != nil {
		t.Fatalf("set inbox: %v", err)
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
	if opts.inboxDir != "/tmp/inbox" {
		t.Fatalf("inboxDir = %q, want /tmp/inbox", opts.inboxDir)
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

func newOperatorWatchFlagCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "operator-watch"}
	cmd.Flags().String("inbox", operatorwatch.DefaultInboxPath, "")
	cmd.Flags().String("claude", "", "")
	cmd.Flags().Bool("dry-run", false, "")
	return cmd
}
