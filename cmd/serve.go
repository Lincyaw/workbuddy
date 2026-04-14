package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run Coordinator + Worker in single process (v0.1.0)",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("workbuddy serve: not yet implemented")
		return nil
	},
}

func init() {
	serveCmd.Flags().IntP("port", "p", 8080, "HTTP server port")
	serveCmd.Flags().Duration("poll-interval", 0, "GitHub poll interval (default 30s)")
	serveCmd.Flags().StringSlice("roles", []string{"dev", "test", "review"}, "Worker roles")
	rootCmd.AddCommand(serveCmd)
}
