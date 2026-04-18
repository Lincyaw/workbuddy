package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Lincyaw/workbuddy/internal/alertbus"
	"github.com/Lincyaw/workbuddy/internal/audit"
	"github.com/Lincyaw/workbuddy/internal/auditapi"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/metrics"
	"github.com/Lincyaw/workbuddy/internal/operator"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/reporter"
	"github.com/Lincyaw/workbuddy/internal/router"
	"github.com/Lincyaw/workbuddy/internal/security"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/tasknotify"
	"github.com/spf13/cobra"
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
	Short: "Run the remote coordinator HTTP API",
	RunE:  runCoordinatorCmd,
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

// --- Full coordinator mode (used by worker tests and future standalone coordinator) ---

const (
	defaultLongPollTimeout = 30 * time.Second
	longPollCheckInterval  = 100 * time.Millisecond
)

type fullCoordinatorServer struct {
	rootCtx     context.Context
	store       *store.Store
	registry    *registry.Registry
	eventlog    *eventlog.EventLogger
	taskHub     *tasknotify.Hub
	pollers     *pollerManager
	config      *coordinatorConfigRuntime
	authEnabled bool
	authToken   string
}

type workerRegisterRequest struct {
	WorkerID string   `json:"worker_id"`
	Repo     string   `json:"repo"`
	Roles    []string `json:"roles"`
	Runtime  string   `json:"runtime,omitempty"`
	Repos    []string `json:"repos,omitempty"`
	Hostname string   `json:"hostname"`
}

type taskPollResponse struct {
	TaskID    string   `json:"task_id"`
	Repo      string   `json:"repo"`
	IssueNum  int      `json:"issue_num"`
	AgentName string   `json:"agent_name"`
	Workflow  string   `json:"workflow,omitempty"`
	State     string   `json:"state,omitempty"`
	Roles     []string `json:"roles,omitempty"`
}

type repoRegisterRequest struct {
	Repo        string                   `json:"repo"`
	Environment string                   `json:"environment,omitempty"`
	Agents      []*config.AgentConfig    `json:"agents"`
	Workflows   []*config.WorkflowConfig `json:"workflows"`
}

type repoStatusResponse struct {
	Repo         string    `json:"repo"`
	Environment  string    `json:"environment"`
	Status       string    `json:"status"`
	PollerStatus string    `json:"poller_status"`
	RegisteredAt time.Time `json:"registered_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type taskResultRequest struct {
	WorkerID      string   `json:"worker_id"`
	Status        string   `json:"status"`
	CurrentLabels []string `json:"current_labels"`
	// InfraFailure flags launcher-layer failures that must NOT be translated
	// into a state-machine failure signal. See issue #131 / AC-3.
	InfraFailure bool   `json:"infra_failure,omitempty"`
	InfraReason  string `json:"infra_reason,omitempty"`
}

type taskHeartbeatRequest struct {
	WorkerID string `json:"worker_id"`
}

type taskReleaseRequest struct {
	WorkerID string `json:"worker_id"`
	Reason   string `json:"reason,omitempty"`
}

// runCoordinatorWithOpts starts a full coordinator (poller + state machine + router + HTTP API).
// It is the backbone for integration tests that pair a worker with a coordinator.
func runCoordinatorWithOpts(opts *coordinatorOpts, ghReader poller.GHReader, parentCtx ...context.Context) error {
	var bootstrapCfg *config.FullConfig
	if strings.TrimSpace(opts.configDir) != "" {
		cfg, warnings, err := config.LoadConfig(opts.configDir)
		if err != nil {
			return fmt.Errorf("coordinator: load config: %w", err)
		}
		for _, w := range warnings {
			log.Printf("[coordinator] warning: %s", w)
		}
		bootstrapCfg = cfg
	}

	port := opts.port
	if bootstrapCfg != nil && bootstrapCfg.Global.Port > 0 {
		port = bootstrapCfg.Global.Port
	}
	if port <= 0 {
		port = defaultPort
	}

	pollInterval := opts.pollInterval
	if bootstrapCfg != nil && bootstrapCfg.Global.PollInterval > 0 {
		pollInterval = bootstrapCfg.Global.PollInterval
	}
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}

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
	if err := recoverTasks(st, alertBus); err != nil {
		log.Printf("[coordinator] warning: recovery failed: %v", err)
	}
	taskHub := tasknotify.NewHub()

	if ghReader == nil {
		ghReader = &GHCLIReader{}
	}
	securityPath := filepath.Join(mustRepoRoot(), ".workbuddy", "security.yaml")
	secRuntime, watchSecurityFile, err := security.NewRuntime(security.Options{
		FlagValue: opts.trustedAuthors,
		FlagSet:   opts.trustedAuthorsSet,
		EnvValue:  os.Getenv("WORKBUDDY_TRUSTED_AUTHORS"),
		FilePath:  securityPath,
	})
	if err != nil {
		return fmt.Errorf("coordinator: load security config: %w", err)
	}
	logSecurityPosture(secRuntime.Current())

	evlog := eventlog.NewEventLogger(st)
	rep := reporter.NewReporter(&reporter.GHCLIWriter{})
	rep.SetEventRecorder(evlog)
	reg := registry.NewRegistry(st, pollInterval)

	var ctx context.Context
	var cancel context.CancelFunc
	var sigCh chan os.Signal
	if len(parentCtx) > 0 && parentCtx[0] != nil {
		ctx, cancel = context.WithCancel(parentCtx[0])
	} else {
		ctx, cancel = context.WithCancel(context.Background())
		sigCh = make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	}
	defer cancel()

	var notifCfg config.NotificationsConfig
	if bootstrapCfg != nil {
		notifCfg = bootstrapCfg.Notifications
	}
	notifierRuntime, err := newNotifierRuntime(ctx, notifCfg, alertBus, taskHub, evlog)
	if err != nil {
		return fmt.Errorf("coordinator: init notifier: %w", err)
	}

	operatorCfg := config.OperatorConfig{Enabled: true}
	defaultRepo := ""
	if bootstrapCfg != nil {
		operatorCfg = bootstrapCfg.Operator
		defaultRepo = bootstrapCfg.Global.Repo
	}
	if operatorCfg.Enabled {
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

	// Optional startup rate-limit budget check; best-effort and non-fatal.
	if bootstrapCfg != nil && strings.TrimSpace(bootstrapCfg.Global.Repo) != "" {
		go runRateLimitBudgetCheck(ctx, "coordinator", bootstrapCfg.Global.Repo)
	}

	api := &fullCoordinatorServer{
		rootCtx:     ctx,
		store:       st,
		registry:    reg,
		eventlog:    evlog,
		taskHub:     taskHub,
		pollers:     newPollerManager(ctx, st, reg, evlog, alertBus, ghReader, rep, mustRepoRoot(), pollInterval, secRuntime),
		authEnabled: opts.auth,
		authToken:   authToken,
	}
	if watchSecurityFile {
		if err := secRuntime.StartFileWatcher(ctx); err != nil {
			return fmt.Errorf("coordinator: start security watcher: %w", err)
		}
	}
	if err := api.pollers.loadExisting(); err != nil {
		return fmt.Errorf("coordinator: load existing registrations: %w", err)
	}
	if bootstrapCfg != nil && strings.TrimSpace(bootstrapCfg.Global.Repo) != "" {
		rec, err := buildRepoRegistrationRecord(buildRepoRegistrationPayload(bootstrapCfg))
		if err != nil {
			return fmt.Errorf("coordinator: build bootstrap repo registration: %w", err)
		}
		if err := st.UpsertRepoRegistration(rec); err != nil {
			return fmt.Errorf("coordinator: bootstrap repo registration: %w", err)
		}
		if err := api.pollers.StartOrUpdate(rec); err != nil {
			return fmt.Errorf("coordinator: start bootstrap repo runtime: %w", err)
		}
		api.config = newCoordinatorConfigRuntime(opts.configDir, bootstrapCfg, st, evlog, api.pollers, reg, notifierRuntime, alertBus, taskHub)
		if err := startCoordinatorConfigWatcher(ctx, opts.configDir, api.config); err != nil {
			return fmt.Errorf("coordinator: start config watcher: %w", err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", api.handleHealth)
	metrics.NewHandler(st).Register(mux)
	readOnlyAudit := audit.NewHTTPHandler(st)
	readOnlyAuditMux := http.NewServeMux()
	readOnlyAudit.Register(readOnlyAuditMux)
	mux.Handle("/events", api.wrapAuth(readOnlyAuditMux))
	mux.Handle("/tasks", api.wrapAuth(readOnlyAuditMux))
	mux.Handle("/issues/", api.wrapAuth(readOnlyAuditMux))
	dashboardAPI := auditapi.NewHandler(st)
	dashboardAPI.SetSessionsDir(filepath.Join(filepath.Dir(opts.dbPath), "sessions"))
	dashboardAPI.RegisterDashboard(mux)
	mux.Handle("/api/v1/repos/register", api.wrapAuth(http.HandlerFunc(api.handleRegisterRepo)))
	mux.Handle("/api/v1/repos/", api.wrapAuth(http.HandlerFunc(api.handleRepoByPath)))
	mux.Handle("/api/v1/repos", api.wrapAuth(http.HandlerFunc(api.handleListRepos)))
	mux.Handle("/api/v1/workers/register", api.wrapAuth(http.HandlerFunc(api.handleRegisterWorker)))
	mux.Handle("/api/v1/workers/", api.wrapAuth(http.HandlerFunc(api.handleWorkerByPath)))
	mux.Handle("/api/v1/config/reload", api.wrapAuth(http.HandlerFunc(api.handleConfigReload)))
	mux.Handle("/api/v1/tasks/poll", api.wrapAuth(http.HandlerFunc(api.handlePollTask)))
	mux.Handle("/api/v1/tasks/", api.wrapAuth(http.HandlerFunc(api.handleTaskAction)))

	listenAddr := opts.listenAddr
	if strings.TrimSpace(listenAddr) == "" {
		listenAddr = fmt.Sprintf(":%d", port)
	}
	srv := &http.Server{
		Addr:    listenAddr,
		Handler: mux,
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

func (s *fullCoordinatorServer) handleConfigReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		coordWriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if s.config == nil {
		coordWriteJSON(w, http.StatusNotFound, map[string]string{"error": "config reload is unavailable without --config-dir bootstrap mode"})
		return
	}
	summary, err := s.config.Reload("manual_api")
	if err != nil {
		coordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	coordWriteJSON(w, http.StatusOK, summary)
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

func (s *fullCoordinatorServer) wrapAuth(next http.Handler) http.Handler {
	if !s.authEnabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		authz := r.Header.Get("Authorization")
		if !strings.HasPrefix(authz, prefix) || strings.TrimSpace(strings.TrimPrefix(authz, prefix)) != s.authToken {
			coordWriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *fullCoordinatorServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	statuses, err := s.pollers.ListStatuses()
	if err != nil {
		coordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	coordWriteJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"repos":  len(statuses),
	})
}

func (s *fullCoordinatorServer) handleRegisterRepo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		coordWriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req repoRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		coordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.Repo = strings.TrimSpace(req.Repo)
	if req.Repo == "" {
		coordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "repo is required"})
		return
	}

	payload := repoRegistrationPayload{
		Repo:        req.Repo,
		Environment: strings.TrimSpace(req.Environment),
		Agents:      req.Agents,
		Workflows:   req.Workflows,
	}
	rec, err := buildRepoRegistrationRecord(&payload)
	if err != nil {
		coordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	prev, err := s.store.GetRepoRegistration(req.Repo)
	if err != nil {
		coordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := s.store.UpsertRepoRegistration(rec); err != nil {
		coordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := s.pollers.StartOrUpdate(rec); err != nil {
		if prev != nil {
			if restoreErr := s.store.UpsertRepoRegistration(*prev); restoreErr != nil {
				coordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("%v (rollback failed: %v)", err, restoreErr)})
				return
			}
		} else {
			if deleteErr := s.store.DeleteRepoRegistration(req.Repo); deleteErr != nil {
				coordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("%v (rollback failed: %v)", err, deleteErr)})
				return
			}
		}
		coordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	coordWriteJSON(w, http.StatusOK, map[string]string{"status": "registered"})
}

func (s *fullCoordinatorServer) handleListRepos(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		coordWriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	statuses, err := s.pollers.ListStatuses()
	if err != nil {
		coordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	resp := make([]repoStatusResponse, 0, len(statuses))
	for _, status := range statuses {
		resp = append(resp, repoStatusResponse{
			Repo:         status.Registration.Repo,
			Environment:  status.Registration.Environment,
			Status:       status.Registration.Status,
			PollerStatus: status.PollerStatus,
			RegisteredAt: status.Registration.RegisteredAt,
			UpdatedAt:    status.Registration.UpdatedAt,
		})
	}
	coordWriteJSON(w, http.StatusOK, resp)
}

func (s *fullCoordinatorServer) handleRepoByPath(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/")
	repo = strings.TrimSpace(repo)
	if repo == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := s.pollers.Deregister(repo); err != nil {
			coordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		coordWriteJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	default:
		coordWriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *fullCoordinatorServer) handleWorkerByPath(w http.ResponseWriter, r *http.Request) {
	workerID := strings.TrimPrefix(r.URL.Path, "/api/v1/workers/")
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := s.registry.Unregister(workerID); err != nil {
			switch {
			case errors.Is(err, registry.ErrWorkerNotFound):
				coordWriteJSON(w, http.StatusNotFound, map[string]string{"error": "worker not found"})
			case errors.Is(err, registry.ErrWorkerHasRunningTask):
				coordWriteJSON(w, http.StatusConflict, map[string]string{"error": "worker has a running task"})
			default:
				coordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			}
			return
		}
		coordWriteJSON(w, http.StatusOK, map[string]string{"status": "unregistered"})
	default:
		coordWriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *fullCoordinatorServer) handleRegisterWorker(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		coordWriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req workerRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		coordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.WorkerID = strings.TrimSpace(req.WorkerID)
	req.Repo = strings.TrimSpace(req.Repo)
	req.Runtime = strings.TrimSpace(req.Runtime)
	if len(req.Repos) == 0 && req.Repo != "" {
		req.Repos = []string{req.Repo}
	}
	if req.Repo == "" && len(req.Repos) > 0 {
		req.Repo = strings.TrimSpace(req.Repos[0])
	}
	if req.WorkerID == "" || req.Repo == "" || len(req.Roles) == 0 {
		coordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id, repo, and roles are required"})
		return
	}
	for _, repo := range req.Repos {
		registered, err := s.pollers.IsRegistered(repo)
		if err != nil {
			coordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if !registered {
			coordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("repo %q is not registered", repo)})
			return
		}
	}
	if err := s.registry.RegisterWithRepos(req.WorkerID, req.Repo, req.Repos, req.Roles, req.Hostname); err != nil {
		coordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.eventlog.Log(eventlog.TypeWorkerRegistered, req.Repo, 0, map[string]any{
		"worker_id": req.WorkerID,
		"roles":     req.Roles,
		"runtime":   req.Runtime,
		"repos":     req.Repos,
		"hostname":  req.Hostname,
	})
	coordWriteJSON(w, http.StatusCreated, map[string]string{"status": "registered"})
}

func (s *fullCoordinatorServer) handlePollTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		coordWriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	workerID := strings.TrimSpace(r.URL.Query().Get("worker_id"))
	if workerID == "" {
		coordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id is required"})
		return
	}
	timeout := parseLongPollTimeout(r.URL.Query().Get("timeout"))
	worker, err := s.lookupWorker(workerID)
	if err != nil {
		coordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if worker == nil {
		coordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown worker"})
		return
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(longPollCheckInterval)
	defer ticker.Stop()

	for {
		task, err := s.claimNextTask(worker)
		switch {
		case err == nil:
		case errors.Is(err, store.ErrTaskClaimConflict):
		default:
			coordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if task != nil {
			_ = s.registry.Heartbeat(worker.ID)
			coordWriteJSON(w, http.StatusOK, task)
			return
		}

		select {
		case <-s.rootCtx.Done():
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			w.WriteHeader(http.StatusNoContent)
			return
		case <-deadline.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-ticker.C:
		}
	}
}

func (s *fullCoordinatorServer) handleTaskAction(w http.ResponseWriter, r *http.Request) {
	taskID, action, ok := parseFullTaskActionPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch {
	case r.Method == http.MethodPost && action == "result":
		s.handleTaskResult(w, r, taskID)
	case r.Method == http.MethodPost && action == "heartbeat":
		s.handleTaskHeartbeat(w, r, taskID)
	case r.Method == http.MethodPost && action == "release":
		s.handleTaskRelease(w, r, taskID)
	default:
		coordWriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *fullCoordinatorServer) handleTaskResult(w http.ResponseWriter, r *http.Request, taskID string) {
	var req taskResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		coordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.WorkerID = strings.TrimSpace(req.WorkerID)
	task, err := s.store.GetTask(taskID)
	if err != nil {
		coordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if task == nil {
		coordWriteJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}
	if req.WorkerID == "" || task.WorkerID != req.WorkerID {
		coordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id does not match claimed task"})
		return
	}
	status := normalizeTaskResultStatus(req.Status)
	if status == "" {
		coordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be completed, failed, or timeout"})
		return
	}
	if err := s.store.UpdateTaskStatus(taskID, status); err != nil {
		coordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := s.registry.Heartbeat(req.WorkerID); err != nil {
		log.Printf("[coordinator] worker heartbeat during result failed: %v", err)
	}
	exitCode := 0
	if status != store.TaskStatusCompleted {
		exitCode = 1
	}
	publishTaskCompletion(s.taskHub, router.WorkerTask{
		TaskID:    task.ID,
		Repo:      task.Repo,
		IssueNum:  task.IssueNum,
		AgentName: task.AgentName,
	}, status, exitCode, time.Now(), time.Now())
	if req.InfraFailure {
		// Launcher-layer failure: the agent never got to decide. Record the
		// infra event for operator visibility, emit the standard completed
		// event for bookkeeping, but DO NOT call MarkAgentCompleted — that
		// would tell the state-machine the agent FAILED, which is the very
		// mis-classification issue #131 is fixing.
		s.eventlog.Log(eventlog.TypeInfraFailure, task.Repo, task.IssueNum, map[string]any{
			"task_id":    task.ID,
			"worker_id":  req.WorkerID,
			"agent_name": task.AgentName,
			"status":     status,
			"reason":     req.InfraReason,
			"source":     "worker_submit",
		})
	} else {
		s.pollers.MarkAgentCompleted(task.Repo, task.IssueNum, task.ID, task.AgentName, exitCode, req.CurrentLabels)
	}
	s.eventlog.Log(eventlog.TypeCompleted, task.Repo, task.IssueNum, map[string]any{
		"task_id":    task.ID,
		"worker_id":  req.WorkerID,
		"agent_name": task.AgentName,
		"status":     status,
	})
	coordWriteJSON(w, http.StatusOK, map[string]string{"status": status})
}

func (s *fullCoordinatorServer) handleTaskHeartbeat(w http.ResponseWriter, r *http.Request, taskID string) {
	var req taskHeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		coordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.WorkerID = strings.TrimSpace(req.WorkerID)
	if req.WorkerID == "" {
		coordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id is required"})
		return
	}
	task, err := s.store.GetTask(taskID)
	if err != nil {
		coordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if task == nil {
		coordWriteJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}
	if task.WorkerID != req.WorkerID {
		coordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id does not match claimed task"})
		return
	}
	if err := s.store.HeartbeatTask(taskID, req.WorkerID, defaultLongPollTimeout); err != nil {
		log.Printf("[coordinator] task heartbeat DB update failed for %s: %v", taskID, err)
	}
	if err := s.registry.Heartbeat(req.WorkerID); err != nil {
		if errors.Is(err, registry.ErrWorkerNotFound) {
			coordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown worker"})
			return
		}
		coordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *fullCoordinatorServer) handleTaskRelease(w http.ResponseWriter, r *http.Request, taskID string) {
	var req taskReleaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		coordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.WorkerID = strings.TrimSpace(req.WorkerID)
	if req.WorkerID == "" {
		coordWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id is required"})
		return
	}
	released, err := s.store.ReleaseTask(taskID, req.WorkerID)
	if err != nil {
		coordWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !released {
		coordWriteJSON(w, http.StatusConflict, map[string]string{"error": "task is not claimable by this worker"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *fullCoordinatorServer) lookupWorker(workerID string) (*store.WorkerRecord, error) {
	workers, err := s.store.QueryWorkers("")
	if err != nil {
		return nil, err
	}
	for _, worker := range workers {
		if worker.ID == workerID {
			return &worker, nil
		}
	}
	return nil, nil
}

func (s *fullCoordinatorServer) claimNextTask(worker *store.WorkerRecord) (*taskPollResponse, error) {
	var roles []string
	if err := json.Unmarshal([]byte(worker.Roles), &roles); err != nil {
		return nil, fmt.Errorf("unmarshal worker roles: %w", err)
	}
	var repos []string
	if err := json.Unmarshal([]byte(worker.ReposJSON), &repos); err != nil || len(repos) == 0 {
		repos = []string{worker.Repo}
	}
	task, err := s.store.ClaimNextTask(worker.ID, roles, repos, "", defaultLongPollTimeout)
	if err != nil || task == nil {
		return nil, err
	}
	s.eventlog.Log(eventlog.TypeDispatch, task.Repo, task.IssueNum, map[string]any{
		"task_id":    task.ID,
		"worker_id":  worker.ID,
		"agent_name": task.AgentName,
	})
	return &taskPollResponse{
		TaskID:    task.ID,
		Repo:      task.Repo,
		IssueNum:  task.IssueNum,
		AgentName: task.AgentName,
		Workflow:  task.Workflow,
		State:     task.State,
		Roles:     append([]string(nil), roles...),
	}, nil
}

func parseLongPollTimeout(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultLongPollTimeout
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	return defaultLongPollTimeout
}

func parseFullTaskActionPath(path string) (taskID string, action string, ok bool) {
	trimmed := strings.TrimPrefix(path, "/api/v1/tasks/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func normalizeTaskResultStatus(raw string) string {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case store.TaskStatusCompleted:
		return store.TaskStatusCompleted
	case store.TaskStatusFailed:
		return store.TaskStatusFailed
	case store.TaskStatusTimeout:
		return store.TaskStatusTimeout
	default:
		return ""
	}
}

func coordWriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("[coordinator] encode response failed: %v", err)
	}
}
