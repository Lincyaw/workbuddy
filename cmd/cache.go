package cmd

import "github.com/spf13/cobra"

var cacheCmd = &cobra.Command{
	Use:   "cache",
	Short: "Manage cached coordinator state",
}

func init() {
	rootCmd.AddCommand(cacheCmd)
}
