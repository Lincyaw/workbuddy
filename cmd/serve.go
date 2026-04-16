package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Lincyaw/workbuddy/internal/audit"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/dependency"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/labelcheck"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/registry"
	"github.com/Lincyaw/workbuddy/internal/reporter"
	"github.com/Lincyaw/workbuddy/internal/router"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/webui"
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
	port             int
	pollInterval     time.Duration
	maxParallelTasks int
	roles            []string
	configDir        string
	dbPath           string
}

// issueTaskLocks serializes tasks for the same repo+issue. Entries are
// ref-counted and evicted once no goroutine holds or waits on them so the
// map cannot grow without bound over a long-running serve process.
type issueTaskLocks struct {
	mu    sync.Mutex
	locks map[string]*issueTaskLock
}

type issueTaskLock struct {
	mu     sync.Mutex
	parent *issueTaskLocks
	key    string
	refs   int
}

func (l *issueTaskLocks) Acquire(repo string, issue int) *issueTaskLock {
	key := runningTaskKey(repo, issue)

	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[string]*issueTaskLock)
	}
	lk, ok := l.locks[key]
	if !ok {
		lk = &issueTaskLock{parent: l, key: key}
		l.locks[key] = lk
	}
	lk.refs++
	l.mu.Unlock()

	lk.mu.Lock()
	return lk
}

func (l *issueTaskLock) Release() {
	l.mu.Unlock()

	l.parent.mu.Lock()
	l.refs--
	if l.refs == 0 {
		if current, ok := l.parent.locks[l.key]; ok && current == l {
			delete(l.parent.locks, l.key)
		}
	}
	l.parent.mu.Unlock()
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

// GHCLIReader implements poller.GHReader using the gh CLI.
type GHCLIReader struct{}

type issueLabelReader interface {
	ReadIssueLabels(repo string, issueNum int) ([]string, error)
}

// ghIssueJSON matches the JSON output of gh issue list --json.
type ghIssueJSON struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Body   string `json:"body"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

type ghIssueLabelsJSON struct {
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

type ghIssueDetailJSON struct {
	Number      int    `json:"number"`
	State       string `json:"state"`
	StateReason string `json:"stateReason"`
	Body        string `json:"body"`
	Labels      struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	ClosedByPullRequestsReferences struct {
		Nodes []struct {
			Number int    `json:"number"`
			State  string `json:"state"`
			URL    string `json:"url"`
		} `json:"nodes"`
	} `json:"closedByPullRequestsReferences"`
}

// ghPRJSON matches the JSON output of gh pr list --json.
type ghPRJSON struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	HeadRefName string `json:"headRefName"`
	State       string `json:"state"`
}

// ListIssues returns issues for the given repo via gh CLI.
func (g *GHCLIReader) ListIssues(repo string) ([]poller.Issue, error) {
	cmd := exec.Command("gh", "issue", "list",
		"--repo", repo,
		"--state", "open",
		"--limit", "100",
		"--json", "number,title,state,body,labels",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh issue list: %w", err)
	}

	var raw []ghIssueJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("gh issue list: parse JSON: %w", err)
	}

	issues := make([]poller.Issue, len(raw))
	for i, r := range raw {
		labels := make([]string, len(r.Labels))
		for j, l := range r.Labels {
			labels[j] = l.Name
		}
		issues[i] = poller.Issue{
			Number: r.Number,
			Title:  r.Title,
			State:  r.State,
			Labels: labels,
			Body:   r.Body,
		}
	}
	return issues, nil
}

// ListPRs returns pull requests for the given repo via gh CLI.
func (g *GHCLIReader) ListPRs(repo string) ([]poller.PR, error) {
	cmd := exec.Command("gh", "pr", "list",
		"--repo", repo,
		"--state", "open",
		"--limit", "100",
		"--json", "number,url,headRefName,state",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}

	var raw []ghPRJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("gh pr list: parse JSON: %w", err)
	}

	prs := make([]poller.PR, len(raw))
	for i, r := range raw {
		prs[i] = poller.PR{
			Number: r.Number,
			URL:    r.URL,
			Branch: r.HeadRefName,
			State:  r.State,
		}
	}
	return prs, nil
}

// CheckRepoAccess verifies gh CLI access to the given repo.
func (g *GHCLIReader) CheckRepoAccess(repo string) error {
	cmd := exec.Command("gh", "repo", "view", repo, "--json", "name")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh repo view %s: %s: %w", repo, string(out), err)
	}
	return nil
}

func (g *GHCLIReader) ReadIssueLabels(repo string, issueNum int) ([]string, error) {
	cmd := exec.Command("gh", "issue", "view",
		fmt.Sprintf("%d", issueNum),
		"--repo", repo,
		"--json", "labels",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh issue view labels: %w", err)
	}

	var raw ghIssueLabelsJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("gh issue view labels: parse JSON: %w", err)
	}

	labels := make([]string, len(raw.Labels))
	for i, label := range raw.Labels {
		labels[i] = label.Name
	}
	return labels, nil
}

func (g *GHCLIReader) ReadIssue(repo string, issueNum int) (poller.IssueDetails, error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return poller.IssueDetails{}, fmt.Errorf("invalid repo %q", repo)
	}
	query := `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){issue(number:$number){number state stateReason body labels(first:100){nodes{name}} closedByPullRequestsReferences(first:10){nodes{number state url}}}}}`
	cmd := exec.Command("gh", "api", "graphql",
		"-f", "query="+query,
		"-F", "owner="+parts[0],
		"-F", "name="+parts[1],
		"-F", fmt.Sprintf("number=%d", issueNum),
	)
	out, err := cmd.Output()
	if err != nil {
		return poller.IssueDetails{}, fmt.Errorf("gh api graphql issue detail: %w", err)
	}
	var response struct {
		Data struct {
			Repository struct {
				Issue ghIssueDetailJSON `json:"issue"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return poller.IssueDetails{}, fmt.Errorf("gh api graphql issue detail parse: %w", err)
	}
	issue := response.Data.Repository.Issue
	labels := make([]string, len(issue.Labels.Nodes))
	for i, label := range issue.Labels.Nodes {
		labels[i] = label.Name
	}
	return poller.IssueDetails{
		Number:           issue.Number,
		State:            strings.ToLower(issue.State),
		StateReason:      strings.ToLower(issue.StateReason),
		Body:             issue.Body,
		Labels:           labels,
		ClosedByLinkedPR: len(issue.ClosedByPullRequestsReferences.Nodes) > 0,
	}, nil
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
	if maxParallelTasks <= 0 {
		return nil, fmt.Errorf("serve: --max-parallel-tasks must be > 0")
	}

	return &serveOpts{
		port:             port,
		pollInterval:     pollInterval,
		maxParallelTasks: maxParallelTasks,
		roles:            roles,
		configDir:        configDir,
		dbPath:           dbPath,
	}, nil
}

// runServeWithOpts is the testable core of the serve command.
// ghReader and launcherOverride allow tests to inject mocks.
// If parentCtx is non-nil, it is used instead of signal handling (for tests).
func runServeWithOpts(opts *serveOpts, ghReader poller.GHReader, launcherOverride *launcher.Launcher, parentCtx ...context.Context) error {
	if opts.maxParallelTasks <= 0 {
		opts.maxParallelTasks = defaultEmbeddedWorkerParallelism()
	}

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

	// 3. Recovery: mark running tasks as failed, re-route pending
	if err := recoverTasks(st); err != nil {
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
	sessionsDir := filepath.Join(repoDir, ".workbuddy", "sessions")
	auditor := audit.NewAuditor(st, sessionsDir)

	// Register embedded worker
	workerID, err := reg.RegisterEmbedded(cfg.Global.Repo, opts.roles)
	if err != nil {
		return fmt.Errorf("serve: register embedded worker: %w", err)
	}

	// Channels
	dispatchCh := make(chan statemachine.DispatchRequest, dispatchChanSize)
	taskCh := make(chan router.WorkerTask, taskChanSize)

	// GH Reader (must be initialized before components that use it)
	if ghReader == nil {
		ghReader = &GHCLIReader{}
	}
	var labelReader issueLabelReader
	if reader, ok := ghReader.(issueLabelReader); ok {
		labelReader = reader
	}

	// State machine
	sm := statemachine.NewStateMachine(cfg.Workflows, st, dispatchCh, evlog)
	depResolver := dependency.NewResolver(st, ghReader, evlog)

	// Workspace isolation via git worktrees
	wsMgr := workspace.NewManager(repoDir)
	_ = wsMgr.Prune() // clean up orphaned worktrees from prior crashes

	// Router
	rt := router.NewRouter(cfg.Agents, reg, st, cfg.Global.Repo, repoDir, taskCh, wsMgr)

	// Launcher
	lnch := launcherOverride
	if lnch == nil {
		lnch = launcher.NewLauncher()
	}

	// Reporter
	rep := reporter.NewReporter(&reporter.GHCLIWriter{})
	rep.SetBaseURL(fmt.Sprintf("http://localhost:%d", cfg.Global.Port))

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

	// 5. Start HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"status":"ok","repo":%q}`, cfg.Global.Repo)
	})

	// Session viewer web UI
	sessionUI := webui.NewHandler(st)
	sessionUI.SetSessionsDir(sessionsDir)
	sessionUI.Register(mux)
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
			if err := depResolver.EvaluateOpenIssues(ctx, cfg.Global.Repo, depGraphVersion); err != nil {
				log.Printf("[serve] dependency resolver error: %v", err)
				return
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
				// Handle issue closure: cancel running agent and skip state machine.
				if ev.Type == poller.EventIssueClosed {
					closedTracker.MarkClosed(ev.Repo, ev.IssueNum)
					if cancelled := runningTasks.Cancel(ev.Repo, ev.IssueNum); cancelled {
						log.Printf("[serve] cancelled agent for closed issue %s#%d", ev.Repo, ev.IssueNum)
					}
					continue // don't pass to state machine
				}
				closedTracker.MarkOpen(ev.Repo, ev.IssueNum)
				smEvent := statemachine.ChangeEvent{
					Type:     ev.Type,
					Repo:     ev.Repo,
					IssueNum: ev.IssueNum,
					Labels:   ev.Labels,
					Detail:   ev.Detail,
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

	// 9. Start embedded Worker goroutine
	deps := &workerDeps{
		launcher:     lnch,
		auditor:      auditor,
		reporter:     rep,
		store:        st,
		sm:           sm,
		workerID:     workerID,
		cfg:          cfg,
		wsMgr:        wsMgr,
		runningTasks: runningTasks,
		closedIssues: closedTracker,
		sessionsDir:  sessionsDir,
		issueReader:  labelReader,
	}
	workerDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(workerDone)
		runEmbeddedWorker(ctx, taskCh, deps, opts.maxParallelTasks)
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

// workerDeps bundles the dependencies for the embedded worker, avoiding parameter sprawl.
type workerDeps struct {
	launcher     *launcher.Launcher
	auditor      *audit.Auditor
	reporter     *reporter.Reporter
	store        *store.Store
	sm           *statemachine.StateMachine
	workerID     string
	cfg          *config.FullConfig
	wsMgr        *workspace.Manager
	runningTasks *RunningTasks
	closedIssues *closedIssues
	sessionsDir  string
	issueReader  issueLabelReader
}

func defaultEmbeddedWorkerParallelism() int {
	if runtime.NumCPU() < defaultMaxParallelTasks {
		return runtime.NumCPU()
	}
	return defaultMaxParallelTasks
}

// runEmbeddedWorker runs the embedded worker loop with bounded cross-issue
// parallelism while serializing tasks for the same repo+issue.
func runEmbeddedWorker(ctx context.Context, taskCh <-chan router.WorkerTask, deps *workerDeps, maxParallelTasks int) {
	if maxParallelTasks <= 0 {
		maxParallelTasks = defaultEmbeddedWorkerParallelism()
	}

	issueLocks := &issueTaskLocks{}
	var wg sync.WaitGroup

	// Fixed-size worker pool. Each worker pulls from taskCh and serializes
	// same-repo+issue work via per-issue locks. This caps in-flight goroutines
	// at maxParallelTasks without holding a global slot while waiting on a
	// per-issue lock (which would cause head-of-line blocking across issues).
	for i := 0; i < maxParallelTasks; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case task, ok := <-taskCh:
					if !ok {
						return
					}
					runWorkerTask(ctx, task, deps, issueLocks)
				}
			}
		}()
	}
	wg.Wait()
}

func runWorkerTask(ctx context.Context, task router.WorkerTask, deps *workerDeps, issueLocks *issueTaskLocks) {
	issueLock := issueLocks.Acquire(task.Repo, task.IssueNum)
	defer issueLock.Release()

	if deps.closedIssues != nil && deps.closedIssues.IsClosed(task.Repo, task.IssueNum) {
		skipTaskForClosedIssue(task, deps)
		return
	}

	executeTask(ctx, task, deps)
}

func skipTaskForClosedIssue(task router.WorkerTask, deps *workerDeps) {
	log.Printf("[worker] skipping queued task %s for closed issue %s#%d",
		task.TaskID, task.Repo, task.IssueNum)

	if deps.store != nil {
		if err := deps.store.UpdateTaskStatus(task.TaskID, store.TaskStatusFailed); err != nil {
			log.Printf("[worker] failed to update skipped task status: %v", err)
		}
	}
	if deps.sm != nil {
		var labels []string
		if deps.store != nil {
			labels = fetchCachedLabels(deps.store, task.Repo, task.IssueNum)
		}
		deps.sm.MarkAgentCompleted(task.Repo, task.IssueNum, labels)
	}
}

// executeTask runs a single agent task and handles reporting/auditing.
func executeTask(ctx context.Context, task router.WorkerTask, deps *workerDeps) {
	// Create a per-task context that can be cancelled independently (e.g., on issue close).
	taskCtx, taskCancel := context.WithCancel(ctx)
	defer taskCancel()

	// Register for cancellation on issue close.
	if deps.runningTasks != nil {
		deps.runningTasks.Register(task.Repo, task.IssueNum, taskCancel)
		defer deps.runningTasks.Remove(task.Repo, task.IssueNum)
	}

	// Clean up worktree after task completes.
	if task.WorktreePath != "" && deps.wsMgr != nil {
		defer func() {
			if err := deps.wsMgr.Remove(task.WorktreePath); err != nil {
				log.Printf("[worker] worktree cleanup failed: %v", err)
			}
		}()
	}
	log.Printf("[worker] executing task %s: agent=%s issue=%s#%d",
		task.TaskID, task.AgentName, task.Repo, task.IssueNum)

	// Mark task as running now that the worker has actually started it.
	if deps.store != nil {
		if err := deps.store.UpdateTaskStatus(task.TaskID, store.TaskStatusRunning); err != nil {
			log.Printf("[worker] failed to update task status to running: %v", err)
		}
	}

	// Add claim reaction (eyes) to signal this issue is being worked on.
	addClaimReaction(taskCtx, task.Repo, task.IssueNum, task.AgentName)

	// Post "Agent Started" comment with session link.
	sessionID := task.Context.Session.ID
	if err := deps.reporter.ReportStarted(task.Repo, task.IssueNum, task.AgentName, sessionID, deps.workerID); err != nil {
		log.Printf("[worker] report started failed: %v", err)
	}

	// Record the session early so the web UI can show it while running.
	if _, err := deps.store.InsertAgentSession(store.AgentSession{
		SessionID: sessionID,
		TaskID:    task.TaskID,
		Repo:      task.Repo,
		IssueNum:  task.IssueNum,
		AgentName: task.AgentName,
	}); err != nil {
		log.Printf("[worker] failed to record session start: %v", err)
	}

	session, err := deps.launcher.Start(taskCtx, task.Agent, task.Context)
	if err != nil {
		log.Printf("[worker] failed to start agent %s: %v", task.AgentName, err)
		if err := deps.store.UpdateTaskStatus(task.TaskID, store.TaskStatusFailed); err != nil {
			log.Printf("[worker] failed to update task status: %v", err)
		}
		deps.sm.MarkAgentCompleted(task.Repo, task.IssueNum, fetchCachedLabels(deps.store, task.Repo, task.IssueNum))
		go func() {
			time.Sleep(60 * time.Second)
			if deps.closedIssues != nil && deps.closedIssues.IsClosed(task.Repo, task.IssueNum) {
				return
			}
			if err := deps.sm.DispatchAgent(context.Background(), task.Repo, task.IssueNum, task.AgentName, task.Workflow, task.State); err != nil {
				log.Printf("[worker] redispatch after start failure for %s#%d: %v", task.Repo, task.IssueNum, err)
			}
		}()
		return
	}
	defer func() { _ = session.Close() }()

	preLabels, preSnapshotErr := snapshotIssueLabels(task.Repo, task.IssueNum, deps.issueReader)
	if preSnapshotErr != nil {
		log.Printf("[worker] label pre-snapshot failed: %v", preSnapshotErr)
	} else if task.Context != nil {
		task.Context.Session.PreLabels = cloneLabels(preLabels)
	}

	eventsCh := make(chan launcherevents.Event, 64)
	eventsPath, waitEvents := streamSessionEvents(deps.sessionsDir, task.Context, eventsCh)
	result, err := session.Run(taskCtx, eventsCh)
	close(eventsCh)
	waitErr := waitEvents()
	if waitErr != nil {
		log.Printf("[worker] event capture failed: %v", waitErr)
	}
	if result != nil && eventsPath != "" && waitErr == nil {
		// Prefer the normalized Event Schema v1 artifact as the session's
		// canonical path so audit/reporter work off the unified stream.
		// The runtime-native artifact (e.g. codex-exec.jsonl) stays on disk
		// but is no longer the handle handed downstream.
		if result.RawSessionPath == "" {
			result.RawSessionPath = result.SessionPath
		}
		result.SessionPath = eventsPath
	}

	postLabels, postSnapshotErr := snapshotIssueLabels(task.Repo, task.IssueNum, deps.issueReader)
	if postSnapshotErr != nil {
		log.Printf("[worker] label post-snapshot failed: %v", postSnapshotErr)
	} else if task.Context != nil {
		task.Context.Session.PostLabels = cloneLabels(postLabels)
	}

	completionLabels := postLabels
	if postSnapshotErr != nil {
		completionLabels = fetchCachedLabels(deps.store, task.Repo, task.IssueNum)
	}

	labelSummary := ""
	if preSnapshotErr == nil && postSnapshotErr == nil {
		if validation, ok, validationErr := validateLabelTransition(task, deps, preLabels, postLabels, result); validationErr != nil {
			log.Printf("[worker] label validation skipped: %v", validationErr)
		} else if ok {
			labelSummary = validation.Summary()
			payload := audit.LabelValidationPayload{
				Pre:            cloneLabels(preLabels),
				Post:           cloneLabels(postLabels),
				ExitCode:       exitCodeForValidation(result),
				Classification: string(validation.Classification),
			}
			if err := deps.auditor.RecordLabelValidation(task.Repo, task.IssueNum, payload); err != nil {
				log.Printf("[worker] label validation audit failed: %v", err)
			}
			if validation.NeedsHumanRecommendation() {
				if err := deps.reporter.ReportNeedsHuman(task.Repo, task.IssueNum, labelSummary); err != nil {
					log.Printf("[worker] needs-human recommendation failed: %v", err)
				}
			}
		}
	} else if preSnapshotErr == nil || postSnapshotErr == nil {
		log.Printf("[worker] label validation skipped: incomplete label snapshots for %s#%d", task.Repo, task.IssueNum)
	} else {
		log.Printf("[worker] label validation skipped: label snapshots unavailable for %s#%d", task.Repo, task.IssueNum)
	}
	if err != nil {
		log.Printf("[worker] agent %s failed: %v", task.AgentName, err)
		if result == nil {
			if err := deps.store.UpdateTaskStatus(task.TaskID, store.TaskStatusFailed); err != nil {
				log.Printf("[worker] failed to update task status: %v", err)
			}
			deps.sm.MarkAgentCompleted(task.Repo, task.IssueNum, completionLabels)
			go func() {
				time.Sleep(60 * time.Second)
				if deps.closedIssues != nil && deps.closedIssues.IsClosed(task.Repo, task.IssueNum) {
					return
				}
				if err := deps.sm.DispatchAgent(context.Background(), task.Repo, task.IssueNum, task.AgentName, task.Workflow, task.State); err != nil {
					log.Printf("[worker] redispatch after run failure for %s#%d: %v", task.Repo, task.IssueNum, err)
				}
			}()
			return
		}
	}

	// Determine task status
	status := store.TaskStatusCompleted
	if err != nil || result.ExitCode != 0 {
		status = store.TaskStatusFailed
	}
	if result.Meta != nil && result.Meta["timeout"] == "true" {
		status = store.TaskStatusTimeout
	}

	// Update task status
	if err := deps.store.UpdateTaskStatus(task.TaskID, status); err != nil {
		log.Printf("[worker] failed to update task status: %v", err)
	}

	// Audit session
	if err := deps.auditor.Capture(sessionID, task.TaskID, task.Repo, task.IssueNum, task.AgentName, result); err != nil {
		log.Printf("[worker] audit capture failed: %v", err)
	}
	// Get retry count for reporting
	retryCount := 0
	maxRetries := 3
	if wf, ok := deps.cfg.Workflows[task.Workflow]; ok {
		maxRetries = wf.MaxRetries
	}
	counts, err := deps.store.QueryTransitionCounts(task.Repo, task.IssueNum)
	if err == nil {
		for _, tc := range counts {
			if tc.ToState == task.State {
				retryCount = tc.Count
				break
			}
		}
	}

	// Report to issue
	if err := deps.reporter.Report(task.Repo, task.IssueNum, task.AgentName, result,
		sessionID, deps.workerID, retryCount, maxRetries, labelSummary); err != nil {
		log.Printf("[worker] report failed: %v", err)
	}

	// Mark agent completed in state machine
	deps.sm.MarkAgentCompleted(task.Repo, task.IssueNum, completionLabels)
}

func streamSessionEvents(sessionsDir string, taskCtx *launcher.TaskContext, eventsCh <-chan launcherevents.Event) (string, func() error) {
	path := filepath.Join(sessionArtifactsBaseDir(sessionsDir, taskCtx), taskCtx.Session.ID, "events-v1.jsonl")
	errCh := make(chan error, 1)
	go func() {
		// Always drain eventsCh to completion so the runtime is never blocked
		// on a full send buffer, even if we can't persist the artifact.
		var initErr, encodeErr error
		var f *os.File
		var enc *json.Encoder
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			initErr = err
		} else if created, err := os.Create(path); err != nil {
			initErr = err
		} else {
			f = created
			enc = json.NewEncoder(f)
		}
		for evt := range eventsCh {
			if enc == nil || encodeErr != nil {
				continue
			}
			if err := enc.Encode(evt); err != nil {
				encodeErr = err
			}
		}
		if f != nil {
			_ = f.Close()
		}
		switch {
		case initErr != nil:
			errCh <- initErr
		case encodeErr != nil:
			errCh <- encodeErr
		default:
			errCh <- nil
		}
	}()
	return path, func() error { return <-errCh }
}

func sessionArtifactsBaseDir(sessionsDir string, taskCtx *launcher.TaskContext) string {
	if taskCtx != nil {
		if repoRoot := strings.TrimSpace(taskCtx.RepoRoot); repoRoot != "" {
			return filepath.Join(repoRoot, ".workbuddy", "sessions")
		}
		if workDir := strings.TrimSpace(taskCtx.WorkDir); workDir != "" {
			return filepath.Join(workDir, ".workbuddy", "sessions")
		}
	}
	if sessionsDir != "" {
		return sessionsDir
	}
	return ".workbuddy/sessions"
}

// addClaimReaction adds an eyes reaction to the issue to signal an agent claimed it.
func addClaimReaction(ctx context.Context, repo string, issueNum int, agentName string) {
	cmd := exec.CommandContext(ctx, "gh", "api",
		fmt.Sprintf("repos/%s/issues/%d/reactions", repo, issueNum),
		"-f", "content=eyes", "--silent")
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return // cancelled, don't log
		}
		log.Printf("[worker] failed to add claim reaction for %s on %s#%d: %v (output: %s)",
			agentName, repo, issueNum, err, string(out))
	}
}

// fetchCachedLabels retrieves the current labels for an issue from the store's
// issue cache. Returns nil if the cache entry is missing or unparseable.
func fetchCachedLabels(st *store.Store, repo string, issueNum int) []string {
	cached, err := st.QueryIssueCache(repo, issueNum)
	if err != nil || cached == nil {
		return nil
	}
	var labels []string
	if err := json.Unmarshal([]byte(cached.Labels), &labels); err != nil {
		log.Printf("[worker] failed to parse cached labels for %s#%d: %v", repo, issueNum, err)
		return nil
	}
	return labels
}

func snapshotIssueLabels(repo string, issueNum int, reader issueLabelReader) ([]string, error) {
	if reader == nil {
		return nil, fmt.Errorf("no issue label reader configured")
	}
	labels, err := reader.ReadIssueLabels(repo, issueNum)
	if err != nil {
		return nil, err
	}
	return cloneLabels(labels), nil
}

func validateLabelTransition(task router.WorkerTask, deps *workerDeps, preLabels, postLabels []string, result *launcher.Result) (labelcheck.Result, bool, error) {
	if deps == nil || deps.cfg == nil {
		return labelcheck.Result{}, false, fmt.Errorf("missing worker config")
	}
	if deps.issueReader == nil {
		return labelcheck.Result{}, false, fmt.Errorf("no issue label reader configured")
	}

	wf, ok := deps.cfg.Workflows[task.Workflow]
	if !ok || wf == nil {
		return labelcheck.Result{}, false, fmt.Errorf("workflow %q not found", task.Workflow)
	}
	queuedState, ok := wf.States[task.State]
	if !ok || queuedState == nil {
		return labelcheck.Result{}, false, fmt.Errorf("state %q not found in workflow %q", task.State, task.Workflow)
	}

	input := labelcheck.Input{
		Pre:      cloneLabels(preLabels),
		Post:     cloneLabels(postLabels),
		ExitCode: exitCodeForValidation(result),
		Current:  labelcheck.State{Name: task.State, Label: queuedState.EnterLabel},
	}

	stateNames := make([]string, 0, len(wf.States))
	for name := range wf.States {
		stateNames = append(stateNames, name)
	}
	sort.Strings(stateNames)

	knownSeen := make(map[string]bool)
	for _, name := range stateNames {
		state := wf.States[name]
		if state == nil || state.EnterLabel == "" || knownSeen[state.EnterLabel] {
			continue
		}
		knownSeen[state.EnterLabel] = true
		input.KnownStates = append(input.KnownStates, labelcheck.State{Name: name, Label: state.EnterLabel})
	}

	input.Current = labelcheck.ResolveCurrent(input.Pre, input.Current, input.KnownStates)

	currentState, err := resolveWorkflowLabelState(wf, input.Current)
	if err != nil {
		return labelcheck.Result{}, false, err
	}

	allowedSeen := make(map[string]bool)
	for _, transition := range currentState.Transitions {
		target, ok := wf.States[transition.To]
		if !ok || target == nil || target.EnterLabel == "" || allowedSeen[target.EnterLabel] {
			continue
		}
		allowedSeen[target.EnterLabel] = true
		input.AllowedTransitions = append(input.AllowedTransitions, labelcheck.State{Name: transition.To, Label: target.EnterLabel})
	}

	return labelcheck.Classify(input), true, nil
}

func resolveWorkflowLabelState(wf *config.WorkflowConfig, current labelcheck.State) (*config.State, error) {
	if wf == nil {
		return nil, fmt.Errorf("missing workflow")
	}
	if current.Name != "" {
		if state, ok := wf.States[current.Name]; ok && state != nil {
			return state, nil
		}
	}
	if current.Label != "" {
		stateNames := make([]string, 0, len(wf.States))
		for name := range wf.States {
			stateNames = append(stateNames, name)
		}
		sort.Strings(stateNames)
		for _, name := range stateNames {
			state := wf.States[name]
			if state != nil && state.EnterLabel == current.Label {
				return state, nil
			}
		}
	}
	return nil, fmt.Errorf("resolved state %q (%q) not found in workflow %q", current.Name, current.Label, wf.Name)
}

func exitCodeForValidation(result *launcher.Result) int {
	if result == nil {
		return -1
	}
	return result.ExitCode
}

func cloneLabels(labels []string) []string {
	if len(labels) == 0 {
		return nil
	}
	return append([]string(nil), labels...)
}

// recoverTasks marks running tasks as failed and re-routes pending tasks on restart.
func recoverTasks(st *store.Store) error {
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
