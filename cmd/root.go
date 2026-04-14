// Package cmd implements the workbuddy CLI commands.
package cmd

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "workbuddy",
	Short: "GitHub Issue-driven agent orchestration platform",
	Long:  "Hub-Spoke architecture: Coordinator polls GitHub Issues and manages label-based state machine; Workers execute agent instances.",
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}
