package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/Lincyaw/workbuddy/internal/hooks"
	"github.com/spf13/cobra"
)

const flagHooksConfig = "hooks-config"

var hooksCmd = &cobra.Command{
	Use:   "hooks",
	Short: "Inspect and manage workbuddy hooks (operator-owned event hooks)",
}

var hooksListCmd = &cobra.Command{
	Use:   "list",
	Short: "List hooks configured in ~/.config/workbuddy/hooks.yaml",
	RunE:  runHooksList,
}

func init() {
	hooksCmd.PersistentFlags().String(flagHooksConfig, "", "Path to hooks YAML (default: ~/.config/workbuddy/hooks.yaml or $WORKBUDDY_HOOKS_CONFIG)")
	hooksCmd.AddCommand(hooksListCmd)
	rootCmd.AddCommand(hooksCmd)

	// Make --hooks-config available on serve and coordinator so the loaded
	// dispatcher matches what `workbuddy hooks list` shows.
	serveCmd.Flags().String(flagHooksConfig, "", "Path to hooks YAML (default: ~/.config/workbuddy/hooks.yaml or $WORKBUDDY_HOOKS_CONFIG)")
	coordinatorCmd.Flags().String(flagHooksConfig, "", "Path to hooks YAML (default: ~/.config/workbuddy/hooks.yaml or $WORKBUDDY_HOOKS_CONFIG)")
}

func runHooksList(cmd *cobra.Command, _ []string) error {
	path, _ := cmd.Flags().GetString(flagHooksConfig)
	resolved := ResolveHooksConfigPath(path)
	cfg, warnings, err := hooks.LoadConfig(resolved)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintf(cmdStderr(cmd), "warning: %s\n", w)
	}
	out := cmdStdout(cmd)
	if cfg == nil || len(cfg.Hooks) == 0 {
		fmt.Fprintf(out, "no hooks configured (looked at %s)\n", displayHooksPath(resolved))
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tEVENTS\tACTION\tENABLED")
	hooksList := append([]hooks.Hook(nil), cfg.Hooks...)
	sort.Slice(hooksList, func(i, j int) bool { return hooksList[i].Name < hooksList[j].Name })
	for _, h := range hooksList {
		enabled := "yes"
		if !h.IsEnabled() {
			enabled = "no"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", h.Name, strings.Join(h.Events, ","), h.Action.Type, enabled)
	}
	return tw.Flush()
}

// ResolveHooksConfigPath picks the effective hooks config path with this
// precedence: explicit flag > $WORKBUDDY_HOOKS_CONFIG env var > default at
// ~/.config/workbuddy/hooks.yaml. Returns "" only if the home directory
// cannot be resolved AND no override is set.
func ResolveHooksConfigPath(flagValue string) string {
	if v := strings.TrimSpace(flagValue); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("WORKBUDDY_HOOKS_CONFIG")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "workbuddy", "hooks.yaml")
}

func displayHooksPath(p string) string {
	if p == "" {
		return "<none>"
	}
	return p
}
