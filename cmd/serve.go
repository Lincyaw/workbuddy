package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Lincyaw/workbuddy/internal/app"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/poller"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/spf13/cobra"
)

const (
	defaultPort             = app.DefaultPort
	defaultPollInterval     = app.DefaultPollInterval
	defaultMaxParallelTasks = app.DefaultMaxParallelTasks
)

type serveOpts struct {
	port              int
	listenAddr        string
	pollInterval      time.Duration
	maxParallelTasks  int
	roles             []string
	configDir         string
	dbPath            string
	auth              bool
	loopbackOnly      bool
	trustedAuthors    string
	trustedAuthorsSet bool
	cookieInsecure    bool
	reportBaseURL     string
	hooksConfig       string
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run coordinator + worker in one process via the distributed HTTP path",
	Long: `Start workbuddy in combined mode.

This command is a thin in-process wrapper around the same coordinator HTTP API
and standalone worker used in split deployments. The worker always talks to
the coordinator over HTTP, even when both run inside one process.`,
	RunE: runServe,
}

func init() {
	serveCmd.Flags().String("listen", "", "Coordinator listen address (default: 127.0.0.1:<port>)")
	serveCmd.Flags().IntP("port", "p", defaultPort, "Coordinator port used when --listen is omitted")
	serveCmd.Flags().Duration("poll-interval", defaultPollInterval, "GitHub poll interval")
	serveCmd.Flags().Int("max-parallel-tasks", 0, fmt.Sprintf("Worker task concurrency override (0 = worker default, min(NumCPU, %d))", defaultMaxParallelTasks))
	serveCmd.Flags().StringSlice("roles", []string{"dev", "test", "review"}, "Worker roles")
	serveCmd.Flags().String("config-dir", ".github/workbuddy", "Configuration directory")
	serveCmd.Flags().String("db-path", ".workbuddy/workbuddy.db", "SQLite database path shared by coordinator and worker")
	serveCmd.Flags().Bool("loopback-only", false, "Allow auth-free task endpoints only when the listen address is loopback-only")
	serveCmd.Flags().Bool("auth", false, "Require WORKBUDDY_AUTH_TOKEN for the coordinator HTTP surface")
	serveCmd.Flags().String("trusted-authors", "", "Comma-separated GitHub logins allowed to trigger agent work")
	serveCmd.Flags().Bool("cookie-insecure", false, "Drop the Secure attribute on session cookies (HTTP reverse-proxy fronts only)")
	serveCmd.Flags().String("report-base-url", "", "Required when --listen is not a loopback address. The URL written into GitHub issue comments as the prefix of session links. Must be reachable from where you'll click the link in a browser. Falls back to WORKBUDDY_REPORT_BASE_URL when unset.")
	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, _ []string) error {
	opts, err := parseServeFlags(cmd)
	if err != nil {
		return err
	}
	if err := requireWritable(cmd, "serve"); err != nil {
		return err
	}
	return runServeWithOutput(opts, nil, nil, cmdStdout(cmd))
}

func parseServeFlags(cmd *cobra.Command) (*serveOpts, error) {
	port, _ := cmd.Flags().GetInt("port")
	listenAddr, _ := cmd.Flags().GetString("listen")
	pollInterval, _ := cmd.Flags().GetDuration("poll-interval")
	maxParallelTasks, _ := cmd.Flags().GetInt("max-parallel-tasks")
	roles, _ := cmd.Flags().GetStringSlice("roles")
	configDir, _ := cmd.Flags().GetString("config-dir")
	dbPath, _ := cmd.Flags().GetString("db-path")
	authEnabled, _ := cmd.Flags().GetBool("auth")
	loopbackOnly, _ := cmd.Flags().GetBool("loopback-only")
	trustedAuthors, _ := cmd.Flags().GetString("trusted-authors")
	trustedAuthorsSet := cmd.Flags().Changed("trusted-authors")
	cookieInsecure, _ := cmd.Flags().GetBool("cookie-insecure")
	reportBaseURL, _ := cmd.Flags().GetString("report-base-url")
	hooksConfig, _ := cmd.Flags().GetString(flagHooksConfig)
	if maxParallelTasks < 0 {
		return nil, fmt.Errorf("serve: --max-parallel-tasks must be >= 0")
	}
	return &serveOpts{
		port:              port,
		listenAddr:        strings.TrimSpace(listenAddr),
		pollInterval:      pollInterval,
		maxParallelTasks:  maxParallelTasks,
		roles:             roles,
		configDir:         configDir,
		dbPath:            dbPath,
		auth:              authEnabled,
		loopbackOnly:      loopbackOnly,
		trustedAuthors:    trustedAuthors,
		trustedAuthorsSet: trustedAuthorsSet,
		cookieInsecure:    cookieInsecure,
		reportBaseURL:     strings.TrimSpace(reportBaseURL),
		hooksConfig:       strings.TrimSpace(hooksConfig),
	}, nil
}

func runServeWithOpts(opts *serveOpts, ghReader poller.GHReader, launcherOverride *runtimepkg.Registry, parentCtx ...context.Context) error {
	return runServeWithOutput(opts, ghReader, launcherOverride, os.Stdout, parentCtx...)
}

func runServeWithOutput(opts *serveOpts, ghReader poller.GHReader, launcherOverride *runtimepkg.Registry, stdout io.Writer, parentCtx ...context.Context) error {
	cfg, err := loadServeConfig(opts)
	if err != nil {
		return err
	}
	if stdout == nil {
		stdout = io.Discard
	}

	repoRoot, err := mustAbsWD()
	if err != nil {
		return fmt.Errorf("serve: resolve repo root: %w", err)
	}
	listenAddr, err := resolveServeListenAddr(opts, cfg)
	if err != nil {
		return err
	}
	if err := validateServeListenSecurity(listenAddr, opts.auth, opts.loopbackOnly); err != nil {
		return err
	}
	baseURL := "http://" + listenAddr
	resolvedReportBaseURL, err := resolveReportBaseURL("serve", listenAddr, opts.reportBaseURL, os.Getenv("WORKBUDDY_REPORT_BASE_URL"))
	if err != nil {
		return err
	}

	ctx, cancel, sigCh := buildRunContext(parentCtx)
	defer cancel()

	coordErrCh := make(chan error, 1)
	go func() {
		coordErrCh <- runCoordinatorWithOpts(&coordinatorOpts{
			dbPath:                opts.dbPath,
			listenAddr:            listenAddr,
			loopbackOnly:          opts.loopbackOnly,
			port:                  cfg.Global.Port,
			pollInterval:          cfg.Global.PollInterval,
			configDir:             opts.configDir,
			auth:                  opts.auth,
			trustedAuthors:        opts.trustedAuthors,
			trustedAuthorsSet:     opts.trustedAuthorsSet,
			cookieInsecure:        opts.cookieInsecure,
			reportBaseURL:         resolvedReportBaseURL,
			hooksConfig:           opts.hooksConfig,
			sharedStoreWithWorker: true, // serve = single-process coord+worker on one DB.
		}, ghReader, ctx)
	}()

	if err := waitForCoordinatorHealth(ctx, baseURL); err != nil {
		cancel()
		return fmt.Errorf("serve: coordinator did not become healthy: %w", err)
	}

	workerErrCh := make(chan error, 1)
	go func() {
		workerErrCh <- runWorkerWithOpts(&workerOpts{
			coordinatorURL:    baseURL,
			token:             strings.TrimSpace(os.Getenv("WORKBUDDY_AUTH_TOKEN")),
			reportBaseURL:     resolvedReportBaseURL,
			mgmtAuthToken:     strings.TrimSpace(os.Getenv("WORKBUDDY_AUTH_TOKEN")),
			roleCSV:           strings.Join(opts.roles, ","),
			configDir:         opts.configDir,
			workDir:           repoRoot,
			sessionsDir:       deriveServeSessionsDir(opts.dbPath, repoRoot),
			dbPath:            opts.dbPath,
			pollTimeout:       defaultWorkerPollTimeout,
			heartbeatInterval: defaultWorkerHeartbeat,
			shutdownTimeout:   defaultWorkerShutdownDeadline,
			concurrency:       serveWorkerConcurrency(opts.maxParallelTasks),
			mgmtAddr:          defaultWorkerMgmtAddr,
		}, launcherOverride, nil, ctx)
	}()

	writeServeBanner(stdout, cfg, opts, listenAddr)

	var serveErr error
	if sigCh != nil {
		select {
		case sig := <-sigCh:
			fmt.Fprintf(stdout, "shutting down after %s\n", sig)
			cancel()
		case err := <-coordErrCh:
			serveErr = err
			cancel()
		case err := <-workerErrCh:
			serveErr = err
			cancel()
		case <-ctx.Done():
		}
	} else {
		select {
		case err := <-coordErrCh:
			serveErr = err
			cancel()
		case err := <-workerErrCh:
			serveErr = err
			cancel()
		case <-ctx.Done():
		}
	}

	coordErr := <-coordErrCh
	workerErr := <-workerErrCh
	if serveErr == nil {
		serveErr = firstNonNilErr(coordErr, workerErr)
	}
	return serveErr
}

func writeServeBanner(stdout io.Writer, cfg *config.FullConfig, opts *serveOpts, listenAddr string) {
	fmt.Fprintf(stdout, "workbuddy serve (repo=%s, roles=[%s], poll=%s, listen=%s)\n",
		cfg.Global.Repo, strings.Join(opts.roles, ","), cfg.Global.PollInterval, listenAddr)
}

func loadServeConfig(opts *serveOpts) (*config.FullConfig, error) {
	cfg, warnings, err := config.LoadConfig(opts.configDir)
	if err != nil {
		return nil, fmt.Errorf("serve: load config: %w", err)
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "[serve] warning: %s\n", w)
	}
	if cfg.Global.Repo == "" {
		return nil, fmt.Errorf("serve: config must specify repo")
	}
	if opts.pollInterval > 0 {
		cfg.Global.PollInterval = opts.pollInterval
	}
	if cfg.Global.PollInterval <= 0 {
		cfg.Global.PollInterval = defaultPollInterval
	}
	if opts.port > 0 {
		cfg.Global.Port = opts.port
	}
	if cfg.Global.Port <= 0 {
		cfg.Global.Port = defaultPort
	}
	return cfg, nil
}

func buildRunContext(parentCtx []context.Context) (context.Context, context.CancelFunc, chan os.Signal) {
	if len(parentCtx) > 0 && parentCtx[0] != nil {
		ctx, cancel := context.WithCancel(parentCtx[0])
		return ctx, cancel, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	return ctx, cancel, sigCh
}

func waitForCoordinatorHealth(ctx context.Context, baseURL string) error {
	client := &http.Client{Timeout: 250 * time.Millisecond}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timeout waiting for %s/health", baseURL)
		case <-ticker.C:
		}
	}
}

func resolveServeListenAddr(opts *serveOpts, cfg *config.FullConfig) (string, error) {
	if opts == nil || cfg == nil {
		return "", fmt.Errorf("serve: options and config are required")
	}
	if opts.listenAddr != "" {
		if host, port, err := net.SplitHostPort(opts.listenAddr); err == nil && port == "0" {
			return reserveListenAddr(host)
		}
		return opts.listenAddr, nil
	}
	return reserveListenAddr("127.0.0.1", cfg.Global.Port)
}

func reserveListenAddr(host string, portOverride ...int) (string, error) {
	port := 0
	if len(portOverride) > 0 {
		port = portOverride[0]
	}
	candidate := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	ln, err := net.Listen("tcp", candidate)
	if err != nil {
		return "", fmt.Errorf("serve: reserve listen addr %s: %w", candidate, err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		return "", fmt.Errorf("serve: close reserved listener %s: %w", addr, err)
	}
	return addr, nil
}

func validateServeListenSecurity(listenAddr string, authEnabled, loopbackOnly bool) error {
	if strings.TrimSpace(listenAddr) == "" {
		return fmt.Errorf("serve: listen address is required")
	}
	if loopbackOnly && !isLoopbackListenAddr(listenAddr) {
		return fmt.Errorf("serve: --loopback-only requires a loopback --listen address, got %q", listenAddr)
	}
	if !authEnabled && !isLoopbackListenAddr(listenAddr) {
		return fmt.Errorf("serve: non-loopback --listen requires --auth, got %q", listenAddr)
	}
	return nil
}

func deriveServeSessionsDir(dbPath, repoRoot string) string {
	trimmed := strings.TrimSpace(dbPath)
	if trimmed == "" {
		return repoRoot + "/.workbuddy/sessions"
	}
	return filepath.Join(filepath.Dir(trimmed), "sessions")
}

func mustAbsWD() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Abs(wd)
}

func firstNonNilErr(errs ...error) error {
	for _, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	return nil
}

func serveWorkerConcurrency(raw int) int {
	if raw > 0 {
		return raw
	}
	return app.DefaultWorkerParallelism()
}
