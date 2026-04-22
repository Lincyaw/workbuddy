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

func addDeprecatedJSONAliasFlag(cmd *cobra.Command) {
	cmd.Flags().Bool("json", false, "Deprecated alias for --format json")
	_ = cmd.Flags().MarkDeprecated("json", "use --format json")
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

	if cmd.Flags().Lookup("json") != nil {
		jsonAlias, _ := cmd.Flags().GetBool("json")
		if jsonAlias {
			if cmd.Flags().Changed("format") && format != outputFormatJSON {
				return "", fmt.Errorf("%s: --json conflicts with --format %s", commandName, format)
			}
			format = outputFormatJSON
		}
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
