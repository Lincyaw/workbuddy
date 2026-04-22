package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/spf13/cobra"
)

type repoRegisterOpts struct {
	coordinator string
	token       string
	configDir   string
	timeout     time.Duration
}

type repoListOpts struct {
	coordinator string
	token       string
	jsonOut     bool
	timeout     time.Duration
}

var repoCmd = &cobra.Command{
	Use:   "repo",
	Short: "Manage repo registrations on a running coordinator",
	Long: `Dynamically attach/detach repositories from a running coordinator without
restarting it. 'repo register' serializes the local .github/workbuddy/
directory (config.yaml + agents + workflows) and POSTs it; the coordinator
then spawns a dedicated poller and state machine for that repo. 'repo list'
enumerates what's currently registered.

Run this from inside the target repo's root so the config directory is
discoverable (override with --config-dir).`,
}

var repoRegisterCmd = &cobra.Command{
	Use:   "register",
	Short: "Register the current repo's config with a coordinator",
	Long: `Package .github/workbuddy/{config.yaml,agents,workflows} from the local
repo and register it with the coordinator. Safe to re-run — the coordinator
replaces the existing registration atomically and only starts polling the
new config once validation passes.`,
	Example: `  export WORKBUDDY_AUTH_TOKEN=...
  cd /path/to/my/repo
  workbuddy repo register --coordinator http://coord:8081`,
	RunE: runRepoRegisterCmd,
}

var repoListCmd = &cobra.Command{
	Use:     "list",
	Short:   "List repos currently registered with a coordinator",
	Example: `  workbuddy repo list --coordinator http://coord:8081`,
	RunE:    runRepoListCmd,
}

func init() {
	repoRegisterCmd.Flags().String("coordinator", "", "Coordinator base URL")
	addCoordinatorAuthFlags(repoRegisterCmd.Flags(), "t", "Bearer token for coordinator auth")
	repoRegisterCmd.Flags().String("config-dir", ".github/workbuddy", "Workbuddy config directory")
	repoRegisterCmd.Flags().Duration("timeout", 15*time.Second, "HTTP timeout")
	repoCmd.AddCommand(repoRegisterCmd)

	repoListCmd.Flags().String("coordinator", "", "Coordinator base URL")
	addCoordinatorAuthFlags(repoListCmd.Flags(), "t", "Bearer token for coordinator auth")
	repoListCmd.Flags().Bool("json", false, "Emit machine-readable JSON")
	repoListCmd.Flags().Duration("timeout", 15*time.Second, "HTTP timeout")
	repoCmd.AddCommand(repoListCmd)

	rootCmd.AddCommand(repoCmd)
}

func runRepoRegisterCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseRepoRegisterFlags(cmd)
	if err != nil {
		return err
	}
	payload, err := runRepoRegister(cmd.Context(), opts)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "registered %s\n", payload.Repo)
	return nil
}

func runRepoListCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseRepoListFlags(cmd)
	if err != nil {
		return err
	}
	return runRepoList(cmd.Context(), opts, cmd.OutOrStdout())
}

func parseRepoListFlags(cmd *cobra.Command) (*repoListOpts, error) {
	coordinatorURL, _ := cmd.Flags().GetString("coordinator")
	jsonOut, _ := cmd.Flags().GetBool("json")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	token, err := resolveCoordinatorAuthToken(cmd, "repo list")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(coordinatorURL) == "" {
		return nil, fmt.Errorf("repo list: --coordinator is required")
	}
	return &repoListOpts{
		coordinator: strings.TrimRight(strings.TrimSpace(coordinatorURL), "/"),
		token:       token,
		jsonOut:     jsonOut,
		timeout:     timeout,
	}, nil
}

func runRepoList(ctx context.Context, opts *repoListOpts, stdout io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.coordinator+"/api/v1/repos", nil)
	if err != nil {
		return fmt.Errorf("repo list: build request: %w", err)
	}
	if opts.token != "" {
		req.Header.Set("Authorization", "Bearer "+opts.token)
	}
	client := &http.Client{Timeout: opts.timeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("repo list: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return &cliExitError{msg: fmt.Sprintf("repo list: coordinator returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body))), code: exitCodeFailure}
	}
	if opts.jsonOut {
		_, err = io.Copy(stdout, resp.Body)
		if err != nil {
			return fmt.Errorf("repo list: copy response: %w", err)
		}
		return nil
	}
	var repos []repoStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&repos); err != nil {
		return fmt.Errorf("repo list: decode response: %w", err)
	}
	renderRepoStatusTable(stdout, repos)
	return nil
}

func parseRepoRegisterFlags(cmd *cobra.Command) (*repoRegisterOpts, error) {
	coordinatorURL, _ := cmd.Flags().GetString("coordinator")
	configDir, _ := cmd.Flags().GetString("config-dir")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	token, err := resolveCoordinatorAuthToken(cmd, "repo register")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(coordinatorURL) == "" {
		return nil, fmt.Errorf("repo register: --coordinator is required")
	}
	return &repoRegisterOpts{
		coordinator: strings.TrimRight(strings.TrimSpace(coordinatorURL), "/"),
		token:       token,
		configDir:   strings.TrimSpace(configDir),
		timeout:     timeout,
	}, nil
}

func runRepoRegister(ctx context.Context, opts *repoRegisterOpts) (*repoRegistrationPayload, error) {
	cfg, _, err := config.LoadConfig(opts.configDir)
	if err != nil {
		return nil, err
	}
	payload := buildRepoRegistrationPayload(cfg)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("repo register: marshal config: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.coordinator+"/api/v1/repos/register", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("repo register: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if opts.token != "" {
		req.Header.Set("Authorization", "Bearer "+opts.token)
	}
	client := &http.Client{Timeout: opts.timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("repo register: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("repo register: coordinator returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return payload, nil
}
