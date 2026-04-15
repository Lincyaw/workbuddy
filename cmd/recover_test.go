package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestParseRecoverFlagsDefaultsToSafeSubset(t *testing.T) {
	cmd := &cobra.Command{Use: "recover"}
	cmd.Flags().Bool("kill-zombies", false, "")
	cmd.Flags().Bool("reset-db", false, "")
	cmd.Flags().Bool("prune-worktrees", false, "")
	cmd.Flags().Bool("prune-remote-branches", false, "")
	cmd.Flags().Bool("force", false, "")
	cmd.Flags().Bool("dry-run", false, "")

	opts, err := parseRecoverFlags(cmd)
	if err != nil {
		t.Fatalf("parseRecoverFlags: %v", err)
	}
	if !opts.killZombies || !opts.resetDB || !opts.pruneWorktrees {
		t.Fatalf("default safe subset not enabled: %+v", opts)
	}
	if opts.pruneRemoteBranches {
		t.Fatalf("pruneRemoteBranches should remain disabled by default: %+v", opts)
	}
}

func TestParseRecoverFlagsRespectsExplicitSelection(t *testing.T) {
	cmd := &cobra.Command{Use: "recover"}
	cmd.Flags().Bool("kill-zombies", false, "")
	cmd.Flags().Bool("reset-db", false, "")
	cmd.Flags().Bool("prune-worktrees", false, "")
	cmd.Flags().Bool("prune-remote-branches", false, "")
	cmd.Flags().Bool("force", false, "")
	cmd.Flags().Bool("dry-run", false, "")
	if err := cmd.Flags().Set("prune-remote-branches", "true"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("force", "true"); err != nil {
		t.Fatal(err)
	}

	opts, err := parseRecoverFlags(cmd)
	if err != nil {
		t.Fatalf("parseRecoverFlags: %v", err)
	}
	if opts.killZombies || opts.resetDB || opts.pruneWorktrees {
		t.Fatalf("explicit selection should disable default safe subset: %+v", opts)
	}
	if !opts.pruneRemoteBranches || !opts.force {
		t.Fatalf("explicit flags missing: %+v", opts)
	}
}
