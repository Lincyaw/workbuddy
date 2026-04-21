package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Lincyaw/workbuddy/internal/alertbus"
	"github.com/Lincyaw/workbuddy/internal/audit"
	"github.com/Lincyaw/workbuddy/internal/auditapi"
	"github.com/Lincyaw/workbuddy/internal/config"
	coordinatorhttp "github.com/Lincyaw/workbuddy/internal/coordinator/http"
	"github.com/Lincyaw/workbuddy/internal/dependency"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/ghadapter"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	"github.com/Lincyaw/workbuddy/internal/metrics"
	"github.com/Lincyaw/workbuddy/internal/notifier"
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

const (
	defaultPort             = 8080
	defaultPollInterval     = 30 * time.Second
	defaultMaxParallelTasks = 4
	taskChanSize            = 64
	dispatchChanSize        = 64
	agentShutdownWait       = 60 * time.Second
)

// RunningTasks is a thread-safe registry of cancel functions for running agent tasks.
type RunningTasks struct {
	mu    sync.Mutex
	tasks map[string]context.CancelFunc // key: "repo#issueNum"
}

// NewRunningTasks creates a new RunningTasks registry.
func NewRunningTasks() *RunningTasks {
	return &RunningTasks{tasks: make(map[string]context.CancelFunc)}
}

func runningTaskKey(repo string, issue int) string {
	return fmt.Sprintf("%s#%d", repo, issue)
}

// Register stores a cancel function for a running task.
func (rt *RunningTasks) Register(repo string, issue int, cancel context.CancelFunc) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.tasks[runningTaskKey(repo, issue)] = cancel
}

// Cancel cancels the running task for the given repo+issue and returns true,
// or returns false if no such task is running.
func (rt *RunningTasks) Cancel(repo string, issue int) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	key := runningTaskKey(repo, issue)
	cancel, ok := rt.tasks[key]
	if !ok {
		return false
	}
	cancel()
	delete(rt.tasks, key)
	return true
}

// Remove removes the entry for a completed task (without cancelling).
func (rt *RunningTasks) Remove(repo string, issue int) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.tasks, runningTaskKey(repo, issue))
}

// serveOpts holds parsed CLI flags for the serve command.
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

// closedIssues tracks issues that were closed while same-issue work was still
// queued so deferred tasks can be dropped before they start.
type closedIssues struct {
	issues sync.Map // key: "repo#issueNum" -> struct{}
}

func (c *closedIssues) MarkClosed(repo string, issue int) {
	c.issues.Store(runningTaskKey(repo, issue), struct{}{})
}

func (c *closedIssues) MarkOpen(repo string, issue int) {
	c.issues.Delete(runningTaskKey(repo, issue))
}

func (c *closedIssues) IsClosed(repo string, issue int) bool {
	_, ok := c.issues.Load(runningTaskKey(repo, issue))
	return ok
}

// GHCLIReader implements poller.GHReader using the shared gh CLI adapter.
type GHCLIReader struct {
	client *ghadapter.CLI
}

func (g *GHCLIReader) cli() *ghadapter.CLI {
	if g != nil && g.client != nil {
		return g.client
	}
	return ghadapter.NewCLI()
}

func (g *GHCLIReader) ListIssues(repo string) ([]poller.Issue, error) {
	return g.cli().ListIssues(repo)
}

func (g *GHCLIReader) ListPRs(repo string) ([]poller.PR, error) {
	return g.cli().ListPRs(repo)
}

func (g *GHCLIReader) CheckRepoAccess(repo string) error {
	return g.cli().CheckRepoAccess(repo)
}

func (g *GHCLIReader) ReadIssueLabels(repo string, issueNum int) ([]string, error) {
	return g.cli().ReadIssueLabels(repo, issueNum)
}

func (g *GHCLIReader) ReadIssue(repo string, issueNum int) (poller.IssueDetails, error) {
	return g.cli().ReadIssue(repo, issueNum)
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

	return runServeWithOpts(opts, nil, nil)
}

// parseServeFlags extracts serve flags from the cobra command.
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

// runServeWithOpts is the testable core of the serve command.
// ghReader and launcherOverride allow tests to inject mocks.
// If parentCtx is non-nil, it is used instead of signal handling (for tests).
func runServeWithOpts(opts *serveOpts, ghReader poller.GHReader, launcherOverride *runtimepkg.Registry, parentCtx ...context.Context) error {
	// 1. Load config
	cfg, warnings, err := config.LoadConfig(opts.configDir)
	if err != nil {
		return fmt.Errorf("serve: load config: %w", err)
	}
	for _, w := range warnings {
		log.Printf("[serve] warning: %s", w)
	}

	// Apply config overrides
	if cfg.Global.Repo == "" {
		return fmt.Errorf("serve: config must specify repo")
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

	// 2. Init SQLite
	st, err := store.NewStore(opts.dbPath)
	if err != nil {
		return fmt.Errorf("serve: init store: %w", err)
	}
	defer func() { _ = st.Close() }()
	alertBus := alertbus.NewBus(64)

	// 3. Recovery: mark running tasks as failed, re-route pending
	if err := recoverTasks(st, alertBus); err != nil {
		log.Printf("[serve] warning: recovery failed: %v", err)
	}

	// 4. Init components
	evlog := eventlog.NewEventLogger(st)
	reg := registry.NewRegistry(st, cfg.Global.PollInterval)
	// Resolve sessionsDir to an absolute path up front so auditor, event writer,
	// and web UI all read/write the same location even if something later
	// changes the process working directory.
	repoDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("serve: get working directory: %w", err)
	}
	securityPath := filepath.Join(repoDir, ".workbuddy", "security.yaml")
	secRuntime, watchSecurityFile, err := security.NewRuntime(security.Options{
		FlagValue: opts.trustedAuthors,
		FlagSet:   opts.trustedAuthorsSet,
		EnvValue:  os.Getenv("WORKBUDDY_TRUSTED_AUTHORS"),
		FilePath:  securityPath,
	})
	if err != nil {
		return fmt.Errorf("serve: load security config: %w", err)
	}
	logSecurityPosture(secRuntime.Current())
	sessionsDir := filepath.Join(repoDir, ".workbuddy", "sessions")
	recorder := workersession.NewRecorder(st, sessionsDir)

	var workerID string
	if !opts.coordinatorAPI {
		workerID, err = reg.RegisterEmbedded(cfg.Global.Repo, opts.roles)
		if err != nil {
			return fmt.Errorf("serve: register embedded worker: %w", err)
		}
	}

	// Channels
	dispatchCh := make(chan statemachine.DispatchRequest, dispatchChanSize)
	var taskCh chan router.WorkerTask
	if !opts.coordinatorAPI {
		taskCh = make(chan router.WorkerTask, taskChanSize)
	}

	// GH Reader (must be initialized before components that use it)
	if ghReader == nil {
		ghReader = &GHCLIReader{}
	}
	var labelReader issueLabelReader
	if reader, ok := ghReader.(issueLabelReader); ok {
		labelReader = reader
	}

	// State machine
	sm := statemachine.NewStateMachine(cfg.Workflows, st, dispatchCh, evlog, alertBus)
	// Enable persistent per-issue dispatch claim (REQ-057). In embedded serve
	// mode the claim holder is the in-process worker so a second serve process
	// sharing the same SQLite DB cannot race on redispatch of the same issue.
	// The claim holder must be process-scoped so restarts or multiple local
	// coordinators on the same host never look like the same claimant.
	claimerID := workerID
	if claimerID == "" {
		claimerID = "coordinator-" + hostnameOrUnknown()
	}
	sm.SetIssueClaim(buildIssueClaimerID(claimerID, os.Getpid()), statemachine.DefaultIssueClaimLease)
	depResolver := dependency.NewResolver(st, ghReader, evlog, alertBus)

	// Workspace isolation is only needed for the embedded worker path.
	var wsMgr *workspace.Manager
	if !opts.coordinatorAPI {
		wsMgr = workspace.NewManager(repoDir)
		_ = wsMgr.Prune() // clean up orphaned worktrees from prior crashes
	}

	// Router
	rt := router.NewRouter(cfg.Agents, reg, st, cfg.Global.Repo, repoDir, taskCh, wsMgr, !opts.coordinatorAPI)
	if issueDataReader, ok := ghReader.(router.IssueDataReader); ok {
		rt.SetIssueDataReader(issueDataReader)
	}

	// Launcher
	lnch := launcherOverride
	if lnch == nil {
		lnch = launcher.NewLauncher()
	}
	lnch.SetSessionManager(runtimepkg.NewSessionManager(sessionsDir, st))

	// Reporter
	rep := reporter.NewReporter(&reporter.GHCLIWriter{})
	rep.SetBaseURL(fmt.Sprintf("http://localhost:%d", cfg.Global.Port))
	rep.SetEventRecorder(recorder)
	rep.SetVerifier(reporter.NewGHClaimVerifier())
	rt.SetReporter(rep)

	// Poller
	p := poller.NewPoller(ghReader, st, cfg.Global.Repo, cfg.Global.PollInterval)
	p.SetEventRecorder(evlog)

	// Context with signal handling or parent context (for tests)
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
	if watchSecurityFile {
		if err := secRuntime.StartFileWatcher(ctx); err != nil {
			return fmt.Errorf("serve: start security watcher: %w", err)
		}
	}

	// Optional startup rate-limit budget check; this is best-effort and
	// intentionally non-fatal for normal startup.
	go runRateLimitBudgetCheck(ctx, "serve", cfg.Global.Repo)

	var wg sync.WaitGroup
	taskHub := tasknotify.NewHub()
	not, err := notifier.New(cfg.Notifications, alertBus, taskHub, evlog)
	if err != nil {
		return fmt.Errorf("serve: init notifier: %w", err)
	}
	not.Start(ctx)

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

	// 5. Start HTTP server
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
	mux.HandleFunc("/tasks/watch", newTaskWatchHandler(taskHub))

	// Session viewer web UI (also serves JSON via auditapi.BuildSessionResponse)
	sessionUI := webui.NewHandler(st)
	sessionUI.SetSessionsDir(sessionsDir)
	sessionUI.Register(mux)
	if opts.coordinatorAPI {
		coordinatorhttp.NewHandler(st).Register(mux)
	}
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Global.Port),
		Handler: mux,
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[serve] HTTP server error: %v", err)
		}
	}()

	// 6. Start Poller goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := p.Run(ctx); err != nil {
			log.Printf("[serve] poller error: %v", err)
		}
	}()

	// Running tasks registry for cancellation on issue close.
	runningTasks := NewRunningTasks()
	closedTracker := &closedIssues{}

	// 7. Start StateMachine event processor (reads from poller events)
	wg.Add(1)
	go func() {
		defer wg.Done()
		var depsResolvedThisCycle bool
		var depGraphVersion int64
		runDependencyMaintenance := func(ctx context.Context) {
			depGraphVersion++
			unblockedIssues, err := depResolver.EvaluateOpenIssues(ctx, cfg.Global.Repo, depGraphVersion)
			if err != nil {
				log.Printf("[serve] dependency resolver error: %v", err)
				return
			}
			for _, issueNum := range unblockedIssues {
				if delErr := st.DeleteIssueCache(cfg.Global.Repo, issueNum); delErr != nil {
					log.Printf("[serve] dependency unblock cache-invalidate #%d: %v", issueNum, delErr)
				} else {
					log.Printf("[serve] dependency unblocked #%d — cache invalidated for redispatch", issueNum)
				}
			}
			// Reaction reconciler: for every issue we just evaluated, if the
			// blocked-state on GitHub differs from the verdict we just
			// computed, add or remove the 😕 reaction. This is the only
			// GitHub UX surface for blocked state — we do NOT write a managed
			// comment.
			caches, err := st.ListIssueCaches(cfg.Global.Repo)
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
				// Handle poll-cycle boundary: clear state-machine dedup so events
				// emitted in the next cycle aren't suppressed by stale per-cycle
				// state. Without this, a label like status:developing that is
				// re-added after a review bounce-back would be silently dropped.
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
				// Handle issue closure: cancel running agent and skip state machine.
				if ev.Type == poller.EventIssueClosed {
					closedTracker.MarkClosed(ev.Repo, ev.IssueNum)
					if cancelled := runningTasks.Cancel(ev.Repo, ev.IssueNum); cancelled {
						log.Printf("[serve] cancelled agent for closed issue %s#%d", ev.Repo, ev.IssueNum)
					}
					continue // don't pass to state machine
				}
				closedTracker.MarkOpen(ev.Repo, ev.IssueNum)
				if !allowSecurityEvent(secRuntime, ev) {
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
	}()

	// 8. Start Router goroutine
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

	// Print startup message
	rolesStr := strings.Join(opts.roles, ",")
	fmt.Printf("workbuddy serving (repo=%s, roles=[%s], poll=%s, port=%d, coordinator_api=%t)\n",
		cfg.Global.Repo, rolesStr, cfg.Global.PollInterval, cfg.Global.Port, opts.coordinatorAPI)

	// 10. Wait for shutdown signal
	if sigCh != nil {
		select {
		case sig := <-sigCh:
			log.Printf("[serve] received signal %s, shutting down...", sig)
		case <-ctx.Done():
		}
	} else {
		<-ctx.Done()
	}

	// Graceful shutdown sequence
	cancel() // Stop poller, router, state machine processor

	// Wait for agents with timeout
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

	// Shutdown HTTP server
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[serve] HTTP server shutdown error: %v", err)
	}

	wg.Wait()
	log.Printf("[serve] shutdown complete")
	return nil
}

func allowSecurityEvent(secRuntime *security.Runtime, ev poller.ChangeEvent) bool {
	if secRuntime == nil {
		return true
	}
	switch ev.Type {
	case poller.EventIssueCreated, poller.EventLabelAdded, poller.EventLabelRemoved:
	default:
		return true
	}
	current := secRuntime.Current()
	if !current.IsRestricted() || current.Allows(ev.Author) {
		return true
	}
	author := strings.TrimSpace(ev.Author)
	if author == "" {
		author = "unknown"
	}
	log.Printf("[security] skipping issue #%d by @%s: author not in trusted_authors", ev.IssueNum, author)
	return false
}

func logSecurityPosture(snapshot security.Snapshot) {
	if !snapshot.IsRestricted() {
		log.Printf("[security] trusted_authors: unrestricted (no allowlist configured)")
		return
	}
	log.Printf("[security] trusted_authors: %s (source: %s)", snapshot.FormatAuthors(), snapshot.Source)
}

// workerDeps bundles the dependencies for the embedded worker, avoiding parameter sprawl.
type workerDeps struct {
	executor     *workerexec.Executor
	recorder     *workersession.Recorder
	reporter     *reporter.Reporter
	store        *store.Store
	sm           *statemachine.StateMachine
	workerID     string
	cfg          *config.FullConfig
	wsMgr        *workspace.Manager
	runningTasks *RunningTasks
	closedIssues *closedIssues
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

func newTaskWatchHandler(hub *tasknotify.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if hub == nil {
			http.Error(w, "task watch unavailable", http.StatusServiceUnavailable)
			return
		}

		repo := strings.TrimSpace(r.URL.Query().Get("repo"))
		issue := 0
		if raw := strings.TrimSpace(r.URL.Query().Get("issue")); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 {
				http.Error(w, "invalid issue", http.StatusBadRequest)
				return
			}
			issue = n
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		subID, ch := hub.Subscribe()
		defer hub.Unsubscribe(subID)

		for {
			select {
			case <-r.Context().Done():
				return
			case event, ok := <-ch:
				if !ok {
					return
				}
				if repo != "" && event.Repo != repo {
					continue
				}
				if issue > 0 && event.IssueNum != issue {
					continue
				}
				data, err := json.Marshal(event)
				if err != nil {
					http.Error(w, "failed to encode task event", http.StatusInternalServerError)
					return
				}
				_, _ = fmt.Fprint(w, "event: task_complete\n")
				_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
				return
			}
		}
	}
}

// recoverTasks marks running tasks as failed and re-routes pending tasks on restart.
func recoverTasks(st *store.Store, alertBus *alertbus.Bus) error {
	// Mark all running tasks as failed (subprocess lost on restart)
	running, err := st.QueryTasks(store.TaskStatusRunning)
	if err != nil {
		return fmt.Errorf("query running tasks: %w", err)
	}
	for _, t := range running {
		log.Printf("[serve] recovery: marking task %s as failed (was running)", t.ID)
		if err := st.UpdateTaskStatus(t.ID, store.TaskStatusFailed); err != nil {
			log.Printf("[serve] recovery: failed to mark task %s: %v", t.ID, err)
		}
		if alertBus != nil {
			alertBus.Publish(alertbus.AlertEvent{
				Kind:      alertbus.KindOrphanedTask,
				Severity:  alertbus.SeverityWarn,
				Repo:      t.Repo,
				IssueNum:  t.IssueNum,
				AgentName: t.AgentName,
				Timestamp: time.Now().Unix(),
				Payload: map[string]any{
					"task_id": t.ID,
					"status":  store.TaskStatusFailed,
				},
			})
		}
	}

	// Log pending tasks that will be re-dispatched via normal poller cycle
	pending, err := st.QueryTasks(store.TaskStatusPending)
	if err != nil {
		return fmt.Errorf("query pending tasks: %w", err)
	}
	if len(pending) > 0 {
		log.Printf("[serve] recovery: %d pending tasks will be re-routed via next poll cycle", len(pending))
	}

	return nil
}
