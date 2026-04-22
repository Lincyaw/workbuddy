package cmd

import "github.com/spf13/cobra"

var adminCmd = &cobra.Command{
	Use:   "admin",
	Short: "Operator recovery and maintenance commands",
}

func init() {
	rootCmd.AddCommand(adminCmd)
}
