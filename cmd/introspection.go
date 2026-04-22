package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const cliSchemaVersion = 1

type cliSchema struct {
	SchemaVersion int           `json:"schema_version"`
	Version       string        `json:"version"`
	VersionString string        `json:"version_string"`
	Command       commandSchema `json:"command"`
}

type commandSchema struct {
	Name        string                   `json:"name"`
	Path        string                   `json:"path"`
	Use         string                   `json:"use"`
	Short       string                   `json:"short"`
	Flags       map[string]flagSchema    `json:"flags"`
	Subcommands map[string]commandSchema `json:"subcommands"`
}

type flagSchema struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type"`
	Default   any    `json:"default"`
	Required  bool   `json:"required"`
}

func writeSchema(w io.Writer, root *cobra.Command) error {
	schema, err := buildCLISchema(root)
	if err != nil {
		return err
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(schema); err != nil {
		return fmt.Errorf("encode cli schema: %w", err)
	}
	return nil
}

func buildCLISchema(root *cobra.Command) (cliSchema, error) {
	if root == nil {
		return cliSchema{}, fmt.Errorf("root command is nil")
	}

	root.Version = versionString()
	root.InitDefaultVersionFlag()

	command, err := buildCommandSchema(root)
	if err != nil {
		return cliSchema{}, err
	}

	version := strings.TrimSpace(Version)
	if version == "" {
		version = "dev"
	}

	return cliSchema{
		SchemaVersion: cliSchemaVersion,
		Version:       version,
		VersionString: versionString(),
		Command:       command,
	}, nil
}

func buildCommandSchema(cmd *cobra.Command) (commandSchema, error) {
	if cmd == nil {
		return commandSchema{}, fmt.Errorf("command is nil")
	}

	schema := commandSchema{
		Name:        cmd.Name(),
		Path:        cmd.CommandPath(),
		Use:         cmd.Use,
		Short:       commandShort(cmd),
		Flags:       map[string]flagSchema{},
		Subcommands: map[string]commandSchema{},
	}

	cmd.NonInheritedFlags().VisitAll(func(flag *pflag.Flag) {
		schema.Flags[flag.Name] = flagSchema{
			Name:      flag.Name,
			Shorthand: flag.Shorthand,
			Type:      flag.Value.Type(),
			Default:   flagDefaultValue(flag),
			Required:  flagRequired(flag),
		}
	})

	for _, sub := range cmd.Commands() {
		if !sub.IsAvailableCommand() || isDefaultHelpCommand(sub) {
			continue
		}
		subSchema, err := buildCommandSchema(sub)
		if err != nil {
			return commandSchema{}, err
		}
		schema.Subcommands[sub.Name()] = subSchema
	}

	return schema, nil
}

func commandShort(cmd *cobra.Command) string {
	if short := strings.TrimSpace(cmd.Short); short != "" {
		return short
	}

	long := strings.TrimSpace(cmd.Long)
	if long == "" {
		return ""
	}
	if idx := strings.IndexByte(long, '\n'); idx >= 0 {
		return strings.TrimSpace(long[:idx])
	}
	return long
}

func flagRequired(flag *pflag.Flag) bool {
	if flag == nil {
		return false
	}
	values, ok := flag.Annotations[cobra.BashCompOneRequiredFlag]
	return ok && len(values) > 0
}

func flagDefaultValue(flag *pflag.Flag) any {
	if flag == nil {
		return nil
	}

	if slice, ok := flag.Value.(pflag.SliceValue); ok {
		values := slice.GetSlice()
		if values == nil {
			return []string{}
		}
		return values
	}

	switch flag.Value.Type() {
	case "bool":
		parsed, err := strconv.ParseBool(flag.DefValue)
		if err == nil {
			return parsed
		}
	case "int", "int8", "int16", "int32", "int64":
		parsed, err := strconv.ParseInt(flag.DefValue, 10, 64)
		if err == nil {
			return parsed
		}
	case "uint", "uint8", "uint16", "uint32", "uint64":
		parsed, err := strconv.ParseUint(flag.DefValue, 10, 64)
		if err == nil {
			return parsed
		}
	case "float32", "float64":
		parsed, err := strconv.ParseFloat(flag.DefValue, 64)
		if err == nil {
			return parsed
		}
	case "string":
		return flag.DefValue
	case "duration":
		return flag.DefValue
	}

	return flag.DefValue
}

func isDefaultHelpCommand(cmd *cobra.Command) bool {
	return cmd != nil && cmd.Name() == "help" && strings.TrimSpace(cmd.Short) == "Help about any command"
}
