package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Lincyaw/workbuddy/internal/audit"
	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/reporter"
	"github.com/Lincyaw/workbuddy/internal/staleinference"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/workerclient"
	"github.com/Lincyaw/workbuddy/internal/workspace"
	"github.com/spf13/cobra"
)

const (
	defaultWorkerPollTimeout      = 30 * time.Second
	defaultWorkerHeartbeat        = 15 * time.Second
	defaultWorkerShutdownDeadline = 5 * time.Second
	defaultWorkerTaskAPITimeout   = 5 * time.Second
	// postSessionDrainTimeout bounds how long executeRemoteTask waits for
	// the stale-inference proxy and the event-log writer to drain after
	// session.Run returns. Under normal load both complete within
	// milliseconds; a longer wait means a channel is wedged (slow disk,
	// full channel back-pressure). When the bound trips we force-abort the
	// drain so the worker slot can be released promptly — the cost is a
	// truncated events log tail.
	postSessionDrainTimeout = 5 * time.Second
)

type workerOpts struct {
	coordinatorURL    string
	token             string
	roleCSV           string
	runtime           string
	repo              string
	reposCSV          string
	workerID          string
	mgmtAddr          string
	configDir         string
	workDir           string
	sessionsDir       string
	dbPath            string
	pollTimeout       time.Duration
	heartbeatInterval time.Duration
	shutdownTimeout   time.Duration
	concurrency       int
}

type workerIssueReader interface {
	issueLabelReader
	ReadIssue(repo string, issueNum int) (poller.IssueDetails, error)
}

var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Run a standalone Worker that long-polls a Coordinator",
	Long:  "Start the standalone Worker process, register capabilities with the remote Coordinator, and execute assigned tasks.",
	RunE:  runWorker,
}

var workerUnregisterCmd = &cobra.Command{
	Use:   "unregister",
	Short: "Unregister a worker from the coordinator",
	RunE:  runWorkerUnregister,
}

func init() {
	workerCmd.Flags().String("coordinator", "", "Coordinator base URL")
	workerCmd.Flags().String("token", "", "Bearer token for Coordinator authentication")
	workerCmd.Flags().String("role", "", "Comma-separated worker roles (default: roles from local agent config)")
	workerCmd.Flags().String("runtime", config.RuntimeClaudeCode, "Worker runtime capability: claude-code or codex")
	workerCmd.Flags().String("repo", "", "Repository in OWNER/NAME form (backward-compatible alias; path defaults to cwd)")
	workerCmd.Flags().String("repos", "", "Comma-separated OWNER/NAME=/path repo bindings")
	workerCmd.Flags().String("id", "", "Stable worker ID (default: hostname)")
	workerCmd.Flags().String("mgmt-addr", defaultWorkerMgmtAddr, "Local-only worker management listen address")
	workerCmd.Flags().Int("concurrency", 1, "Maximum concurrent tasks per worker")
	_ = workerCmd.MarkFlagRequired("coordinator")
	_ = workerCmd.MarkFlagRequired("token")

	workerUnregisterCmd.Flags().String("coordinator", "", "Coordinator base URL")
	workerUnregisterCmd.Flags().String("token", "", "Bearer token for Coordinator authentication")
	workerUnregisterCmd.Flags().String("id", "", "Worker ID to unregister")
	_ = workerUnregisterCmd.MarkFlagRequired("coordinator")
	_ = workerUnregisterCmd.MarkFlagRequired("token")
	_ = workerUnregisterCmd.MarkFlagRequired("id")

	workerReposCmd.AddCommand(workerReposAddCmd, workerReposRemoveCmd, workerReposListCmd)
	workerCmd.AddCommand(workerReposCmd)
	workerCmd.AddCommand(workerUnregisterCmd)
	rootCmd.AddCommand(workerCmd)
}

func runWorker(cmd *cobra.Command, _ []string) error {
	opts, err := parseWorkerFlags(cmd)
	if err != nil {
		return err
	}
	return runWorkerWithOpts(opts, nil, nil)
}

func runWorkerUnregister(cmd *cobra.Command, _ []string) error {
	coordinatorURL, _ := cmd.Flags().GetString("coordinator")
	token, _ := cmd.Flags().GetString("token")
	workerID, _ := cmd.Flags().GetString("id")

	client := workerclient.New(strings.TrimSpace(coordinatorURL), strings.TrimSpace(token), nil)
	if err := client.Unregister(cmd.Context(), strings.TrimSpace(workerID)); err != nil {
		return fmt.Errorf("worker unregister: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "unregistered worker %s\n", workerID)
	return nil
}

func parseWorkerFlags(cmd *cobra.Command) (*workerOpts, error) {
	coordinatorURL, _ := cmd.Flags().GetString("coordinator")
	token, _ := cmd.Flags().GetString("token")
	roleCSV, _ := cmd.Flags().GetString("role")
	runtimeName, _ := cmd.Flags().GetString("runtime")
	repo, _ := cmd.Flags().GetString("repo")
	reposCSV, _ := cmd.Flags().GetString("repos")
	workerID, _ := cmd.Flags().GetString("id")
	mgmtAddr, _ := cmd.Flags().GetString("mgmt-addr")
	concurrency, _ := cmd.Flags().GetInt("concurrency")
	if concurrency < 1 {
		concurrency = 1
	}
	return &workerOpts{
		coordinatorURL:    strings.TrimSpace(coordinatorURL),
		token:             strings.TrimSpace(token),
		roleCSV:           roleCSV,
		runtime:           runtimeName,
		repo:              strings.TrimSpace(repo),
		reposCSV:          strings.TrimSpace(reposCSV),
		workerID:          strings.TrimSpace(workerID),
		mgmtAddr:          strings.TrimSpace(mgmtAddr),
		configDir:         ".github/workbuddy",
		pollTimeout:       defaultWorkerPollTimeout,
		heartbeatInterval: defaultWorkerHeartbeat,
		shutdownTimeout:   defaultWorkerShutdownDeadline,
		concurrency:       concurrency,
	}, nil
}

func runWorkerWithOpts(opts *workerOpts, lnch *launcher.Launcher, reader workerIssueReader, parentCtx ...context.Context) error {
	if opts == nil {
		return fmt.Errorf("worker: options are required")
	}
	cfg, warnings, err := config.LoadConfig(opts.configDir)
	if err != nil {
		return fmt.Errorf("worker: load config: %w", err)
	}
	for _, w := range warnings {
		log.Printf("[worker] warning: %s", w)
	}

	workDir := opts.workDir
	if workDir == "" {
		workDir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("worker: get working directory: %w", err)
		}
	}
	workDir, err = filepath.Abs(workDir)
	if err != nil {
		return fmt.Errorf("worker: resolve working directory: %w", err)
	}
	repoBindings, err := resolveWorkerRepoBindings(opts, strings.TrimSpace(cfg.Global.Repo), workDir)
	if err != nil {
		return err
	}

	if opts.dbPath == "" {
		opts.dbPath = filepath.Join(workDir, ".workbuddy", "worker.db")
	}
	if opts.sessionsDir == "" {
		opts.sessionsDir = filepath.Join(workDir, ".workbuddy", "sessions")
	}

	publicRuntime, runtimeAlias, err := normalizeWorkerRuntime(opts.runtime)
	if err != nil {
		return err
	}
	roles := parseWorkerRoles(opts.roleCSV, cfg.Agents)
	if len(roles) == 0 {
		return fmt.Errorf("worker: at least one role is required")
	}

	workerID := strings.TrimSpace(opts.workerID)
	if workerID == "" {
		hostname, _ := os.Hostname()
		if hostname == "" {
			hostname = "worker"
		}
		workerID = hostname
	}

	if lnch == nil {
		lnch = launcher.NewLauncher()
	}
	if reader == nil {
		reader = &GHCLIReader{}
	}

	localStore, err := store.NewStore(opts.dbPath)
	if err != nil {
		return fmt.Errorf("worker: init local store: %w", err)
	}
	defer func() { _ = localStore.Close() }()

	client := workerclient.New(opts.coordinatorURL, opts.token, nil)
	rep := reporter.NewReporter(&reporter.GHCLIWriter{})
	rep.SetEventRecorder(eventlog.NewEventLogger(localStore))
	rep.SetVerifier(reporter.NewGHClaimVerifier())
	auditor := audit.NewAuditor(localStore, opts.sessionsDir)
	bindings := newWorkerRepoBindingStore(repoBindings)
	workspaces := newWorkerWorkspaceSet()

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

	if sigCh != nil {
		go func() {
			select {
			case sig := <-sigCh:
				log.Printf("[worker] received signal %s, shutting down...", sig)
				cancel()
			case <-ctx.Done():
			}
		}()
	}

	addrFile := workerAddrFile(workDir)
	mgmtServer, err := startWorkerMgmtServer(opts.mgmtAddr, addrFile, bindings, func(changeCtx context.Context, _ []string) error {
		return registerWorkerRepos(changeCtx, client, workerID, roles, publicRuntime, bindings.list())
	})
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), opts.shutdownTimeout)
		defer shutdownCancel()
		if err := mgmtServer.Close(shutdownCtx); err != nil {
			log.Printf("[worker] mgmt server shutdown failed: %v", err)
		}
	}()

	if err := registerWorkerRepos(ctx, client, workerID, roles, publicRuntime, bindings.list()); err != nil {
		if errors.Is(err, workerclient.ErrUnauthorized) {
			return fmt.Errorf("worker: coordinator rejected the provided token")
		}
		return fmt.Errorf("worker: register with coordinator: %w", err)
	}

	concurrency := opts.concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var (
		taskErrMu  sync.Mutex
		taskErrVal error
	)

	for {
		if ctx.Err() != nil {
			break
		}
		task, err := client.PollTask(ctx, workerID, opts.pollTimeout)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				break
			}
			if errors.Is(err, workerclient.ErrUnauthorized) {
				wg.Wait()
				return fmt.Errorf("worker: coordinator rejected the provided token")
			}
			wg.Wait()
			return fmt.Errorf("worker: poll task: %w", err)
		}
		if task == nil {
			continue
		}
		repoPath, ok := bindings.get(task.Repo)
		if !ok {
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), opts.shutdownTimeout)
			err := client.ReleaseTask(releaseCtx, task.TaskID, workerclient.ReleaseRequest{WorkerID: workerID, Reason: "repo not bound on worker"})
			releaseCancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[worker] failed to release unmapped task %s for repo %s: %v", task.TaskID, task.Repo, err)
			}
			continue
		}

		// Acquire a semaphore slot (blocks if all slots are in use).
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break
		}
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		go func(t *workerclient.Task) {
			defer func() { <-sem; wg.Done() }()
			wsMgr := workspaces.forRepoPath(repoPath, func(path string) workspaceManager {
				return workspace.NewManager(path)
			})
			if err := executeRemoteTask(ctx, t, client, cfg, lnch, auditor, rep, reader, repoPath, opts.sessionsDir, workerID, runtimeAlias, opts.heartbeatInterval, opts.shutdownTimeout, wsMgr); err != nil {
				taskErrMu.Lock()
				if taskErrVal == nil {
					taskErrVal = err
				}
				taskErrMu.Unlock()
				log.Printf("[worker] task %s error: %v", t.TaskID, err)
			}
		}(task)
	}

	// Wait for all in-flight tasks to finish before exiting.
	wg.Wait()

	taskErrMu.Lock()
	defer taskErrMu.Unlock()
	return taskErrVal
}

func executeRemoteTask(ctx context.Context, task *workerclient.Task, client *workerclient.Client, cfg *config.FullConfig, lnch *launcher.Launcher, auditor *audit.Auditor, rep *reporter.Reporter, reader workerIssueReader, workDir, sessionsDir, workerID, runtimeAlias string, heartbeatInterval, shutdownTimeout time.Duration, wsMgr workspaceManager) error {
	agentCfg, ok := cfg.Agents[task.AgentName]
	if !ok {
		return fmt.Errorf("worker: agent %q not found in local config", task.AgentName)
	}
	agentCopy := *agentCfg
	if runtimeAlias != "" {
		agentCopy.Runtime = runtimeAlias
	}

	launchCtx := buildRemoteTaskContext(task, reader, workDir)
	sessionID := launchCtx.Session.ID

	// Start heartbeat BEFORE worktree setup. `git worktree add` on a large
	// repo with many concurrent claims can block well past the 30s task lease;
	// a silent lease expiry there lets the same worker re-claim the same task
	// from its own poll loop, producing two parallel goroutines for one task
	// (see issue #143 and the chain to #141).
	taskCtx, taskCancel := context.WithCancel(ctx)
	defer taskCancel()

	var released atomic.Bool
	releaseDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			releaseCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancel()
			if err := client.ReleaseTask(releaseCtx, task.TaskID, workerclient.ReleaseRequest{WorkerID: workerID, Reason: "worker shutdown"}); err == nil {
				released.Store(true)
			}
			taskCancel()
		case <-releaseDone:
		}
	}()
	defer close(releaseDone)

	heartbeatStop := make(chan struct{})
	heartbeatDone := make(chan struct{})
	var heartbeatStopOnce sync.Once
	stopHeartbeat := func() {
		heartbeatStopOnce.Do(func() {
			close(heartbeatStop)
		})
		<-heartbeatDone
	}
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				hbCtx, cancel := context.WithTimeout(context.Background(), boundedWorkerTaskAPITimeout(heartbeatInterval))
				err := client.Heartbeat(hbCtx, task.TaskID, workerclient.HeartbeatRequest{WorkerID: workerID})
				cancel()
				if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, workerclient.ErrUnauthorized) {
					// Coordinator returns HTTP 400 "worker_id does not match claimed task"
					// when another claimer (often this same worker's poll loop after a
					// lease expiry) has taken over. Stop executing: a second goroutine
					// already owns this task and will run it; continuing here just
					// races against it on the same worktree. See #143.
					if isTaskOwnershipLost(err) {
						log.Printf("[worker] heartbeat reports ownership lost for task %s: %v — cancelling local execution", task.TaskID, err)
						taskCancel()
						return
					}
					log.Printf("[worker] heartbeat failed for task %s: %v", task.TaskID, err)
				}
			case <-heartbeatStop:
				return
			case <-taskCtx.Done():
				return
			}
		}
	}()
	defer stopHeartbeat()

	// Create an isolated worktree for this task so multiple agents
	// don't interfere with each other's git state.
	var worktreePath string
	if wsMgr != nil {
		wt, err := wsMgr.Create(task.IssueNum, task.TaskID)
		if err != nil {
			log.Printf("[worker] failed to create worktree for issue #%d: %v", task.IssueNum, err)
			// Report the worktree failure as a user-visible comment.
			result := &launcher.Result{
				ExitCode: 1,
				Stderr:   err.Error(),
			}
			reportCtx, cancel := context.WithTimeout(context.Background(), boundedWorkerTaskAPITimeout(shutdownTimeout))
			if rerr := rep.Report(reportCtx, task.Repo, task.IssueNum, task.AgentName, result, sessionID, workerID, 0, workflowMaxRetries(cfg, task.Workflow), "", ""); rerr != nil {
				log.Printf("[worker] failed to report worktree failure: %v", rerr)
			}
			cancel()
			// Requeue the claimed task so it can be retried after the worktree issue is fixed.
			releaseCtx, cancel := context.WithTimeout(context.Background(), boundedWorkerTaskAPITimeout(shutdownTimeout))
			if rerr := client.ReleaseTask(releaseCtx, task.TaskID, workerclient.ReleaseRequest{
				WorkerID: workerID,
				Reason:   fmt.Sprintf("worktree setup failed: %v", err),
			}); rerr != nil {
				log.Printf("[worker] failed to release task after worktree setup failure: %v", rerr)
			}
			cancel()
			return fmt.Errorf("worker: worktree setup failed for issue #%d: %w", task.IssueNum, err)
		}
		workDir = wt
		worktreePath = wt
		launchCtx.RepoRoot = wt
		launchCtx.WorkDir = wt
		log.Printf("[worker] using worktree %s for issue #%d", wt, task.IssueNum)
	}
	defer func() {
		if worktreePath != "" && wsMgr != nil {
			if err := wsMgr.Remove(worktreePath); err != nil {
				log.Printf("[worker] worktree cleanup failed for issue #%d: %v", task.IssueNum, err)
			}
		}
	}()

	sessionID = launchCtx.Session.ID
	if err := rep.ReportStarted(taskCtx, task.Repo, task.IssueNum, task.AgentName, sessionID, workerID); err != nil {
		log.Printf("[worker] report started failed: %v", err)
	}

	session, err := lnch.Start(taskCtx, &agentCopy, launchCtx)
	if err != nil {
		return fmt.Errorf("worker: start agent %s: %w", task.AgentName, err)
	}
	defer func() { _ = session.Close() }()

	eventsCh := make(chan launcherevents.Event, 64)
	eventsPath, waitEvents := streamSessionEvents(launchCtx, eventsCh)

	// Set up stale inference watchdog channel wrapping.
	// When enabled, events flow through a proxy channel that records
	// activity timestamps; otherwise session writes directly to eventsCh.
	siCfg := cfg.Worker.StaleInference
	sessionCh := eventsCh // channel passed to session.Run
	var proxyDone chan struct{}
	if siCfg.StaleInferenceEnabled() {
		tracker := staleinference.NewEventTracker()
		watchdogCtx, watchdogCancel := context.WithCancel(taskCtx)
		defer watchdogCancel()
		go staleinference.Watch(watchdogCtx, staleinference.Config{
			IdleThreshold:        siCfg.IdleThreshold,
			CheckInterval:        siCfg.CheckInterval,
			CompletedGracePeriod: siCfg.CompletedGracePeriod,
		}, tracker, taskCancel)

		proxyCh := make(chan launcherevents.Event, 64)
		proxyDone = make(chan struct{})
		go func() {
			defer close(proxyDone)
			for evt := range proxyCh {
				if evt.Kind == launcherevents.KindTaskComplete {
					tracker.RecordCompletion()
				} else {
					tracker.RecordActivity()
				}
				select {
				case eventsCh <- evt:
				case <-taskCtx.Done():
					// Drain remaining events without blocking if context is cancelled.
					for range proxyCh {
					}
					return
				}
			}
		}()
		sessionCh = proxyCh
	}

	result, runErr := session.Run(taskCtx, sessionCh)
	// Drain the stale-inference proxy. A closed sessionCh (= proxyCh) only
	// makes the proxy's `for evt := range proxyCh` loop exit when the body
	// isn't currently blocked on `eventsCh <- evt`. Under concurrent load
	// eventsCh can back up (streamSessionEvents writes to disk, proxy has
	// cap=64 input/output); when it does, the proxy parks inside the
	// select{} and close(sessionCh) is not enough to release it. Bound the
	// wait and fall back to taskCancel — the proxy has a
	// `case <-taskCtx.Done()` escape that drains without sending. Without
	// this, proxy wedges, <-proxyDone hangs, executeRemoteTask never returns,
	// and the worker slot is held forever with heartbeat still firing.
	// See #143 follow-up: post-session zombie.
	if proxyDone != nil {
		close(sessionCh)
		select {
		case <-proxyDone:
		case <-time.After(postSessionDrainTimeout):
			log.Printf("[worker] proxy drain timed out for task %s after %s; cancelling task ctx to unblock", task.TaskID, postSessionDrainTimeout)
			taskCancel()
			<-proxyDone
		}
	}
	close(eventsCh)
	// Same story for the events writer: bound the wait so a slow consumer
	// cannot trap executeRemoteTask in the cleanup phase.
	waitDone := make(chan error, 1)
	go func() { waitDone <- waitEvents() }()
	select {
	case werr := <-waitDone:
		if werr != nil {
			log.Printf("[worker] event capture failed: %v", werr)
		}
	case <-time.After(postSessionDrainTimeout):
		log.Printf("[worker] event stream drain timed out for task %s after %s; dropping tail of events log", task.TaskID, postSessionDrainTimeout)
	}
	stopHeartbeat()
	if result == nil {
		// session.Run returned nil — we have no verdict to attribute. Classify
		// as infra failure so the coordinator does not mark the agent as
		// having FAILED. See issue #131 / AC-3.
		result = &launcher.Result{
			ExitCode: -1,
			Stderr:   runErrString(runErr),
			Meta: map[string]string{
				launcher.MetaInfraFailure:       "true",
				launcher.MetaInfraFailureReason: "session.Run returned nil result",
			},
		}
	}
	if result.Meta == nil {
		result.Meta = map[string]string{}
	}
	if eventsPath != "" {
		if result.RawSessionPath == "" {
			result.RawSessionPath = result.SessionPath
		}
		result.SessionPath = eventsPath
	}
	if runErr != nil && result.Stderr == "" {
		result.Stderr = runErr.Error()
	}
	if err := auditor.Capture(sessionID, task.TaskID, task.Repo, task.IssueNum, task.AgentName, result); err != nil {
		log.Printf("[worker] audit capture failed: %v", err)
	}
	if ctx.Err() != nil && !released.Load() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		err := client.ReleaseTask(releaseCtx, task.TaskID, workerclient.ReleaseRequest{WorkerID: workerID, Reason: "worker shutdown"})
		cancel()
		if err == nil {
			released.Store(true)
		}
	}
	if released.Load() {
		return nil
	}

	currentLabels, err := snapshotIssueLabels(task.Repo, task.IssueNum, reader)
	if err != nil {
		currentLabels = append([]string(nil), launchCtx.Issue.Labels...)
	}

	status := store.TaskStatusCompleted
	if runErr != nil || result.ExitCode != 0 {
		status = store.TaskStatusFailed
	}
	if result.Meta["timeout"] == "true" {
		status = store.TaskStatusTimeout
	}

	// Verify agent claims before submitting result to coordinator.
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), boundedWorkerTaskAPITimeout(shutdownTimeout))
	verifyRes, verifyErr := rep.Verify(verifyCtx, task.Repo, task.IssueNum, result)
	verifyCancel()
	if verifyErr != nil {
		log.Printf("[worker] claim verification error for task %s: %v", task.TaskID, verifyErr)
	}
	if verifyRes != nil && verifyRes.Partial {
		log.Printf("[worker] agent %s for %s#%d claimed side-effects not verified — treating as failure", task.AgentName, task.Repo, task.IssueNum)
		status = store.TaskStatusFailed
	}

	// Detect launcher-layer infra failure so the coordinator does not treat
	// this as an agent FAIL verdict. See issue #131 / AC-3.
	infraFailure := launcher.IsInfraFailure(result)
	infraReason := ""
	if infraFailure && result.Meta != nil {
		infraReason = result.Meta[launcher.MetaInfraFailureReason]
	}
	submitCtx, cancel := context.WithTimeout(context.Background(), boundedWorkerTaskAPITimeout(shutdownTimeout))
	defer cancel()
	if err := client.SubmitResult(submitCtx, task.TaskID, workerclient.ResultRequest{
		WorkerID:      workerID,
		Status:        status,
		CurrentLabels: currentLabels,
		InfraFailure:  infraFailure,
		InfraReason:   infraReason,
	}); err != nil {
		log.Printf("[worker] submit result failed for task %s: %v", task.TaskID, err)
		return nil
	}
	reportCtx, reportCancel := context.WithTimeout(context.Background(), boundedWorkerTaskAPITimeout(shutdownTimeout))
	defer reportCancel()
	if err := rep.ReportVerified(reportCtx, task.Repo, task.IssueNum, task.AgentName, result, sessionID, workerID, 0, workflowMaxRetries(cfg, task.Workflow), "", workDir, verifyRes); err != nil {
		log.Printf("[worker] report failed: %v", err)
	}
	return nil
}

func boundedWorkerTaskAPITimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 || timeout > defaultWorkerTaskAPITimeout {
		return defaultWorkerTaskAPITimeout
	}
	return timeout
}

// isTaskOwnershipLost returns true when err from a coordinator task endpoint
// indicates this worker no longer owns the task — typically because the lease
// expired and another claimer (or this worker's own poll loop) took over.
// Heartbeat/submit callers should stop executing when this is true.
func isTaskOwnershipLost(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "worker_id does not match claimed task") ||
		strings.Contains(msg, "task already completed") ||
		strings.Contains(msg, "not claimable by this worker") ||
		strings.Contains(msg, "task is no longer owned by worker")
}

func buildRemoteTaskContext(task *workerclient.Task, reader workerIssueReader, workDir string) *launcher.TaskContext {
	issue := launcher.IssueContext{Number: task.IssueNum}
	if reader != nil {
		if details, err := reader.ReadIssue(task.Repo, task.IssueNum); err == nil {
			issue.Body = details.Body
			issue.Labels = append([]string(nil), details.Labels...)
		}
	}
	return &launcher.TaskContext{
		Issue:    issue,
		Repo:     task.Repo,
		RepoRoot: workDir,
		WorkDir:  workDir,
		Session: launcher.SessionContext{
			ID: fmt.Sprintf("session-%s", task.TaskID),
		},
	}
}

func parseWorkerRoles(raw string, agents map[string]*config.AgentConfig) []string {
	if strings.TrimSpace(raw) != "" {
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			role := strings.TrimSpace(part)
			if role == "" || slices.Contains(out, role) {
				continue
			}
			out = append(out, role)
		}
		return out
	}
	var out []string
	for _, agent := range agents {
		if agent == nil || agent.Role == "" || slices.Contains(out, agent.Role) {
			continue
		}
		out = append(out, agent.Role)
	}
	slices.Sort(out)
	return out
}

func normalizeWorkerRuntime(raw string) (public string, runtimeAlias string, err error) {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "", config.RuntimeClaudeCode:
		return config.RuntimeClaudeCode, config.RuntimeClaudeCode, nil
	case config.RuntimeCodex, config.RuntimeCodexServer:
		return config.RuntimeCodex, config.RuntimeCodex, nil
	default:
		return "", "", fmt.Errorf("worker: unsupported runtime %q (want claude-code or codex)", raw)
	}
}

func hostnameOrUnknown() string {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return "unknown"
	}
	return hostname
}

func workflowMaxRetries(cfg *config.FullConfig, workflow string) int {
	if cfg == nil || cfg.Workflows == nil {
		return 3
	}
	if wf, ok := cfg.Workflows[workflow]; ok && wf.MaxRetries > 0 {
		return wf.MaxRetries
	}
	return 3
}

func runErrString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
