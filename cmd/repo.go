package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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

var repoCmd = &cobra.Command{
	Use:   "repo",
	Short: "Manage repo registrations with a coordinator",
}

var repoRegisterCmd = &cobra.Command{
	Use:   "register",
	Short: "Register the current repo config with a coordinator",
	RunE:  runRepoRegisterCmd,
}

func init() {
	repoRegisterCmd.Flags().String("coordinator", "http://127.0.0.1:8081", "Coordinator base URL")
	repoRegisterCmd.Flags().String("token", "", "Bearer token for coordinator auth (defaults to WORKBUDDY_AUTH_TOKEN)")
	repoRegisterCmd.Flags().String("config-dir", ".github/workbuddy", "Workbuddy config directory")
	repoRegisterCmd.Flags().Duration("timeout", 15*time.Second, "HTTP timeout")
	repoCmd.AddCommand(repoRegisterCmd)
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

func parseRepoRegisterFlags(cmd *cobra.Command) (*repoRegisterOpts, error) {
	coordinatorURL, _ := cmd.Flags().GetString("coordinator")
	token, _ := cmd.Flags().GetString("token")
	configDir, _ := cmd.Flags().GetString("config-dir")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	token = strings.TrimSpace(token)
	if token == "" {
		token = strings.TrimSpace(os.Getenv("WORKBUDDY_AUTH_TOKEN"))
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
