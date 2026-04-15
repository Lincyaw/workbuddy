package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
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

const (
	defaultLongPollTimeout = 30 * time.Second
	longPollCheckInterval  = 100 * time.Millisecond
)

type coordinatorOpts struct {
	port         int
	pollInterval time.Duration
	configDir    string
	dbPath       string
	auth         bool
}

type coordinatorServer struct {
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

var coordinatorCmd = &cobra.Command{
	Use:   "coordinator",
	Short: "Run Coordinator only (v0.2.0 distributed mode)",
	Long:  "Start the standalone Coordinator process with poller, state machine, task router, and worker HTTP API.",
	RunE:  runCoordinator,
}

func init() {
	coordinatorCmd.Flags().IntP("port", "p", defaultPort, "HTTP server port")
	coordinatorCmd.Flags().Duration("poll-interval", defaultPollInterval, "GitHub poll interval")
	coordinatorCmd.Flags().String("config-dir", ".github/workbuddy", "Configuration directory")
	coordinatorCmd.Flags().String("db-path", ".workbuddy/workbuddy.db", "SQLite database path")
	coordinatorCmd.Flags().Bool("auth", false, "Require Bearer authentication for worker API requests")
	rootCmd.AddCommand(coordinatorCmd)
}

func runCoordinator(cmd *cobra.Command, _ []string) error {
	opts, err := parseCoordinatorFlags(cmd)
	if err != nil {
		return err
	}
	return runCoordinatorWithOpts(opts, nil)
}

func parseCoordinatorFlags(cmd *cobra.Command) (*coordinatorOpts, error) {
	port, _ := cmd.Flags().GetInt("port")
	pollInterval, _ := cmd.Flags().GetDuration("poll-interval")
	configDir, _ := cmd.Flags().GetString("config-dir")
	dbPath, _ := cmd.Flags().GetString("db-path")
	authEnabled, _ := cmd.Flags().GetBool("auth")
	return &coordinatorOpts{
		port:         port,
		pollInterval: pollInterval,
		configDir:    configDir,
		dbPath:       dbPath,
		auth:         authEnabled,
	}, nil
}

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
	rt := router.NewRouter(cfg.Agents, reg, st, cfg.Global.Repo, mustRepoRoot(), nil, nil)
	rep := reporter.NewReporter(&reporter.GHCLIWriter{})
	p := poller.NewPoller(ghReader, st, cfg.Global.Repo, cfg.Global.PollInterval)

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

	api := &coordinatorServer{
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
			if err := depResolver.EvaluateOpenIssues(runCtx, cfg.Global.Repo, depGraphVersion); err != nil {
				log.Printf("[coordinator] dependency resolver error: %v", err)
				return
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

	fmt.Printf("workbuddy coordinator (repo=%s, poll=%s, port=%d, auth=%t)\n",
		cfg.Global.Repo, cfg.Global.PollInterval, cfg.Global.Port, opts.auth)

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

func (s *coordinatorServer) wrapAuth(next http.Handler) http.Handler {
	if !s.authEnabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		authz := r.Header.Get("Authorization")
		if !strings.HasPrefix(authz, prefix) || strings.TrimSpace(strings.TrimPrefix(authz, prefix)) != s.authToken {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *coordinatorServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
		"repo":   s.repo,
	})
}

func (s *coordinatorServer) handleRegisterWorker(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req workerRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.WorkerID = strings.TrimSpace(req.WorkerID)
	req.Repo = strings.TrimSpace(req.Repo)
	if req.WorkerID == "" || req.Repo == "" || len(req.Roles) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id, repo, and roles are required"})
		return
	}
	if err := s.registry.Register(req.WorkerID, req.Repo, req.Roles, req.Hostname); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.eventlog.Log(eventlog.TypeWorkerRegistered, req.Repo, 0, map[string]any{
		"worker_id": req.WorkerID,
		"roles":     req.Roles,
		"hostname":  req.Hostname,
	})
	writeJSON(w, http.StatusCreated, map[string]string{"status": "registered"})
}

func (s *coordinatorServer) handlePollTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	workerID := strings.TrimSpace(r.URL.Query().Get("worker_id"))
	if workerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id is required"})
		return
	}
	timeout := parseLongPollTimeout(r.URL.Query().Get("timeout"))
	worker, err := s.lookupWorker(workerID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if worker == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown worker"})
		return
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(longPollCheckInterval)
	defer ticker.Stop()

	for {
		task, err := s.claimNextTask(worker)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if task != nil {
			_ = s.registry.Heartbeat(worker.ID)
			writeJSON(w, http.StatusOK, task)
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

func (s *coordinatorServer) handleTaskAction(w http.ResponseWriter, r *http.Request) {
	taskID, action, ok := parseTaskActionPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch {
	case r.Method == http.MethodPost && action == "result":
		s.handleTaskResult(w, r, taskID)
	case r.Method == http.MethodPost && action == "heartbeat":
		s.handleTaskHeartbeat(w, r, taskID)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *coordinatorServer) handleTaskResult(w http.ResponseWriter, r *http.Request, taskID string) {
	var req taskResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.WorkerID = strings.TrimSpace(req.WorkerID)
	task, err := s.store.GetTask(taskID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if task == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}
	if req.WorkerID == "" || task.WorkerID != req.WorkerID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id does not match claimed task"})
		return
	}
	status := normalizeTaskResultStatus(req.Status)
	if status == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be completed, failed, or timeout"})
		return
	}
	if err := s.store.UpdateTaskStatus(taskID, status); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := s.registry.Heartbeat(req.WorkerID); err != nil {
		log.Printf("[coordinator] worker heartbeat during result failed: %v", err)
	}
	s.stateMachine.MarkAgentCompleted(task.Repo, task.IssueNum, req.CurrentLabels)
	s.eventlog.Log(eventlog.TypeCompleted, task.Repo, task.IssueNum, map[string]any{
		"task_id":    task.ID,
		"worker_id":  req.WorkerID,
		"agent_name": task.AgentName,
		"status":     status,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

func (s *coordinatorServer) handleTaskHeartbeat(w http.ResponseWriter, r *http.Request, taskID string) {
	var req taskHeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.WorkerID = strings.TrimSpace(req.WorkerID)
	if req.WorkerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id is required"})
		return
	}
	task, err := s.store.GetTask(taskID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if task == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}
	if task.WorkerID != req.WorkerID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id does not match claimed task"})
		return
	}
	if err := s.registry.Heartbeat(req.WorkerID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *coordinatorServer) lookupWorker(workerID string) (*store.WorkerRecord, error) {
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

func (s *coordinatorServer) claimNextTask(worker *store.WorkerRecord) (*taskPollResponse, error) {
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

func parseTaskActionPath(path string) (taskID string, action string, ok bool) {
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

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("[coordinator] encode response failed: %v", err)
	}
}
