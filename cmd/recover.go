package cmd

import (
	"os"

	recovery "github.com/Lincyaw/workbuddy/internal/recover"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

type recoverOpts struct {
	killZombies         bool
	resetDB             bool
	pruneWorktrees      bool
	pruneRemoteBranches bool
	force               bool
	dryRun              bool
}

var recoverCmd = &cobra.Command{
	Use:   "recover",
	Short: "Recover local workbuddy runtime state after an unclean shutdown",
	Long:  "Clean up stale runtime processes, worktrees, cached SQLite state, and optional remote workbuddy branches.",
	RunE:  runRecoverCmd,
}

func init() {
	recoverCmd.Flags().Bool("kill-zombies", false, "Terminate stale codex and workbuddy serve processes for this repo")
	recoverCmd.Flags().Bool("reset-db", false, "Clear runtime SQLite tables while preserving schema and transition counts")
	recoverCmd.Flags().Bool("prune-worktrees", false, "Remove stale .workbuddy/worktrees entries and run git worktree prune")
	recoverCmd.Flags().Bool("prune-remote-branches", false, "Delete orphaned origin/workbuddy/issue-* branches that have no open PR")
	recoverCmd.Flags().Bool("force", false, "Skip confirmation prompts for destructive actions")
	recoverCmd.Flags().Bool("dry-run", false, "Print the actions that would be taken without executing them")
	rootCmd.AddCommand(recoverCmd)
}

func runRecoverCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseRecoverFlags(cmd)
	if err != nil {
		return err
	}
	return recovery.Run(cmd.Context(), recovery.Options{
		KillZombies:         opts.killZombies,
		ResetDB:             opts.resetDB,
		PruneWorktrees:      opts.pruneWorktrees,
		PruneRemoteBranches: opts.pruneRemoteBranches,
		Force:               opts.force,
		DryRun:              opts.dryRun,
		Interactive:         isInteractiveTerminal(),
		Stdin:               cmd.InOrStdin(),
		Stdout:              cmd.OutOrStdout(),
		Stderr:              cmd.ErrOrStderr(),
	})
}

func parseRecoverFlags(cmd *cobra.Command) (*recoverOpts, error) {
	killZombies, _ := cmd.Flags().GetBool("kill-zombies")
	resetDB, _ := cmd.Flags().GetBool("reset-db")
	pruneWorktrees, _ := cmd.Flags().GetBool("prune-worktrees")
	pruneRemoteBranches, _ := cmd.Flags().GetBool("prune-remote-branches")
	force, _ := cmd.Flags().GetBool("force")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	if !killZombies && !resetDB && !pruneWorktrees && !pruneRemoteBranches {
		killZombies = true
		resetDB = true
		pruneWorktrees = true
	}
	return &recoverOpts{
		killZombies:         killZombies,
		resetDB:             resetDB,
		pruneWorktrees:      pruneWorktrees,
		pruneRemoteBranches: pruneRemoteBranches,
		force:               force,
		dryRun:              dryRun,
	}, nil
}

func isInteractiveTerminal() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
}
