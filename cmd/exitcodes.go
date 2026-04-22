package cmd

import (
	"context"
	"errors"
	"strings"
)

// ExitCode is the process exit code returned by CLI commands.
type ExitCode int

const (
	ExitCodeSuccess ExitCode = 0

	exitCodeFailure ExitCode = 1

	ExitCodeUsage             ExitCode = 2
	ExitCodeNotFound          ExitCode = 3
	ExitCodeUnauthorized      ExitCode = 4
	ExitCodeConflict          ExitCode = 5
	ExitCodeCancelled         ExitCode = 6
	ExitCodeMissingDependency ExitCode = 7
)

func normalizeCLIError(err error) error {
	if err == nil {
		return nil
	}

	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) {
		return err
	}

	switch {
	case errors.Is(err, context.Canceled):
		return &cliExitError{msg: err.Error(), code: ExitCodeCancelled}
	case isUsageError(err):
		return &cliExitError{msg: err.Error(), code: ExitCodeUsage}
	default:
		return err
	}
}

func isUsageError(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	switch {
	case strings.Contains(msg, "unknown command"),
		strings.Contains(msg, "unknown flag"),
		strings.Contains(msg, "unknown shorthand flag"),
		strings.Contains(msg, "flag needs an argument"),
		strings.Contains(msg, "accepts "),
		strings.Contains(msg, "requires at least"),
		strings.Contains(msg, "requires at most"),
		strings.Contains(msg, "requires exactly"),
		strings.Contains(msg, "requires no arguments"),
		strings.Contains(msg, "invalid argument"),
		strings.Contains(msg, "must be one of"):
		return true
	default:
		return false
	}
}
