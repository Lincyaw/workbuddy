package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

const (
	outputFormatText = "text"
	outputFormatJSON = "json"
)

func addOutputFormatFlag(cmd *cobra.Command) {
	cmd.Flags().String("format", outputFormatText, "Output format: text or json")
}

func resolveOutputFormat(cmd *cobra.Command, commandName string) (string, error) {
	format, _ := cmd.Flags().GetString("format")
	format = strings.TrimSpace(format)
	if format == "" {
		format = outputFormatText
	}
	if format != outputFormatText && format != outputFormatJSON {
		return "", fmt.Errorf("%s: --format must be one of text, json", commandName)
	}

	return format, nil
}

func isJSONOutput(format string) bool {
	return strings.EqualFold(strings.TrimSpace(format), outputFormatJSON)
}

func writeJSON(w io.Writer, payload any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}
