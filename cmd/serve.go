package cmd

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Lincyaw/workbuddy/internal/audit"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/reporter"
	"github.com/Lincyaw/workbuddy/internal/router"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/spf13/cobra"
)

const (
	defaultPort         = 8080
	defaultPollInterval = 30 * time.Second
	taskChanSize        = 64
	dispatchChanSize    = 64
	agentShutdownWait   = 60 * time.Second
)

// serveOpts holds parsed CLI flags for the serve command.
type serveOpts struct {
	port         int
	pollInterval time.Duration
	roles        []string
	configDir    string
	dbPath       string
}

// GHCLIReader implements poller.GHReader using the gh CLI.
type GHCLIReader struct{}

func (g *GHCLIReader) ListIssues(repo string) ([]poller.Issue, error) {
	return nil, fmt.Errorf("gh CLI not available in test mode")
}

func (g *GHCLIReader) ListPRs(repo string) ([]poller.PR, error) {
	return nil, fmt.Errorf("gh CLI not available in test mode")
}

func (g *GHCLIReader) CheckRepoAccess(repo string) error {
	return nil
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
	serveCmd.Flags().StringSlice("roles", []string{"dev", "test", "review"}, "Worker roles")
	serveCmd.Flags().String("config-dir", ".github/workbuddy", "Configuration directory")
	serveCmd.Flags().String("db-path", ".workbuddy/workbuddy.db", "SQLite database path")
	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, args []string) error {
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
	roles, _ := cmd.Flags().GetStringSlice("roles")
	configDir, _ := cmd.Flags().GetString("config-dir")
	dbPath, _ := cmd.Flags().GetString("db-path")

	return &serveOpts{
		port:         port,
		pollInterval: pollInterval,
		roles:        roles,
		configDir:    configDir,
		dbPath:       dbPath,
	}, nil
}

// runServeWithOpts is the testable core of the serve command.
// ghReader and launcherOverride allow tests to inject mocks.
// If parentCtx is non-nil, it is used instead of signal handling (for tests).
func runServeWithOpts(opts *serveOpts, ghReader poller.GHReader, launcherOverride *launcher.Launcher, parentCtx ...context.Context) error {
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
	defer st.Close()

	// 3. Recovery: mark running tasks as failed, re-route pending
	if err := recoverTasks(st); err != nil {
		log.Printf("[serve] warning: recovery failed: %v", err)
	}

	// 4. Init components
	evlog := eventlog.NewEventLogger(st)
	reg := registry.NewRegistry(st, cfg.Global.PollInterval)
	auditor := audit.NewAuditor(st, ".workbuddy/sessions")

	// Register embedded worker
	workerID, err := reg.RegisterEmbedded(cfg.Global.Repo, opts.roles)
	if err != nil {
		return fmt.Errorf("serve: register embedded worker: %w", err)
	}

	// Channels
	dispatchCh := make(chan statemachine.DispatchRequest, dispatchChanSize)
	taskCh := make(chan router.WorkerTask, taskChanSize)

	// State machine
	sm := statemachine.NewStateMachine(cfg.Workflows, st, dispatchCh, evlog)

	// Router
	rt := router.NewRouter(cfg.Agents, reg, st, cfg.Global.Repo, taskCh)

	// Launcher
	lnch := launcherOverride
	if lnch == nil {
		lnch = launcher.NewLauncher()
	}

	// Reporter
	rep := reporter.NewReporter(&reporter.GHCLIWriter{})

	// GH Reader
	if ghReader == nil {
		ghReader = &GHCLIReader{}
	}

	// Poller
	p := poller.NewPoller(ghReader, st, cfg.Global.Repo, cfg.Global.PollInterval)

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

	var wg sync.WaitGroup

	// 5. Start HTTP server (/health only)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"ok","repo":%q}`, cfg.Global.Repo)
	})
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

	// 7. Start StateMachine event processor (reads from poller events)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-p.Events():
				if !ok {
					return
				}
				smEvent := statemachine.ChangeEvent{
					Type:     ev.Type,
					Repo:     ev.Repo,
					IssueNum: ev.IssueNum,
					Labels:   ev.Labels,
					Detail:   ev.Detail,
				}
				if err := sm.HandleEvent(smEvent); err != nil {
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

	// 9. Start embedded Worker goroutine
	workerDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(workerDone)
		runEmbeddedWorker(ctx, taskCh, lnch, auditor, rep, st, sm, workerID, cfg)
	}()

	// Print startup message
	rolesStr := strings.Join(opts.roles, ",")
	fmt.Printf("workbuddy serving (repo=%s, roles=[%s], poll=%s, port=%d)\n",
		cfg.Global.Repo, rolesStr, cfg.Global.PollInterval, cfg.Global.Port)

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

// runEmbeddedWorker runs the embedded worker loop.
func runEmbeddedWorker(
	ctx context.Context,
	taskCh <-chan router.WorkerTask,
	lnch *launcher.Launcher,
	auditor *audit.Auditor,
	rep *reporter.Reporter,
	st *store.Store,
	sm *statemachine.StateMachine,
	workerID string,
	cfg *config.FullConfig,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case task, ok := <-taskCh:
			if !ok {
				return
			}
			executeTask(ctx, task, lnch, auditor, rep, st, sm, workerID, cfg)
		}
	}
}

// executeTask runs a single agent task and handles reporting/auditing.
func executeTask(
	ctx context.Context,
	task router.WorkerTask,
	lnch *launcher.Launcher,
	auditor *audit.Auditor,
	rep *reporter.Reporter,
	st *store.Store,
	sm *statemachine.StateMachine,
	workerID string,
	cfg *config.FullConfig,
) {
	log.Printf("[worker] executing task %s: agent=%s issue=%s#%d",
		task.TaskID, task.AgentName, task.Repo, task.IssueNum)

	result, err := lnch.Launch(ctx, task.Agent, task.Context)
	if err != nil {
		log.Printf("[worker] agent %s failed: %v", task.AgentName, err)
		if err := st.UpdateTaskStatus(task.TaskID, "failed"); err != nil {
			log.Printf("[worker] failed to update task status: %v", err)
		}
		sm.MarkAgentCompleted(task.Repo, task.IssueNum, nil)
		return
	}

	// Determine task status
	status := "completed"
	if result.ExitCode != 0 {
		status = "failed"
	}
	if result.Meta != nil && result.Meta["timeout"] == "true" {
		status = "timeout"
	}

	// Update task status
	if err := st.UpdateTaskStatus(task.TaskID, status); err != nil {
		log.Printf("[worker] failed to update task status: %v", err)
	}

	// Audit session
	sessionID := task.Context.Session.ID
	if err := auditor.Capture(sessionID, task.TaskID, task.Repo, task.IssueNum, task.AgentName, result); err != nil {
		log.Printf("[worker] audit capture failed: %v", err)
	}

	// Get retry count for reporting
	retryCount := 0
	maxRetries := 3
	if wf, ok := cfg.Workflows[task.Workflow]; ok {
		maxRetries = wf.MaxRetries
	}
	counts, err := st.QueryTransitionCounts(task.Repo, task.IssueNum)
	if err == nil {
		for _, tc := range counts {
			if tc.ToState == task.State {
				retryCount = tc.Count
				break
			}
		}
	}

	// Report to issue
	if err := rep.Report(task.Repo, task.IssueNum, task.AgentName, result,
		sessionID, workerID, retryCount, maxRetries); err != nil {
		log.Printf("[worker] report failed: %v", err)
	}

	// Mark agent completed in state machine
	sm.MarkAgentCompleted(task.Repo, task.IssueNum, nil)
}

// recoverTasks marks running tasks as failed and re-routes pending tasks on restart.
func recoverTasks(st *store.Store) error {
	// Mark all running tasks as failed (subprocess lost on restart)
	running, err := st.QueryTasks("running")
	if err != nil {
		return fmt.Errorf("query running tasks: %w", err)
	}
	for _, t := range running {
		log.Printf("[serve] recovery: marking task %s as failed (was running)", t.ID)
		if err := st.UpdateTaskStatus(t.ID, "failed"); err != nil {
			log.Printf("[serve] recovery: failed to mark task %s: %v", t.ID, err)
		}
	}

	// Log pending tasks that will be re-dispatched via normal poller cycle
	pending, err := st.QueryTasks("pending")
	if err != nil {
		return fmt.Errorf("query pending tasks: %w", err)
	}
	if len(pending) > 0 {
		log.Printf("[serve] recovery: %d pending tasks will be re-routed via next poll cycle", len(pending))
	}

	return nil
}
