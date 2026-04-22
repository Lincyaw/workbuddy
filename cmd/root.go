// Package cmd implements the workbuddy CLI commands.
package cmd

import (
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/spf13/cobra"
)

const (
	flagNoColor        = "no-color"
	flagNonInteractive = "non-interactive"
	flagReadOnly       = "read-only"
)

var rootCmd = &cobra.Command{
	Use:               "workbuddy",
	Short:             "GitHub Issue-driven agent orchestration platform",
	Long:              "Hub-Spoke architecture: Coordinator polls GitHub Issues and manages label-based state machine; Workers execute agent instances.",
	SilenceErrors:     true,
	SilenceUsage:      true,
	RunE:              runRootCmd,
	PersistentPreRunE: configureRootCommand,
}

func init() {
	rootCmd.PersistentFlags().Bool(flagNoColor, false, "Strip ANSI color codes from all command output")
	rootCmd.PersistentFlags().Bool(flagNonInteractive, false, "Fail instead of prompting for interactive confirmation")
	rootCmd.PersistentFlags().Bool(flagReadOnly, false, "Reject commands that would write or mutate state")
}

func configureRootCommand(cmd *cobra.Command, _ []string) error {
	log.SetOutput(cmdStderr(cmd))
	return nil
}

// Execute runs the root command.
func Execute() error {
	cmd, err := rootCmd.ExecuteC()
	if err == nil {
		return nil
	}
	err = normalizeCLIError(err)
	if shouldPrintUsage(err) {
		if cmd == nil {
			cmd = rootCmd
		}
		printCommandError(cmd.ErrOrStderr(), err, cmd.UsageString())
		return &reportedCLIError{err: err}
	}
	return err
}

func shouldPrintUsage(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "unknown flag:") ||
		strings.Contains(message, "required flag(s)") ||
		strings.HasPrefix(message, "unknown command ")
}

func printCommandError(stderr io.Writer, err error, usage string) {
	if err == nil {
		return
	}
	_, _ = fmt.Fprintf(stderr, "Error: %v\n", err)
	if strings.TrimSpace(usage) == "" {
		return
	}
	_, _ = io.WriteString(stderr, usage)
}

type reportedCLIError struct {
	err error
}

func (e *reportedCLIError) Error() string {
	return ""
}

func (e *reportedCLIError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *reportedCLIError) ExitCode() int {
	if e == nil || e.err == nil {
		return 1
	}
	type exitCoder interface {
		ExitCode() int
	}
	var ec exitCoder
	if errors.As(e.err, &ec) {
		return ec.ExitCode()
	}
	return 1
}

func init() {
	rootCmd.Flags().Bool("dump-schema", false, "Emit the full CLI command tree as JSON")
}

func runRootCmd(cmd *cobra.Command, _ []string) error {
	dumpSchema, _ := cmd.Flags().GetBool("dump-schema")
	if dumpSchema {
		return writeSchema(cmd.OutOrStdout(), cmd.Root())
	}
	if err := cmd.Help(); err != nil {
		return fmt.Errorf("render root help: %w", err)
	}
	return nil
}
