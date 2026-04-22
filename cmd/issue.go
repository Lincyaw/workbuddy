package cmd

import "github.com/spf13/cobra"

var issueCmd = &cobra.Command{
	Use:   "issue",
	Short: "Manage issue-scoped recovery operations",
}

func init() {
	rootCmd.AddCommand(issueCmd)
}
