package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

func confirmDestructiveAction(op string, stdin io.Reader, stdout io.Writer, interactive, force, dryRun bool, prompt string, details []string) (bool, error) {
	if dryRun || force {
		return true, nil
	}
	if !interactive {
		return false, fmt.Errorf("%s: --force is required in non-interactive mode; re-run with --force or use a TTY to confirm", op)
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if prompt != "" {
		if _, err := fmt.Fprintln(stdout, prompt); err != nil {
			return false, fmt.Errorf("%s: write prompt: %w", op, err)
		}
	}
	for _, detail := range details {
		detail = strings.TrimSpace(detail)
		if detail == "" {
			continue
		}
		if _, err := fmt.Fprintf(stdout, " - %s\n", detail); err != nil {
			return false, fmt.Errorf("%s: write prompt detail: %w", op, err)
		}
	}
	if _, err := fmt.Fprint(stdout, "Type 'yes' to continue: "); err != nil {
		return false, fmt.Errorf("%s: write prompt: %w", op, err)
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	answer, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("%s: read confirmation: %w", op, err)
	}
	if !strings.EqualFold(strings.TrimSpace(answer), "yes") {
		if _, err := fmt.Fprintln(stdout, "Canceled."); err != nil {
			return false, fmt.Errorf("%s: write cancel message: %w", op, err)
		}
		return false, nil
	}
	return true, nil
}
