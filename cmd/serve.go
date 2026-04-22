package cmd

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Lincyaw/workbuddy/internal/alertbus"
	"github.com/Lincyaw/workbuddy/internal/app"
	"github.com/Lincyaw/workbuddy/internal/audit"
	"github.com/Lincyaw/workbuddy/internal/auditapi"
	"github.com/Lincyaw/workbuddy/internal/config"
	coordinatorhttp "github.com/Lincyaw/workbuddy/internal/coordinator/http"
	"github.com/Lincyaw/workbuddy/internal/dependency"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	"github.com/Lincyaw/workbuddy/internal/metrics"
	"github.com/Lincyaw/workbuddy/internal/operator"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/reporter"
	"github.com/Lincyaw/workbuddy/internal/router"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/security"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/tasknotify"
	"github.com/Lincyaw/workbuddy/internal/webui"
	workerexec "github.com/Lincyaw/workbuddy/internal/worker"
	workersession "github.com/Lincyaw/workbuddy/internal/worker/session"
	"github.com/Lincyaw/workbuddy/internal/workspace"
	"github.com/spf13/cobra"
)

// cmd-local defaults kept for back-compat with existing Cobra flag defaults
// and tests that still reference them.
const (
	defaultPort             = app.DefaultPort
	defaultPollInterval     = app.DefaultPollInterval
	defaultMaxParallelTasks = app.DefaultMaxParallelTasks
	taskChanSize            = app.TaskChanSize
	dispatchChanSize        = app.DispatchChanSize
	agentShutdownWait       = app.AgentShutdownWait
)

// Aliases to the app package so tests and other cmd files keep compiling.
type (
	RunningTasks = app.RunningTasks
	closedIssues = app.ClosedIssues
	GHCLIReader  = app.GHCLIReader
)

var (
	NewRunningTasks     = app.NewRunningTasks
	recoverTasks        = app.RecoverTasks
	allowSecurityEvent  = app.AllowSecurityEvent
	logSecurityPosture  = app.LogSecurityPosture
	newTaskWatchHandler = app.NewTaskWatchHandler
)

// serveOpts holds parsed CLI flags for the serve command. Flag parsing and
// Cobra wiring stay here; the actual composition lives in runServeWithOpts,
// which delegates the assembly graph to internal/app-level helpers.
type serveOpts struct {
	port              int
	pollInterval      time.Duration
	maxParallelTasks  int
	roles             []string
	configDir         string
	dbPath            string
	coordinatorAPI    bool
	trustedAuthors    string
	trustedAuthorsSet bool
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run Coordinator + Worker in single process (v0.1.0)",
	Long: `Start workbuddy in single-process mode (v0.1.0).

Runs the Coordinator (Poller + StateMachine + TaskRouter) and an embedded
Worker in the same process. Communication is via Go channels.`,
	RunE: runServe,
}

func init() {
	serveCmd.Flags().IntP("port", "p", defaultPort, "HTTP server port")
	serveCmd.Flags().Duration("poll-interval", defaultPollInterval, "GitHub poll interval")
	serveCmd.Flags().Int("max-parallel-tasks", 0, fmt.Sprintf("Maximum number of embedded worker tasks to run in parallel across issues (0 = auto, min(NumCPU, %d))", defaultMaxParallelTasks))
	serveCmd.Flags().StringSlice("roles", []string{"dev", "test", "review"}, "Worker roles")
	serveCmd.Flags().String("config-dir", ".github/workbuddy", "Configuration directory")
	serveCmd.Flags().String("db-path", ".workbuddy/workbuddy.db", "SQLite database path")
	serveCmd.Flags().Bool("coordinator-api", false, "Expose coordinator task claim API and persist tasks for remote workers")
	serveCmd.Flags().String("trusted-authors", "", "Comma-separated GitHub logins allowed to trigger agent work")
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
	return runServeWithOpts(opts, nil, nil)
}

func parseServeFlags(cmd *cobra.Command) (*serveOpts, error) {
	port, _ := cmd.Flags().GetInt("port")
	pollInterval, _ := cmd.Flags().GetDuration("poll-interval")
	maxParallelTasks, _ := cmd.Flags().GetInt("max-parallel-tasks")
	roles, _ := cmd.Flags().GetStringSlice("roles")
	configDir, _ := cmd.Flags().GetString("config-dir")
	dbPath, _ := cmd.Flags().GetString("db-path")
	coordinatorAPI, _ := cmd.Flags().GetBool("coordinator-api")
	trustedAuthors, _ := cmd.Flags().GetString("trusted-authors")
	trustedAuthorsSet := cmd.Flags().Changed("trusted-authors")
	if maxParallelTasks < 0 {
		return nil, fmt.Errorf("serve: --max-parallel-tasks must be >= 0")
	}
	return &serveOpts{
		port:              port,
		pollInterval:      pollInterval,
		maxParallelTasks:  maxParallelTasks,
		roles:             roles,
		configDir:         configDir,
		dbPath:            dbPath,
		coordinatorAPI:    coordinatorAPI,
		trustedAuthors:    trustedAuthors,
		trustedAuthorsSet: trustedAuthorsSet,
	}, nil
}

// runServeWithOpts composes the single-process serve topology: store →
// recovery+security → eventlog → (optional coordinator API) → poller → state
// machine → router → embedded worker. Kept in cmd/ because tests depend on
// its signature and on the injection points (ghReader, launcherOverride,
// parentCtx); the individual pieces it assembles live in internal/app.
func runServeWithOpts(opts *serveOpts, ghReader poller.GHReader, launcherOverride *runtimepkg.Registry, parentCtx ...context.Context) error {
	cfg, err := loadServeConfig(opts)
	if err != nil {
		return err
	}

	st, err := store.NewStore(opts.dbPath)
	if err != nil {
		return fmt.Errorf("serve: init store: %w", err)
	}
	defer func() { _ = st.Close() }()
	alertBus := alertbus.NewBus(64)
	if err := app.RecoverCoordinatorIssueClaims(st, os.Getpid()); err != nil {
		log.Printf("[serve] warning: issue-claim recovery failed: %v", err)
	}
	if err := app.RecoverTasks(st, alertBus); err != nil {
		log.Printf("[serve] warning: recovery failed: %v", err)
	}

	evlog := eventlog.NewEventLogger(st)
	reg := registry.NewRegistry(st, cfg.Global.PollInterval)

	repoDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("serve: get working directory: %w", err)
	}
	secRuntime, watchSecurityFile, err := security.NewRuntime(security.Options{
		FlagValue: opts.trustedAuthors,
		FlagSet:   opts.trustedAuthorsSet,
		EnvValue:  os.Getenv("WORKBUDDY_TRUSTED_AUTHORS"),
		FilePath:  filepath.Join(repoDir, ".workbuddy", "security.yaml"),
	})
	if err != nil {
		return fmt.Errorf("serve: load security config: %w", err)
	}
	app.LogSecurityPosture(secRuntime.Current())
	sessionsDir := filepath.Join(repoDir, ".workbuddy", "sessions")
	recorder := workersession.NewRecorder(st, sessionsDir)

	var workerID string
	if !opts.coordinatorAPI {
		workerID, err = reg.RegisterEmbedded(cfg.Global.Repo, opts.roles)
		if err != nil {
			return fmt.Errorf("serve: register embedded worker: %w", err)
		}
	}

	dispatchCh := make(chan statemachine.DispatchRequest, dispatchChanSize)
	var taskCh chan router.WorkerTask
	if !opts.coordinatorAPI {
		taskCh = make(chan router.WorkerTask, taskChanSize)
	}

	if ghReader == nil {
		ghReader = &app.GHCLIReader{}
	}
	var labelReader issueLabelReader
	if reader, ok := ghReader.(issueLabelReader); ok {
		labelReader = reader
	}

	sm := statemachine.NewStateMachine(cfg.Workflows, st, dispatchCh, evlog, alertBus)
	claimerID := workerID
	if claimerID == "" {
		claimerID = "coordinator-" + app.HostnameOrUnknown()
	}
	sm.SetIssueClaim(app.BuildIssueClaimerID(claimerID, os.Getpid()), statemachine.DefaultIssueClaimLease)
	depResolver := dependency.NewResolver(st, ghReader, evlog, alertBus)

	var wsMgr *workspace.Manager
	if !opts.coordinatorAPI {
		wsMgr = workspace.NewManager(repoDir)
		_ = wsMgr.Prune()
	}

	rt := router.NewRouter(cfg.Agents, reg, st, cfg.Global.Repo, repoDir, taskCh, wsMgr, !opts.coordinatorAPI)
	if issueDataReader, ok := ghReader.(router.IssueDataReader); ok {
		rt.SetIssueDataReader(issueDataReader)
	}

	lnch := launcherOverride
	if lnch == nil {
		lnch = launcher.NewLauncher()
	}
	lnch.SetSessionManager(runtimepkg.NewSessionManager(sessionsDir, st))

	rep := reporter.NewReporter(&reporter.GHCLIWriter{})
	rep.SetBaseURL(fmt.Sprintf("http://localhost:%d", cfg.Global.Port))
	rep.SetEventRecorder(recorder)
	rep.SetVerifier(reporter.NewGHClaimVerifier())
	// Note: router no longer holds a Reporter — worktree-failure reporting
	// now lives on the worker side (see internal/worker/executor.go). The
	// reporter wired above is used by the poller/worker paths.

	p := poller.NewPoller(ghReader, st, cfg.Global.Repo, cfg.Global.PollInterval)
	p.SetEventRecorder(evlog)

	ctx, cancel, sigCh := buildRunContext(parentCtx)
	defer cancel()
	if watchSecurityFile {
		if err := secRuntime.StartFileWatcher(ctx); err != nil {
			return fmt.Errorf("serve: start security watcher: %w", err)
		}
	}

	go app.RunRateLimitBudgetCheck(ctx, "serve", cfg.Global.Repo)

	var wg sync.WaitGroup
	taskHub := tasknotify.NewHub()
	// In serve mode we don't expose live config reload — just start the
	// notifier with the loaded config; ctx cancellation stops it.
	if _, err := app.NewNotifierRuntime(ctx, cfg.Notifications, alertBus, taskHub, evlog); err != nil {
		return fmt.Errorf("serve: init notifier: %w", err)
	}

	if cfg.Operator.Enabled {
		detector := operator.NewDetector(operator.DetectorOptions{
			Store:                   st,
			Config:                  cfg.Operator,
			AlertBus:                alertBus,
			DefaultRepo:             cfg.Global.Repo,
			DefaultPollInterval:     cfg.Global.PollInterval,
			WorkerHeartbeatInterval: defaultWorkerHeartbeat,
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := detector.Run(ctx); err != nil {
				log.Printf("[serve] operator detector error: %v", err)
			}
		}()
	}

	mux := newServeMux(st, cfg, evlog, sessionsDir, taskHub, opts.coordinatorAPI)
	srv := &http.Server{Addr: fmt.Sprintf(":%d", cfg.Global.Port), Handler: mux}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[serve] HTTP server error: %v", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := p.Run(ctx); err != nil {
			log.Printf("[serve] poller error: %v", err)
		}
	}()

	runningTasks := app.NewRunningTasks()
	closedTracker := &app.ClosedIssues{}

	wg.Add(1)
	go func() {
		defer wg.Done()
		runServeEventLoop(ctx, p, sm, evlog, rep, st, depResolver, cfg.Global.Repo, secRuntime, runningTasks, closedTracker)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := rt.Run(ctx, dispatchCh); err != nil {
			log.Printf("[serve] router error: %v", err)
		}
	}()

	workerDone := make(chan struct{})
	if !opts.coordinatorAPI {
		deps := &workerDeps{
			executor:     workerexec.NewExecutor(lnch, labelReader),
			recorder:     recorder,
			reporter:     rep,
			store:        st,
			sm:           sm,
			workerID:     workerID,
			cfg:          cfg,
			wsMgr:        wsMgr,
			runningTasks: runningTasks,
			closedIssues: closedTracker,
			taskHub:      taskHub,
			sessionsDir:  sessionsDir,
			issueReader:  labelReader,
		}
		wg.Add(1)
		embeddedWorker := workerexec.NewEmbeddedWorker(deps.embeddedDeps(), opts.maxParallelTasks)
		go func() {
			defer wg.Done()
			defer close(workerDone)
			embeddedWorker.Run(ctx, taskCh)
		}()
	} else {
		close(workerDone)
	}

	fmt.Printf("workbuddy serving (repo=%s, roles=[%s], poll=%s, port=%d, coordinator_api=%t)\n",
		cfg.Global.Repo, strings.Join(opts.roles, ","), cfg.Global.PollInterval, cfg.Global.Port, opts.coordinatorAPI)

	if sigCh != nil {
		select {
		case sig := <-sigCh:
			log.Printf("[serve] received signal %s, shutting down...", sig)
		case <-ctx.Done():
		}
	} else {
		<-ctx.Done()
	}

	cancel()
	agentDone := make(chan struct{})
	go func() {
		<-workerDone
		close(agentDone)
	}()
	select {
	case <-agentDone:
		log.Printf("[serve] all agents completed")
	case <-time.After(agentShutdownWait):
		log.Printf("[serve] agent shutdown timeout (%s), forcing exit", agentShutdownWait)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[serve] HTTP server shutdown error: %v", err)
	}

	// Stop any shared agent-backend resources (codex app-server, etc.) held
	// by the runtime registry. No-op for runtimes without persistent state.
	if err := lnch.Shutdown(shutdownCtx); err != nil {
		log.Printf("[serve] runtime shutdown error: %v", err)
	}

	wg.Wait()
	log.Printf("[serve] shutdown complete")
	return nil
}

// loadServeConfig loads, validates, and applies CLI overrides to the serve
// FullConfig. Kept separate so flag parsing is cleanly isolated from
// composition.
func loadServeConfig(opts *serveOpts) (*config.FullConfig, error) {
	cfg, warnings, err := config.LoadConfig(opts.configDir)
	if err != nil {
		return nil, fmt.Errorf("serve: load config: %w", err)
	}
	for _, w := range warnings {
		log.Printf("[serve] warning: %s", w)
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

// buildRunContext wires the shutdown context, either from an explicit parent
// (tests) or from SIGINT/SIGTERM (real runs).
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

// newServeMux registers the HTTP handlers for the embedded serve mode.
func newServeMux(st *store.Store, cfg *config.FullConfig, evlog *eventlog.EventLogger, sessionsDir string, taskHub *tasknotify.Hub, coordinatorAPI bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"status":"ok","repo":%q}`, cfg.Global.Repo)
	})
	audit.NewHTTPHandler(st).Register(mux)
	metrics.NewHandler(st).WithEventLogger(evlog).Register(mux)
	dashboardAPI := auditapi.NewHandler(st)
	dashboardAPI.SetSessionsDir(sessionsDir)
	dashboardAPI.RegisterDashboard(mux)
	mux.HandleFunc("/tasks/watch", app.NewTaskWatchHandler(taskHub))
	sessionUI := webui.NewHandler(st)
	sessionUI.SetSessionsDir(sessionsDir)
	sessionUI.Register(mux)
	if coordinatorAPI {
		coordinatorhttp.NewHandler(st).Register(mux)
	}
	return mux
}

// runServeEventLoop is the poll-event fanout loop: it bridges poller events
// into the state machine, runs dependency maintenance on cycle boundaries,
// and cancels running agents when their issues close.
func runServeEventLoop(
	ctx context.Context,
	p *poller.Poller,
	sm *statemachine.StateMachine,
	evlog *eventlog.EventLogger,
	rep *reporter.Reporter,
	st *store.Store,
	depResolver *dependency.Resolver,
	repo string,
	secRuntime *security.Runtime,
	runningTasks *app.RunningTasks,
	closedTracker *app.ClosedIssues,
) {
	var depsResolvedThisCycle bool
	var depGraphVersion int64
	runDependencyMaintenance := func(ctx context.Context) {
		depGraphVersion++
		unblockedIssues, err := depResolver.EvaluateOpenIssues(ctx, repo, depGraphVersion)
		if err != nil {
			log.Printf("[serve] dependency resolver error: %v", err)
			return
		}
		for _, issueNum := range unblockedIssues {
			if delErr := st.DeleteIssueCache(repo, issueNum); delErr != nil {
				log.Printf("[serve] dependency unblock cache-invalidate #%d: %v", issueNum, delErr)
			} else {
				log.Printf("[serve] dependency unblocked #%d — cache invalidated for redispatch", issueNum)
			}
		}
		caches, err := st.ListIssueCaches(repo)
		if err != nil {
			log.Printf("[serve] dependency reaction list-caches error: %v", err)
			return
		}
		for _, cached := range caches {
			if cached.State != "open" {
				continue
			}
			state, err := st.QueryIssueDependencyState(cached.Repo, cached.IssueNum)
			if err != nil {
				log.Printf("[serve] dependency reaction query state %s#%d: %v", cached.Repo, cached.IssueNum, err)
				continue
			}
			if state == nil {
				continue
			}
			wantBlocked := state.Verdict == store.DependencyVerdictBlocked || state.Verdict == store.DependencyVerdictNeedsHuman
			if wantBlocked == state.LastReactionBlocked {
				continue
			}
			if err := rep.SetBlockedReaction(ctx, cached.Repo, cached.IssueNum, wantBlocked); err != nil {
				log.Printf("[serve] dependency reaction set %s#%d blocked=%v: %v", cached.Repo, cached.IssueNum, wantBlocked, err)
				continue
			}
			if err := st.MarkDependencyReactionApplied(cached.Repo, cached.IssueNum, wantBlocked); err != nil {
				log.Printf("[serve] dependency reaction mark %s#%d: %v", cached.Repo, cached.IssueNum, err)
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
				sm.CheckAllStuck(ev.Repo)
				depsResolvedThisCycle = false
				sm.ResetDedup()
				continue
			}
			if ev.Type == poller.EventIssueClosed {
				closedTracker.MarkClosed(ev.Repo, ev.IssueNum)
				if cancelled := runningTasks.Cancel(ev.Repo, ev.IssueNum); cancelled {
					log.Printf("[serve] cancelled agent for closed issue %s#%d", ev.Repo, ev.IssueNum)
				}
				continue
			}
			closedTracker.MarkOpen(ev.Repo, ev.IssueNum)
			if !app.AllowSecurityEvent(secRuntime, ev) {
				continue
			}
			if !depsResolvedThisCycle {
				runDependencyMaintenance(ctx)
				depsResolvedThisCycle = true
			}
			smEvent := statemachine.ChangeEvent{
				Type:     ev.Type,
				Repo:     ev.Repo,
				IssueNum: ev.IssueNum,
				Labels:   ev.Labels,
				Detail:   ev.Detail,
				Author:   ev.Author,
			}
			if err := sm.HandleEvent(ctx, smEvent); err != nil {
				log.Printf("[serve] state machine error: %v", err)
			}
		}
	}
}

// workerDeps bundles the dependencies for the embedded worker, avoiding
// parameter sprawl. Kept in cmd/ because tests construct this directly.
type workerDeps struct {
	executor     *workerexec.Executor
	recorder     *workersession.Recorder
	reporter     *reporter.Reporter
	store        *store.Store
	sm           *statemachine.StateMachine
	workerID     string
	cfg          *config.FullConfig
	wsMgr        *workspace.Manager
	runningTasks *app.RunningTasks
	closedIssues *app.ClosedIssues
	taskHub      *tasknotify.Hub
	sessionsDir  string
	issueReader  issueLabelReader
}

func (d *workerDeps) embeddedDeps() workerexec.EmbeddedDeps {
	var runningTasks workerexec.RunningTaskRegistry
	if d.runningTasks != nil {
		runningTasks = d.runningTasks
	}
	var closedIssues workerexec.ClosedIssueTracker
	if d.closedIssues != nil {
		closedIssues = d.closedIssues
	}
	var issueReader workerexec.IssueLabelReader
	if d.issueReader != nil {
		issueReader = d.issueReader
	}
	return workerexec.EmbeddedDeps{
		Executor:         d.executor,
		Recorder:         d.recorder,
		Reporter:         d.reporter,
		Store:            d.store,
		StateMachine:     d.sm,
		WorkerID:         d.workerID,
		Config:           d.cfg,
		WorkspaceManager: d.wsMgr,
		RunningTasks:     runningTasks,
		ClosedIssues:     closedIssues,
		TaskHub:          d.taskHub,
		IssueReader:      issueReader,
	}
}
