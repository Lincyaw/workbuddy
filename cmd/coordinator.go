package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/Lincyaw/workbuddy/internal/alertbus"
	"github.com/Lincyaw/workbuddy/internal/app"
	"github.com/Lincyaw/workbuddy/internal/audit"
	"github.com/Lincyaw/workbuddy/internal/auditapi"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/metrics"
	"github.com/Lincyaw/workbuddy/internal/operator"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/reporter"
	"github.com/Lincyaw/workbuddy/internal/security"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/tasknotify"
	"github.com/spf13/cobra"
)

// Aliases so cmd-internal call sites (and some infrastructure tests in
// cmd/*_test.go) keep working after the HTTP handlers moved to internal/app.
type (
	fullCoordinatorServer = app.FullCoordinatorServer
	taskResultRequest     = app.TaskResultRequest
	taskHeartbeatRequest  = app.TaskHeartbeatRequest
	taskReleaseRequest    = app.TaskReleaseRequest
	workerRegisterRequest = app.WorkerRegisterRequest
	taskPollResponse      = app.TaskPollResponse
	repoRegisterRequest   = app.RepoRegisterRequest
	repoStatusResponse    = app.RepoStatusResponse
)

const (
	defaultLongPollTimeout = app.DefaultLongPollTimeout
	longPollCheckInterval  = app.LongPollCheckInterval
)

type coordinatorOpts struct {
	dbPath       string
	listenAddr   string
	loopbackOnly bool
	// Fields used by the full coordinator mode (runCoordinatorWithOpts).
	port              int
	pollInterval      time.Duration
	configDir         string
	auth              bool
	trustedAuthors    string
	trustedAuthorsSet bool
}

type tokenCreateOpts struct {
	dbPath   string
	workerID string
	repo     string
	roles    []string
	hostname string
}

type tokenListOpts struct {
	dbPath string
	repo   string
}

type tokenRevokeOpts struct {
	dbPath   string
	workerID string
	kid      string
}

var coordinatorCmd = &cobra.Command{
	Use:   "coordinator",
	Short: "Run the remote coordinator HTTP API (distributed mode)",
	Long: `Start the workbuddy coordinator as a standalone HTTP service. The
coordinator polls GitHub, runs the label-driven state machine, persists
tasks/events/claims in SQLite, and hands tasks to remote workers via
long-poll.

Use this for distributed deployments (coordinator on one host, workers on
others). For single-host development use 'workbuddy serve' instead.

Authentication: pass --auth to require WORKBUDDY_AUTH_TOKEN on the worker
and repo registration endpoints. Use --loopback-only for auth-free local
testing. Use 'workbuddy coordinator token' to mint per-worker tokens.

Multi-repo: register additional repos at runtime with 'workbuddy repo
register' from each repo's root; the coordinator spawns a dedicated poller
per repo.`,
	Example: `  # Local coordinator, loopback-only (auth-free dev mode)
  workbuddy coordinator --listen 127.0.0.1:8081 --loopback-only

  # Production coordinator (bind all interfaces + auth)
  export WORKBUDDY_AUTH_TOKEN=$(openssl rand -hex 24)
  workbuddy coordinator --listen 0.0.0.0:8081 --auth \
    --poll-interval 15s --trusted-authors alice,bob`,
	RunE: runCoordinatorCmd,
}

var coordinatorTokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Manage worker authentication tokens",
}

var coordinatorTokenCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create or rotate a worker token",
	RunE:  runCoordinatorTokenCreateCmd,
}

var coordinatorTokenListCmd = &cobra.Command{
	Use:   "list",
	Short: "List worker token metadata",
	RunE:  runCoordinatorTokenListCmd,
}

var coordinatorTokenRevokeCmd = &cobra.Command{
	Use:   "revoke",
	Short: "Revoke a worker token immediately",
	RunE:  runCoordinatorTokenRevokeCmd,
}

func init() {
	coordinatorCmd.Flags().String("db", ".workbuddy/workbuddy.db", "SQLite database path")
	coordinatorCmd.Flags().String("listen", "127.0.0.1:8081", "Coordinator listen address")
	coordinatorCmd.Flags().Bool("loopback-only", false, "Allow auth-free task endpoints for loopback-only dev mode")
	coordinatorCmd.Flags().String("config-dir", "", "Optional bootstrap config directory for single-repo compatibility")
	coordinatorCmd.Flags().Duration("poll-interval", defaultPollInterval, "GitHub poll interval for managed repos")
	coordinatorCmd.Flags().Int("port", 8081, "Coordinator API port")
	coordinatorCmd.Flags().Bool("auth", false, "Require WORKBUDDY_AUTH_TOKEN for worker and repo registration APIs")
	coordinatorCmd.Flags().String("trusted-authors", "", "Comma-separated GitHub logins allowed to trigger agent work")

	coordinatorTokenCreateCmd.Flags().String("db", ".workbuddy/workbuddy.db", "SQLite database path")
	coordinatorTokenCreateCmd.Flags().String("worker-id", "", "Worker ID")
	coordinatorTokenCreateCmd.Flags().String("repo", "", "Worker repo")
	coordinatorTokenCreateCmd.Flags().StringSlice("roles", nil, "Worker roles")
	coordinatorTokenCreateCmd.Flags().String("hostname", "", "Worker hostname")
	_ = coordinatorTokenCreateCmd.MarkFlagRequired("worker-id")
	_ = coordinatorTokenCreateCmd.MarkFlagRequired("repo")
	_ = coordinatorTokenCreateCmd.MarkFlagRequired("roles")

	coordinatorTokenListCmd.Flags().String("db", ".workbuddy/workbuddy.db", "SQLite database path")
	coordinatorTokenListCmd.Flags().String("repo", "", "Optional repo filter")

	coordinatorTokenRevokeCmd.Flags().String("db", ".workbuddy/workbuddy.db", "SQLite database path")
	coordinatorTokenRevokeCmd.Flags().String("worker-id", "", "Worker ID")
	coordinatorTokenRevokeCmd.Flags().String("kid", "", "Expected key ID")
	_ = coordinatorTokenRevokeCmd.MarkFlagRequired("worker-id")

	coordinatorTokenCmd.AddCommand(coordinatorTokenCreateCmd, coordinatorTokenListCmd, coordinatorTokenRevokeCmd)
	coordinatorCmd.AddCommand(coordinatorTokenCmd)
	rootCmd.AddCommand(coordinatorCmd)
}

func runCoordinatorCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseCoordinatorFlags(cmd)
	if err != nil {
		return err
	}
	return runCoordinatorWithOpts(opts, nil, cmd.Context())
}

func parseCoordinatorFlags(cmd *cobra.Command) (*coordinatorOpts, error) {
	dbPath, _ := cmd.Flags().GetString("db")
	listenAddr, _ := cmd.Flags().GetString("listen")
	loopbackOnly, _ := cmd.Flags().GetBool("loopback-only")
	configDir, _ := cmd.Flags().GetString("config-dir")
	pollInterval, _ := cmd.Flags().GetDuration("poll-interval")
	port, _ := cmd.Flags().GetInt("port")
	authEnabled, _ := cmd.Flags().GetBool("auth")
	trustedAuthors, _ := cmd.Flags().GetString("trusted-authors")
	trustedAuthorsSet := cmd.Flags().Changed("trusted-authors")
	if strings.TrimSpace(listenAddr) == "" {
		return nil, fmt.Errorf("coordinator: --listen is required")
	}
	if loopbackOnly && !isLoopbackListenAddr(listenAddr) {
		return nil, fmt.Errorf("coordinator: --loopback-only requires a loopback --listen address, got %q", listenAddr)
	}
	return &coordinatorOpts{
		dbPath:            dbPath,
		listenAddr:        listenAddr,
		loopbackOnly:      loopbackOnly,
		port:              port,
		pollInterval:      pollInterval,
		configDir:         strings.TrimSpace(configDir),
		auth:              authEnabled,
		trustedAuthors:    trustedAuthors,
		trustedAuthorsSet: trustedAuthorsSet,
	}, nil
}

func isLoopbackListenAddr(listenAddr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(listenAddr))
	if err != nil {
		return false
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func runCoordinatorTokenCreateCmd(cmd *cobra.Command, _ []string) error {
	dbPath, _ := cmd.Flags().GetString("db")
	workerID, _ := cmd.Flags().GetString("worker-id")
	repo, _ := cmd.Flags().GetString("repo")
	roles, _ := cmd.Flags().GetStringSlice("roles")
	hostname, _ := cmd.Flags().GetString("hostname")
	if hostname == "" {
		var err error
		hostname, err = os.Hostname()
		if err != nil {
			hostname = "unknown"
		}
	}

	st, err := store.NewStore(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	issued, err := st.IssueWorkerToken(workerID, repo, roles, hostname)
	if err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "worker_id\t%s\nkid\t%s\ntoken\t%s\n", issued.WorkerID, issued.KID, issued.Token)
	return nil
}

func runCoordinatorTokenListCmd(cmd *cobra.Command, _ []string) error {
	dbPath, _ := cmd.Flags().GetString("db")
	repo, _ := cmd.Flags().GetString("repo")

	st, err := store.NewStore(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	records, err := st.ListWorkerTokens(repo)
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "WORKER ID\tREPO\tKID\tSTATUS\tREVOKED")
	for _, rec := range records {
		revoked := "active"
		if rec.RevokedAt != nil {
			revoked = rec.RevokedAt.Format("2006-01-02T15:04:05Z07:00")
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", rec.WorkerID, rec.Repo, rec.KID, rec.Status, revoked)
	}
	return tw.Flush()
}

func runCoordinatorTokenRevokeCmd(cmd *cobra.Command, _ []string) error {
	dbPath, _ := cmd.Flags().GetString("db")
	workerID, _ := cmd.Flags().GetString("worker-id")
	kid, _ := cmd.Flags().GetString("kid")

	st, err := store.NewStore(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if err := st.RevokeWorkerToken(workerID, kid); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "revoked worker_id=%s kid=%s\n", workerID, kid)
	return nil
}

// --- Full coordinator mode (used by worker tests and the standalone coordinator binary) ---

// runCoordinatorWithOpts composes the distributed coordinator topology:
// store → recovery → security → eventlog → notifier → operator → poller
// manager → coordinator HTTP server. The HTTP surface and the per-repo
// runtime live in internal/app; this function owns flag/config → runtime
// translation and lifecycle orchestration.
func runCoordinatorWithOpts(opts *coordinatorOpts, ghReader poller.GHReader, parentCtx ...context.Context) error {
	bootstrapCfg, err := loadCoordinatorBootstrapConfig(opts)
	if err != nil {
		return err
	}
	port, pollInterval := resolveCoordinatorTiming(opts, bootstrapCfg)

	authToken := strings.TrimSpace(os.Getenv("WORKBUDDY_AUTH_TOKEN"))
	if opts.auth && authToken == "" {
		return fmt.Errorf("coordinator: --auth requires WORKBUDDY_AUTH_TOKEN")
	}
	if !opts.auth && authToken == "" {
		log.Printf("[coordinator] warning: WORKBUDDY_AUTH_TOKEN is not set; worker API is running without authentication")
	}

	st, err := store.NewStore(opts.dbPath)
	if err != nil {
		return fmt.Errorf("coordinator: init store: %w", err)
	}
	defer func() { _ = st.Close() }()

	alertBus := alertbus.NewBus(64)
	if err := app.RecoverTasks(st, alertBus); err != nil {
		log.Printf("[coordinator] warning: recovery failed: %v", err)
	}
	taskHub := tasknotify.NewHub()

	if ghReader == nil {
		ghReader = &app.GHCLIReader{}
	}
	secRuntime, watchSecurityFile, err := security.NewRuntime(security.Options{
		FlagValue: opts.trustedAuthors,
		FlagSet:   opts.trustedAuthorsSet,
		EnvValue:  os.Getenv("WORKBUDDY_TRUSTED_AUTHORS"),
		FilePath:  filepath.Join(mustRepoRoot(), ".workbuddy", "security.yaml"),
	})
	if err != nil {
		return fmt.Errorf("coordinator: load security config: %w", err)
	}
	app.LogSecurityPosture(secRuntime.Current())

	evlog := eventlog.NewEventLogger(st)
	rep := reporter.NewReporter(&reporter.GHCLIWriter{})
	rep.SetEventRecorder(evlog)
	reg := registry.NewRegistry(st, pollInterval)

	ctx, cancel, sigCh := buildRunContext(parentCtx)
	defer cancel()

	var notifCfg config.NotificationsConfig
	if bootstrapCfg != nil {
		notifCfg = bootstrapCfg.Notifications
	}
	notifierRuntime, err := app.NewNotifierRuntime(ctx, notifCfg, alertBus, taskHub, evlog)
	if err != nil {
		return fmt.Errorf("coordinator: init notifier: %w", err)
	}

	startCoordinatorOperatorDetector(ctx, st, alertBus, bootstrapCfg, pollInterval)

	if bootstrapCfg != nil && strings.TrimSpace(bootstrapCfg.Global.Repo) != "" {
		go app.RunRateLimitBudgetCheck(ctx, "coordinator", bootstrapCfg.Global.Repo)
	}

	api := &app.FullCoordinatorServer{
		RootCtx:     ctx,
		Store:       st,
		Registry:    reg,
		Eventlog:    evlog,
		TaskHub:     taskHub,
		Pollers:     app.NewPollerManager(ctx, st, reg, evlog, alertBus, ghReader, rep, mustRepoRoot(), pollInterval, secRuntime),
		AuthEnabled: opts.auth,
		AuthToken:   authToken,
	}
	if watchSecurityFile {
		if err := secRuntime.StartFileWatcher(ctx); err != nil {
			return fmt.Errorf("coordinator: start security watcher: %w", err)
		}
	}
	if err := api.Pollers.LoadExisting(); err != nil {
		return fmt.Errorf("coordinator: load existing registrations: %w", err)
	}
	if err := bootstrapCoordinatorRegistration(ctx, api, bootstrapCfg, st, evlog, reg, notifierRuntime, alertBus, taskHub, opts.configDir); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:    resolveListenAddr(opts.listenAddr, port),
		Handler: buildCoordinatorMux(api, st, evlog, opts.dbPath),
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[coordinator] HTTP server error: %v", err)
		}
	}()

	if sigCh != nil {
		select {
		case sig := <-sigCh:
			log.Printf("[coordinator] received signal %s, shutting down...", sig)
		case <-ctx.Done():
		}
	} else {
		<-ctx.Done()
	}

	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[coordinator] HTTP server shutdown error: %v", err)
	}

	wg.Wait()
	log.Printf("[coordinator] shutdown complete")
	return nil
}

func loadCoordinatorBootstrapConfig(opts *coordinatorOpts) (*config.FullConfig, error) {
	if strings.TrimSpace(opts.configDir) == "" {
		return nil, nil
	}
	cfg, warnings, err := config.LoadConfig(opts.configDir)
	if err != nil {
		return nil, fmt.Errorf("coordinator: load config: %w", err)
	}
	for _, w := range warnings {
		log.Printf("[coordinator] warning: %s", w)
	}
	return cfg, nil
}

func resolveCoordinatorTiming(opts *coordinatorOpts, bootstrapCfg *config.FullConfig) (port int, pollInterval time.Duration) {
	port = opts.port
	if bootstrapCfg != nil && bootstrapCfg.Global.Port > 0 {
		port = bootstrapCfg.Global.Port
	}
	if port <= 0 {
		port = defaultPort
	}
	pollInterval = opts.pollInterval
	if bootstrapCfg != nil && bootstrapCfg.Global.PollInterval > 0 {
		pollInterval = bootstrapCfg.Global.PollInterval
	}
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	return port, pollInterval
}

func startCoordinatorOperatorDetector(ctx context.Context, st *store.Store, alertBus *alertbus.Bus, bootstrapCfg *config.FullConfig, pollInterval time.Duration) {
	operatorCfg := config.OperatorConfig{Enabled: true}
	defaultRepo := ""
	if bootstrapCfg != nil {
		operatorCfg = bootstrapCfg.Operator
		defaultRepo = bootstrapCfg.Global.Repo
	}
	if !operatorCfg.Enabled {
		return
	}
	detector := operator.NewDetector(operator.DetectorOptions{
		Store:                   st,
		Config:                  operatorCfg,
		AlertBus:                alertBus,
		DefaultRepo:             defaultRepo,
		DefaultPollInterval:     pollInterval,
		WorkerHeartbeatInterval: defaultWorkerHeartbeat,
	})
	go func() {
		if err := detector.Run(ctx); err != nil {
			log.Printf("[coordinator] operator detector error: %v", err)
		}
	}()
}

func bootstrapCoordinatorRegistration(
	ctx context.Context,
	api *app.FullCoordinatorServer,
	bootstrapCfg *config.FullConfig,
	st *store.Store,
	evlog *eventlog.EventLogger,
	reg *registry.Registry,
	notifierRuntime *app.NotifierRuntime,
	alertBus *alertbus.Bus,
	taskHub *tasknotify.Hub,
	configDir string,
) error {
	if bootstrapCfg == nil || strings.TrimSpace(bootstrapCfg.Global.Repo) == "" {
		return nil
	}
	rec, err := app.BuildRepoRegistrationRecord(app.BuildRepoRegistrationPayload(bootstrapCfg))
	if err != nil {
		return fmt.Errorf("coordinator: build bootstrap repo registration: %w", err)
	}
	if err := st.UpsertRepoRegistration(rec); err != nil {
		return fmt.Errorf("coordinator: bootstrap repo registration: %w", err)
	}
	if err := api.Pollers.StartOrUpdate(rec); err != nil {
		return fmt.Errorf("coordinator: start bootstrap repo runtime: %w", err)
	}
	api.Config = app.NewCoordinatorConfigRuntime(configDir, bootstrapCfg, st, evlog, api.Pollers, reg, notifierRuntime, alertBus, taskHub)
	if err := app.StartCoordinatorConfigWatcher(ctx, configDir, api.Config); err != nil {
		return fmt.Errorf("coordinator: start config watcher: %w", err)
	}
	return nil
}

func resolveListenAddr(listenAddr string, port int) string {
	if strings.TrimSpace(listenAddr) == "" {
		return fmt.Sprintf(":%d", port)
	}
	return listenAddr
}

func buildCoordinatorMux(api *app.FullCoordinatorServer, st *store.Store, evlog *eventlog.EventLogger, dbPath string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", api.HandleHealth)
	metrics.NewHandler(st).WithEventLogger(evlog).Register(mux)
	readOnlyAudit := audit.NewHTTPHandler(st)
	readOnlyAuditMux := http.NewServeMux()
	readOnlyAudit.Register(readOnlyAuditMux)
	mux.Handle("/events", api.WrapAuth(readOnlyAuditMux))
	mux.Handle("/tasks", api.WrapAuth(readOnlyAuditMux))
	mux.Handle("/issues/", api.WrapAuth(readOnlyAuditMux))
	dashboardAPI := auditapi.NewHandler(st)
	dashboardAPI.SetSessionsDir(filepath.Join(filepath.Dir(dbPath), "sessions"))
	dashboardAPI.RegisterDashboard(mux)
	mux.Handle("/api/v1/repos/register", api.WrapAuth(http.HandlerFunc(api.HandleRegisterRepo)))
	mux.Handle("/api/v1/repos/", api.WrapAuth(http.HandlerFunc(api.HandleRepoByPath)))
	mux.Handle("/api/v1/repos", api.WrapAuth(http.HandlerFunc(api.HandleListRepos)))
	mux.Handle("/api/v1/workers/register", api.WrapAuth(http.HandlerFunc(api.HandleRegisterWorker)))
	mux.Handle("/api/v1/workers/", api.WrapAuth(http.HandlerFunc(api.HandleWorkerByPath)))
	mux.Handle("/api/v1/config/reload", api.WrapAuth(http.HandlerFunc(api.HandleConfigReload)))
	mux.Handle("/api/v1/tasks/poll", api.WrapAuth(http.HandlerFunc(api.HandlePollTask)))
	mux.Handle("/api/v1/tasks/", api.WrapAuth(http.HandlerFunc(api.HandleTaskAction)))
	return mux
}

func mustRepoRoot() string {
	repoRoot, err := os.Getwd()
	if err != nil {
		return "."
	}
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return repoRoot
	}
	return abs
}

