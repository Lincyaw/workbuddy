package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Lincyaw/workbuddy/internal/app"
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

var (
	hooksTestHookName     string
	hooksTestEventFixture string
)

var hooksStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show per-hook invocation stats from a running coordinator",
	Long: `Query the coordinator's /api/v1/hooks/status endpoint and render per-hook
counts (success/failure/filtered/disabled), the last error, and whether the
hook is auto-disabled. Requires --coordinator and a bearer token (env
WORKBUDDY_AUTH_TOKEN or --token).`,
	RunE: runHooksStatus,
}

var hooksReloadCmd = &cobra.Command{
	Use:   "reload",
	Short: "Reload the coordinator's hooks YAML, clearing auto-disable state",
	Long: `POST to the coordinator's /api/v1/hooks/reload endpoint. The coordinator
re-reads its hooks YAML, rebuilds dispatcher bindings (which clears
auto-disable), and emits a hooks_reloaded event. Requires --coordinator and
a bearer token.`,
	RunE: runHooksReload,
}

var hooksTestCmd = &cobra.Command{
	Use:   "test",
	Short: "Fire a hook once with a fixture event (does NOT write eventlog)",
	Long: `Run the action attached to --hook against a v1 payload built from --event-fixture.
The fixture is a JSON file with at least { "type": "...", "repo": "...", "issue_num": N, "payload": {...} }.
Stdout/stderr/exit/duration of the underlying action are printed; nothing is persisted.`,
	RunE: runHooksTest,
}

func init() {
	hooksCmd.PersistentFlags().String(flagHooksConfig, "", "Path to hooks YAML (default: ~/.config/workbuddy/hooks.yaml or $WORKBUDDY_HOOKS_CONFIG)")
	hooksCmd.AddCommand(hooksListCmd)
	hooksTestCmd.Flags().StringVar(&hooksTestHookName, "hook", "", "Name of the hook to fire (required)")
	hooksTestCmd.Flags().StringVar(&hooksTestEventFixture, "event-fixture", "", "Path to a JSON event fixture (required)")
	_ = hooksTestCmd.MarkFlagRequired("hook")
	_ = hooksTestCmd.MarkFlagRequired("event-fixture")
	hooksCmd.AddCommand(hooksTestCmd)

	hooksStatusCmd.Flags().String("coordinator", "", "Coordinator base URL")
	addCoordinatorAuthFlags(hooksStatusCmd.Flags(), "t", "Bearer token for coordinator auth")
	hooksStatusCmd.Flags().Duration("timeout", 15*time.Second, "HTTP timeout")
	addOutputFormatFlag(hooksStatusCmd)
	hooksCmd.AddCommand(hooksStatusCmd)

	hooksReloadCmd.Flags().String("coordinator", "", "Coordinator base URL")
	addCoordinatorAuthFlags(hooksReloadCmd.Flags(), "t", "Bearer token for coordinator auth")
	hooksReloadCmd.Flags().Duration("timeout", 15*time.Second, "HTTP timeout")
	addOutputFormatFlag(hooksReloadCmd)
	hooksCmd.AddCommand(hooksReloadCmd)

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

// fixtureFile is the on-disk shape of `--event-fixture` JSON. Unknown keys
// are tolerated for forward compatibility.
type fixtureFile struct {
	Type     string          `json:"type"`
	Repo     string          `json:"repo"`
	IssueNum int             `json:"issue_num"`
	Payload  json.RawMessage `json:"payload"`
}

func runHooksTest(cmd *cobra.Command, _ []string) error {
	path, _ := cmd.Flags().GetString(flagHooksConfig)
	resolved := ResolveHooksConfigPath(path)
	cfg, warnings, err := hooks.LoadConfig(resolved)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintf(cmdStderr(cmd), "warning: %s\n", w)
	}
	if cfg == nil {
		return fmt.Errorf("no hooks configured at %s", displayHooksPath(resolved))
	}
	var hook *hooks.Hook
	for i := range cfg.Hooks {
		if cfg.Hooks[i].Name == hooksTestHookName {
			hook = &cfg.Hooks[i]
			break
		}
	}
	if hook == nil {
		return fmt.Errorf("hook %q not found in %s", hooksTestHookName, displayHooksPath(resolved))
	}

	raw, err := os.ReadFile(hooksTestEventFixture)
	if err != nil {
		return fmt.Errorf("read fixture: %w", err)
	}
	var fx fixtureFile
	if err := json.Unmarshal(raw, &fx); err != nil {
		return fmt.Errorf("parse fixture %s: %w", hooksTestEventFixture, err)
	}
	if strings.TrimSpace(fx.Type) == "" {
		return fmt.Errorf("fixture %s: missing required field \"type\"", hooksTestEventFixture)
	}

	ev := hooks.Event{
		Type:      fx.Type,
		Repo:      fx.Repo,
		IssueNum:  fx.IssueNum,
		Payload:   []byte(fx.Payload),
		Timestamp: time.Now().UTC(),
	}
	envelope := hooks.BuildEnvelope(ev)
	payload, err := hooks.MarshalEnvelope(envelope)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}

	timeout := hook.Timeout
	if timeout <= 0 {
		timeout = hooks.DefaultHookTimeout
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	out := cmdStdout(cmd)
	switch hook.Action.Type {
	case hooks.ActionTypeCommand:
		action, err := hooks.BuildCommandAction(hook)
		if err != nil {
			return err
		}
		res := action.Run(ctx, ev, payload)
		fmt.Fprintf(out, "hook: %s\naction: command\nduration: %s\nexit: %d\n", hook.Name, res.Duration, res.ExitCode)
		if res.Err != nil {
			fmt.Fprintf(out, "error: %v\n", res.Err)
		}
		fmt.Fprintf(out, "--- stdout ---\n%s", string(res.Stdout))
		if len(res.Stdout) > 0 && res.Stdout[len(res.Stdout)-1] != '\n' {
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "--- stderr ---\n%s", string(res.Stderr))
		if len(res.Stderr) > 0 && res.Stderr[len(res.Stderr)-1] != '\n' {
			fmt.Fprintln(out)
		}
		if res.Err != nil {
			return fmt.Errorf("hook %q failed: %w", hook.Name, res.Err)
		}
		return nil
	default:
		// webhook (and any future generic action) — fall back to the registry.
		action, _, err := hooks.DefaultActionRegistry().Build(hook)
		if err != nil {
			return err
		}
		start := time.Now()
		execErr := action.Execute(ctx, ev, payload)
		fmt.Fprintf(out, "hook: %s\naction: %s\nduration: %s\n", hook.Name, hook.Action.Type, time.Since(start))
		if execErr != nil {
			fmt.Fprintf(out, "error: %v\n", execErr)
			return fmt.Errorf("hook %q failed: %w", hook.Name, execErr)
		}
		fmt.Fprintln(out, "result: ok")
		return nil
	}
}

type hooksRemoteOpts struct {
	coordinator string
	token       string
	timeout     time.Duration
	format      string
}

func parseHooksRemoteFlags(cmd *cobra.Command, name string) (*hooksRemoteOpts, error) {
	coordURL, _ := cmd.Flags().GetString("coordinator")
	if strings.TrimSpace(coordURL) == "" {
		return nil, fmt.Errorf("%s: --coordinator is required", name)
	}
	timeout, _ := cmd.Flags().GetDuration("timeout")
	token, err := resolveCoordinatorAuthToken(cmd, name)
	if err != nil {
		return nil, err
	}
	format, err := resolveOutputFormat(cmd, name)
	if err != nil {
		return nil, err
	}
	return &hooksRemoteOpts{
		coordinator: strings.TrimRight(strings.TrimSpace(coordURL), "/"),
		token:       token,
		timeout:     timeout,
		format:      format,
	}, nil
}

func runHooksStatus(cmd *cobra.Command, _ []string) error {
	opts, err := parseHooksRemoteFlags(cmd, "hooks status")
	if err != nil {
		return err
	}
	body, err := callHooksAPI(cmd.Context(), opts, http.MethodGet, "/api/v1/hooks/status")
	if err != nil {
		return err
	}
	if isJSONOutput(opts.format) {
		_, err = cmdStdout(cmd).Write(body)
		return err
	}
	var resp app.HooksStatusResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("hooks status: decode response: %w", err)
	}
	out := cmdStdout(cmd)
	fmt.Fprintf(out, "config: %s\n", displayHooksPath(resp.ConfigPath))
	fmt.Fprintf(out, "overflow: %d  dropped: %d\n\n", resp.OverflowTotal, resp.DroppedTotal)
	if len(resp.Hooks) == 0 {
		fmt.Fprintln(out, "no hooks registered")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tEVENTS\tACTION\tENABLED\tDISABLED\tOK\tFAIL\tFILTERED\tLAST_ERROR")
	rows := append([]app.HookStatusEntry(nil), resp.Hooks...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	for _, h := range rows {
		enabled := "yes"
		if !h.Enabled {
			enabled = "no"
		}
		disabled := "no"
		if h.AutoDisabled {
			disabled = "yes"
		}
		lastErr := h.LastError
		if len(lastErr) > 60 {
			lastErr = lastErr[:57] + "..."
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\n",
			h.Name, strings.Join(h.Events, ","), h.ActionType, enabled, disabled,
			h.Successes, h.Failures, h.Filtered, lastErr)
	}
	return tw.Flush()
}

func runHooksReload(cmd *cobra.Command, _ []string) error {
	opts, err := parseHooksRemoteFlags(cmd, "hooks reload")
	if err != nil {
		return err
	}
	if err := requireWritable(cmd, "hooks reload"); err != nil {
		return err
	}
	body, err := callHooksAPI(cmd.Context(), opts, http.MethodPost, "/api/v1/hooks/reload")
	if err != nil {
		return err
	}
	if isJSONOutput(opts.format) {
		_, err = cmdStdout(cmd).Write(body)
		return err
	}
	var resp app.HooksReloadResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("hooks reload: decode response: %w", err)
	}
	out := cmdStdout(cmd)
	fmt.Fprintf(out, "reloaded %d hook(s) from %s\n", resp.HookCount, displayHooksPath(resp.ConfigPath))
	for _, w := range resp.Warnings {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
	return nil
}

func callHooksAPI(ctx context.Context, opts *hooksRemoteOpts, method, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, opts.coordinator+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if opts.token != "" {
		req.Header.Set("Authorization", "Bearer "+opts.token)
	}
	client := &http.Client{Timeout: opts.timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, &cliExitError{
			msg:  fmt.Sprintf("coordinator returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body))),
			code: exitCodeFailure,
		}
	}
	return body, nil
}

func displayHooksPath(p string) string {
	if p == "" {
		return "<none>"
	}
	return p
}
