package cmd

import (
	"fmt"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/operatorwatch"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/spf13/cobra"
)

type operatorWatchOpts struct {
	inboxDir   string
	claudePath string
	dryRun     bool
}

var operatorWatchCmd = &cobra.Command{
	Use:   "operator-watch",
	Short: "Watch the local operator inbox and dispatch Claude incident handlers",
	RunE:  runOperatorWatchCmd,
}

func init() {
	operatorWatchCmd.Flags().String("inbox", operatorwatch.DefaultInboxPath, "Incident inbox directory")
	operatorWatchCmd.Flags().String("claude", "", "Path to the claude CLI binary (default: search PATH)")
	operatorWatchCmd.Flags().Bool("dry-run", false, "Log the Claude invocation without spawning it")
	rootCmd.AddCommand(operatorWatchCmd)
}

func runOperatorWatchCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseOperatorWatchFlags(cmd)
	if err != nil {
		return err
	}

	st, err := store.NewStore(operatorwatch.DefaultDBPath)
	if err != nil {
		return fmt.Errorf("operator-watch: open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	return operatorwatch.Run(cmd.Context(), operatorwatch.Options{
		InboxDir:   opts.inboxDir,
		ClaudePath: opts.claudePath,
		ConfigDir:  operatorwatch.DefaultConfigDir,
		Timeout:    operatorwatch.DefaultTimeout,
		DryRun:     opts.dryRun,
		Stdout:     cmd.OutOrStdout(),
		Stderr:     cmd.ErrOrStderr(),
		Logger:     eventlog.NewEventLogger(st),
	})
}

func parseOperatorWatchFlags(cmd *cobra.Command) (*operatorWatchOpts, error) {
	inboxDir, _ := cmd.Flags().GetString("inbox")
	claudePath, _ := cmd.Flags().GetString("claude")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	inboxDir = strings.TrimSpace(inboxDir)
	if inboxDir == "" {
		return nil, fmt.Errorf("operator-watch: --inbox is required")
	}

	return &operatorWatchOpts{
		inboxDir:   inboxDir,
		claudePath: strings.TrimSpace(claudePath),
		dryRun:     dryRun,
	}, nil
}
