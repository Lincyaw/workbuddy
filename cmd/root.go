// Package cmd implements the workbuddy CLI commands.
package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:           "workbuddy",
	Short:         "GitHub Issue-driven agent orchestration platform",
	Long:          "Hub-Spoke architecture: Coordinator polls GitHub Issues and manages label-based state machine; Workers execute agent instances.",
	SilenceErrors: true,
	SilenceUsage:  true,
}

// Execute runs the root command.
func Execute() error {
	cmd, err := rootCmd.ExecuteC()
	if err == nil {
		return nil
	}
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
