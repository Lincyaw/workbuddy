package cmd

import (
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

var (
	ansiCSIRegexp = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	ansiOSCRegexp = regexp.MustCompile(`\x1b\][^\a\x1b]*(?:\a|\x1b\\)`)
)

func cmdStdout(cmd *cobra.Command) io.Writer {
	if cmd == nil {
		return io.Discard
	}
	return wrapOutputWriter(cmd, cmd.OutOrStdout())
}

func cmdStderr(cmd *cobra.Command) io.Writer {
	if cmd == nil {
		return io.Discard
	}
	return wrapOutputWriter(cmd, cmd.ErrOrStderr())
}

func wrapOutputWriter(cmd *cobra.Command, w io.Writer) io.Writer {
	if !flagEnabled(cmd, flagNoColor) {
		return w
	}
	return ansiStripWriter{target: w}
}

func flagEnabled(cmd *cobra.Command, name string) bool {
	if cmd == nil {
		return false
	}
	value, err := cmd.Flags().GetBool(name)
	return err == nil && value
}

func isNonInteractive(cmd *cobra.Command) bool {
	return flagEnabled(cmd, flagNonInteractive)
}

func isReadOnlyMode(cmd *cobra.Command) bool {
	return flagEnabled(cmd, flagReadOnly)
}

func requireWritable(cmd *cobra.Command, action string) error {
	if !isReadOnlyMode(cmd) {
		return nil
	}
	action = strings.TrimSpace(action)
	if action == "" {
		action = cmd.CommandPath()
	}
	return &cliExitError{
		msg:  fmt.Sprintf("%s: blocked by --read-only; command writes or mutates state", action),
		code: 1,
	}
}

type ansiStripWriter struct {
	target io.Writer
}

func (w ansiStripWriter) Write(p []byte) (int, error) {
	cleaned := stripANSI(p)
	_, err := w.target.Write(cleaned)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func stripANSI(p []byte) []byte {
	p = ansiOSCRegexp.ReplaceAll(p, nil)
	p = ansiCSIRegexp.ReplaceAll(p, nil)
	return p
}
