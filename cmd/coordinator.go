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
	"slices"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Lincyaw/workbuddy/internal/auditapi"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/coordinator"
	"github.com/Lincyaw/workbuddy/internal/dependency"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/reporter"
	"github.com/Lincyaw/workbuddy/internal/router"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/spf13/cobra"
)

type coordinatorOpts struct {
	dbPath       string
	listenAddr   string
	loopbackOnly bool
	// Fields used by the full coordinator mode (runCoordinatorWithOpts).
	port         int
	pollInterval time.Duration
	configDir    string
	auth         bool
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

	st, err := store.NewStore(opts.dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	handler := coordinator.NewServer(st, coordinator.ServerOptions{
		LoopbackOnly: opts.loopbackOnly,
	})
	srv := &http.Server{
		Addr:    opts.listenAddr,
		Handler: handler,
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		ln, err := net.Listen("tcp", opts.listenAddr)
		if err != nil {
			errCh <- err
			return
		}
		errCh <- srv.Serve(ln)
	}()

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	case <-ctx.Done():
		return srv.Shutdown(context.Background())
	}
}

func parseCoordinatorFlags(cmd *cobra.Command) (*coordinatorOpts, error) {
	dbPath, _ := cmd.Flags().GetString("db")
	listenAddr, _ := cmd.Flags().GetString("listen")
	loopbackOnly, _ := cmd.Flags().GetBool("loopback-only")
	if strings.TrimSpace(listenAddr) == "" {
		return nil, fmt.Errorf("coordinator: --listen is required")
	}
	if loopbackOnly && !isLoopbackListenAddr(listenAddr) {
		return nil, fmt.Errorf("coordinator: --loopback-only requires a loopback --listen address, got %q", listenAddr)
	}
	return &coordinatorOpts{
		dbPath:       dbPath,
		listenAddr:   listenAddr,
		loopbackOnly: loopbackOnly,
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
	rootCtx      context.Context
	store        *store.Store
	registry     *registry.Registry
	stateMachine *statemachine.StateMachine
	eventlog     *eventlog.EventLogger
	agents       map[string]*config.AgentConfig
	repo         string
	authEnabled  bool
	authToken    string
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

type taskResultRequest struct {
	WorkerID      string   `json:"worker_id"`
	Status        string   `json:"status"`
	CurrentLabels []string `json:"current_labels"`
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
	cfg, warnings, err := config.LoadConfig(opts.configDir)
	if err != nil {
		return fmt.Errorf("coordinator: load config: %w", err)
	}
	for _, w := range warnings {
		log.Printf("[coordinator] warning: %s", w)
	}
	if cfg.Global.Repo == "" {
		return fmt.Errorf("coordinator: config must specify repo")
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

	if err := recoverTasks(st); err != nil {
		log.Printf("[coordinator] warning: recovery failed: %v", err)
	}

	if ghReader == nil {
		ghReader = &GHCLIReader{}
	}

	evlog := eventlog.NewEventLogger(st)
	reg := registry.NewRegistry(st, cfg.Global.PollInterval)
	dispatchCh := make(chan statemachine.DispatchRequest, dispatchChanSize)
	sm := statemachine.NewStateMachine(cfg.Workflows, st, dispatchCh, evlog)
	depResolver := dependency.NewResolver(st, ghReader, evlog)
	rt := router.NewRouter(cfg.Agents, reg, st, cfg.Global.Repo, mustRepoRoot(), nil, nil, false)
	rep := reporter.NewReporter(&reporter.GHCLIWriter{})
	rep.SetEventRecorder(evlog)
	p := poller.NewPoller(ghReader, st, cfg.Global.Repo, cfg.Global.PollInterval)
	p.SetEventRecorder(evlog)

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

	// Optional startup rate-limit budget check; best-effort and non-fatal.
	go runRateLimitBudgetCheck(ctx, "coordinator", cfg.Global.Repo)

	api := &fullCoordinatorServer{
		rootCtx:      ctx,
		store:        st,
		registry:     reg,
		stateMachine: sm,
		eventlog:     evlog,
		agents:       cfg.Agents,
		repo:         cfg.Global.Repo,
		authEnabled:  opts.auth,
		authToken:    authToken,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", api.handleHealth)
	dashboardAPI := auditapi.NewHandler(st)
	dashboardAPI.SetSessionsDir(filepath.Join(filepath.Dir(opts.dbPath), "sessions"))
	dashboardAPI.RegisterDashboard(mux)
	mux.Handle("/api/v1/workers/register", api.wrapAuth(http.HandlerFunc(api.handleRegisterWorker)))
	mux.Handle("/api/v1/tasks/poll", api.wrapAuth(http.HandlerFunc(api.handlePollTask)))
	mux.Handle("/api/v1/tasks/", api.wrapAuth(http.HandlerFunc(api.handleTaskAction)))

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Global.Port),
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

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := p.Run(ctx); err != nil {
			log.Printf("[coordinator] poller error: %v", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		var depsResolvedThisCycle bool
		var depGraphVersion int64
		runDependencyMaintenance := func(runCtx context.Context) {
			depGraphVersion++
			unblockedIssues, err := depResolver.EvaluateOpenIssues(runCtx, cfg.Global.Repo, depGraphVersion)
			if err != nil {
				log.Printf("[coordinator] dependency resolver error: %v", err)
				return
			}
			for _, issueNum := range unblockedIssues {
				if delErr := st.DeleteIssueCache(cfg.Global.Repo, issueNum); delErr != nil {
					log.Printf("[coordinator] dependency unblock cache-invalidate #%d: %v", issueNum, delErr)
				} else {
					log.Printf("[coordinator] dependency unblocked #%d — cache invalidated for redispatch", issueNum)
				}
			}
			caches, err := st.ListIssueCaches(cfg.Global.Repo)
			if err != nil {
				log.Printf("[coordinator] dependency reaction list-caches error: %v", err)
				return
			}
			for _, cached := range caches {
				if cached.State != "open" {
					continue
				}
				state, err := st.QueryIssueDependencyState(cached.Repo, cached.IssueNum)
				if err != nil {
					log.Printf("[coordinator] dependency reaction query state %s#%d: %v", cached.Repo, cached.IssueNum, err)
					continue
				}
				if state == nil {
					continue
				}
				wantBlocked := state.Verdict == store.DependencyVerdictBlocked || state.Verdict == store.DependencyVerdictNeedsHuman
				if wantBlocked == state.LastReactionBlocked {
					continue
				}
				if err := rep.SetBlockedReaction(runCtx, cached.Repo, cached.IssueNum, wantBlocked); err != nil {
					log.Printf("[coordinator] dependency reaction set %s#%d blocked=%v: %v", cached.Repo, cached.IssueNum, wantBlocked, err)
					continue
				}
				if err := st.MarkDependencyReactionApplied(cached.Repo, cached.IssueNum, wantBlocked); err != nil {
					log.Printf("[coordinator] dependency reaction mark %s#%d: %v", cached.Repo, cached.IssueNum, err)
				}
			}
		}

		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-p.Events():
				if !ok {
					return
				}
				if ev.Type == poller.EventPollCycleDone {
					evlog.Log(poller.EventPollCycleDone, ev.Repo, 0, map[string]any{"source": "poller"})
					if !depsResolvedThisCycle {
						runDependencyMaintenance(ctx)
					}
					depsResolvedThisCycle = false
					sm.ResetDedup()
					continue
				}
				if !depsResolvedThisCycle {
					runDependencyMaintenance(ctx)
					depsResolvedThisCycle = true
				}
				if err := sm.HandleEvent(ctx, statemachine.ChangeEvent{
					Type:     ev.Type,
					Repo:     ev.Repo,
					IssueNum: ev.IssueNum,
					Labels:   ev.Labels,
					Detail:   ev.Detail,
				}); err != nil {
					log.Printf("[coordinator] state machine error: %v", err)
				}
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := rt.Run(ctx, dispatchCh); err != nil {
			log.Printf("[coordinator] router error: %v", err)
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
	coordWriteJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
		"repo":   s.repo,
	})
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
	if err := s.registry.Register(req.WorkerID, req.Repo, req.Roles, req.Hostname); err != nil {
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
		if err != nil {
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
	s.stateMachine.MarkAgentCompleted(task.Repo, task.IssueNum, task.ID, task.AgentName, exitCode, req.CurrentLabels)
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
	if err := s.registry.Heartbeat(req.WorkerID); err != nil {
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
	pending, err := s.store.QueryTasks(store.TaskStatusPending)
	if err != nil {
		return nil, err
	}
	for _, task := range pending {
		if task.Repo != worker.Repo {
			continue
		}
		agent, ok := s.agents[task.AgentName]
		if !ok {
			continue
		}
		if !slices.Contains(roles, agent.Role) {
			continue
		}
		claimed, err := s.store.ClaimTask(task.ID, worker.ID)
		if err != nil {
			return nil, err
		}
		if !claimed {
			continue
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
			Roles:     append([]string(nil), roles...),
		}, nil
	}
	return nil, nil
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
