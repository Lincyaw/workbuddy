// Package cmd: `workbuddy db` command group.
//
// `db` is the umbrella for offline database maintenance commands. v0.5 ships
// a single subcommand, `db migrate`, which performs a one-shot offline copy
// of every row from a SQLite source DSN to a MySQL destination DSN (per
// docs/decisions/2026-05-13-k8s-agentm-otel.md Block 3 § Migration tool and
// issue #318). Future maintenance commands (dump, verify, repair) can hang
// off the same group.
package cmd

import "github.com/spf13/cobra"

var dbCmd = &cobra.Command{
	Use:   "db",
	Short: "Offline database maintenance commands",
	Long: `Offline database maintenance commands.

Subcommands operate on a workbuddy SQLite or MySQL database without a
running coordinator. The coordinator MUST be stopped before running any
mutating db subcommand.`,
}

func init() {
	rootCmd.AddCommand(dbCmd)
}
