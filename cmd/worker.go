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
	"syscall"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	"github.com/Lincyaw/workbuddy/internal/poller"
	"github.com/Lincyaw/workbuddy/internal/reporter"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/store"
	workerexec "github.com/Lincyaw/workbuddy/internal/worker"
	workersession "github.com/Lincyaw/workbuddy/internal/worker/session"
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
	addCoordinatorAuthFlags(workerCmd.Flags(), "", "Bearer token for Coordinator authentication")
	workerCmd.Flags().String("role", "", "Comma-separated worker roles (default: roles from local agent config)")
	workerCmd.Flags().String("runtime", config.RuntimeClaudeCode, "Worker runtime capability: claude-code or codex")
	workerCmd.Flags().String("config-dir", ".github/workbuddy", "Configuration directory (relative to each bound repo unless absolute)")
	workerCmd.Flags().String("repo", "", "Repository in OWNER/NAME form (backward-compatible alias; path defaults to cwd)")
	workerCmd.Flags().String("repos", "", "Comma-separated OWNER/NAME=/path repo bindings")
	workerCmd.Flags().String("id", "", "Stable worker ID (default: hostname)")
	workerCmd.Flags().String("mgmt-addr", defaultWorkerMgmtAddr, "Local-only worker management listen address")
	workerCmd.Flags().Int("concurrency", 1, "Maximum concurrent tasks per worker")
	_ = workerCmd.MarkFlagRequired("coordinator")

	workerUnregisterCmd.Flags().String("coordinator", "", "Coordinator base URL")
	addCoordinatorAuthFlags(workerUnregisterCmd.Flags(), "", "Bearer token for Coordinator authentication")
	workerUnregisterCmd.Flags().String("id", "", "Worker ID to unregister")
	_ = workerUnregisterCmd.MarkFlagRequired("coordinator")
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
	workerID, _ := cmd.Flags().GetString("id")
	token, err := resolveCoordinatorAuthToken(cmd, "worker unregister")
	if err != nil {
		return err
	}
	if token == "" {
		return fmt.Errorf("worker unregister: --token-file, deprecated --token, or WORKBUDDY_AUTH_TOKEN is required")
	}

	client := workerclient.New(strings.TrimSpace(coordinatorURL), strings.TrimSpace(token), nil)
	if err := client.Unregister(cmd.Context(), strings.TrimSpace(workerID)); err != nil {
		return fmt.Errorf("worker unregister: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "unregistered worker %s\n", workerID)
	return nil
}

func parseWorkerFlags(cmd *cobra.Command) (*workerOpts, error) {
	coordinatorURL, _ := cmd.Flags().GetString("coordinator")
	roleCSV, _ := cmd.Flags().GetString("role")
	runtimeName, _ := cmd.Flags().GetString("runtime")
	configDir, _ := cmd.Flags().GetString("config-dir")
	repo, _ := cmd.Flags().GetString("repo")
	reposCSV, _ := cmd.Flags().GetString("repos")
	workerID, _ := cmd.Flags().GetString("id")
	mgmtAddr, _ := cmd.Flags().GetString("mgmt-addr")
	concurrency, _ := cmd.Flags().GetInt("concurrency")
	if concurrency < 1 {
		concurrency = 1
	}
	token, err := resolveCoordinatorAuthToken(cmd, "worker")
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, fmt.Errorf("worker: --token-file, deprecated --token, or WORKBUDDY_AUTH_TOKEN is required")
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
		configDir:         strings.TrimSpace(configDir),
		pollTimeout:       defaultWorkerPollTimeout,
		heartbeatInterval: defaultWorkerHeartbeat,
		shutdownTimeout:   defaultWorkerShutdownDeadline,
		concurrency:       concurrency,
	}, nil
}

func runWorkerWithOpts(opts *workerOpts, lnch *runtimepkg.Registry, reader workerIssueReader, parentCtx ...context.Context) error {
	if opts == nil {
		return fmt.Errorf("worker: options are required")
	}

	var err error
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

	configRepo := ""
	if strings.TrimSpace(opts.reposCSV) == "" && strings.TrimSpace(opts.repo) == "" {
		bootstrapDir := resolveWorkerConfigDir(workDir, opts.configDir)
		bootstrapCfg, _, err := config.LoadConfig(bootstrapDir)
		if err != nil {
			return fmt.Errorf("worker: load bootstrap config: %w", err)
		}
		configRepo = strings.TrimSpace(bootstrapCfg.Global.Repo)
	}

	repoBindings, err := resolveWorkerRepoBindings(opts, configRepo, workDir)
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

	// Wire the session manager so the runtime registry creates a ManagedSession
	// per task and populates taskCtx.SessionHandle(). Without this, the bridge
	// pump races a non-existent session stream reader and can wedge on a full
	// events channel, holding the per-issue execution lock until stale-inference
	// tears the agent down at 30m.
	lnch.SetSessionManager(runtimepkg.NewSessionManager(opts.sessionsDir, localStore))

	client := workerclient.New(opts.coordinatorURL, opts.token, nil)
	rep := reporter.NewReporter(&reporter.GHCLIWriter{})
	recorder := workersession.NewRecorder(localStore, opts.sessionsDir)
	rep.SetEventRecorder(recorder)
	rep.SetVerifier(reporter.NewGHClaimVerifier())
	bindings := newWorkerRepoBindingStore(repoBindings)
	configs := newWorkerRepoConfigStore(opts.configDir)
	workspaces := newWorkerWorkspaceSet()
	executor := workerexec.NewExecutor(lnch, reader)

	if _, err := configs.reload(bindings.list()); err != nil {
		return err
	}
	roles := parseWorkerRoles(opts.roleCSV, configs.list())
	if len(roles) == 0 {
		return fmt.Errorf("worker: at least one role is required")
	}

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
	reloadAndRegister := func(changeCtx context.Context) (*workerConfigReloadSummary, error) {
		summary, err := configs.reload(bindings.list())
		if err != nil {
			return nil, err
		}
		currentRoles := parseWorkerRoles(opts.roleCSV, configs.list())
		if len(currentRoles) == 0 {
			return nil, fmt.Errorf("worker: at least one role is required")
		}
		if err := registerWorkerRepos(changeCtx, client, workerID, currentRoles, publicRuntime, bindings.list()); err != nil {
			return nil, err
		}
		return summary, nil
	}

	mgmtServer, err := startWorkerMgmtServer(
		opts.mgmtAddr,
		addrFile,
		bindings,
		func(changeCtx context.Context, _ []string) error {
			_, err := reloadAndRegister(changeCtx)
			return err
		},
		func(reloadCtx context.Context) (any, error) {
			return reloadAndRegister(reloadCtx)
		},
	)
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
			return &cliExitError{
				msg:  "worker: coordinator rejected the provided token",
				code: ExitCodeUnauthorized,
			}
		}
		return fmt.Errorf("worker: register with coordinator: %w", err)
	}

	concurrency := opts.concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

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
				return &cliExitError{
					msg:  "worker: coordinator rejected the provided token",
					code: ExitCodeUnauthorized,
				}
			}
			wg.Wait()
			return fmt.Errorf("worker: poll task: %w", err)
		}
		if task == nil {
			continue
		}
		repoPath, pathOK := bindings.get(task.Repo)
		repoCfg, cfgOK := configs.get(task.Repo)
		if !pathOK || !cfgOK {
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), opts.shutdownTimeout)
			reason := "repo not bound on worker"
			if pathOK && !cfgOK {
				reason = "repo config not loaded on worker"
			}
			err := client.ReleaseTask(releaseCtx, task.TaskID, workerclient.ReleaseRequest{WorkerID: workerID, Reason: reason})
			releaseCancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[worker] failed to release task %s for repo %s: %v", task.TaskID, task.Repo, err)
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
			distributed := workerexec.NewDistributedWorker(workerexec.DistributedDeps{
				Config:            repoCfg,
				Executor:          executor,
				Recorder:          recorder,
				Reporter:          rep,
				Reader:            reader,
				Client:            client,
				WorkDir:           repoPath,
				WorkerID:          workerID,
				RuntimeAlias:      runtimeAlias,
				HeartbeatInterval: opts.heartbeatInterval,
				ShutdownTimeout:   opts.shutdownTimeout,
				WorkspaceManager:  wsMgr,
			})
			if err := distributed.ExecuteTask(ctx, t); err != nil {
				// Per-task failures (including non-retryable coordinator
				// SubmitResult failures) are logged and surfaced via the
				// reporter/release paths, but must not fail the whole
				// worker process — the poll loop continues with the next
				// task.
				log.Printf("[worker] task %s error: %v", t.TaskID, err)
			}
		}(task)
	}

	// Wait for all in-flight tasks to finish before exiting.
	wg.Wait()

	// Stop any shared agent-backend resources (codex app-server, etc.).
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), opts.shutdownTimeout)
	defer shutdownCancel()
	if err := lnch.Shutdown(shutdownCtx); err != nil {
		log.Printf("[worker] runtime shutdown error: %v", err)
	}
	return nil
}

func executeRemoteTask(ctx context.Context, task *workerclient.Task, client *workerclient.Client, cfg *config.FullConfig, executor *workerexec.Executor, recorder *workersession.Recorder, rep *reporter.Reporter, reader workerIssueReader, workDir, workerID, runtimeAlias string, heartbeatInterval, shutdownTimeout time.Duration, wsMgr workspaceManager) error {
	distributed := workerexec.NewDistributedWorker(workerexec.DistributedDeps{
		Config:            cfg,
		Executor:          executor,
		Recorder:          recorder,
		Reporter:          rep,
		Reader:            reader,
		Client:            client,
		WorkDir:           workDir,
		WorkerID:          workerID,
		RuntimeAlias:      runtimeAlias,
		HeartbeatInterval: heartbeatInterval,
		ShutdownTimeout:   shutdownTimeout,
		WorkspaceManager:  wsMgr,
	})
	return distributed.ExecuteTask(ctx, task)
}

func boundedWorkerTaskAPITimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 || timeout > defaultWorkerTaskAPITimeout {
		return defaultWorkerTaskAPITimeout
	}
	return timeout
}

func isTaskOwnershipLost(err error) bool {
	return workerexec.IsTaskOwnershipLost(err)
}

func buildRemoteTaskContext(task *workerclient.Task, reader workerIssueReader, workDir string) *runtimepkg.TaskContext {
	return workerexec.BuildRemoteTaskContext(task, reader, workDir)
}

func parseWorkerRoles(raw string, configs []*config.FullConfig) []string {
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
	for _, cfg := range configs {
		if cfg == nil {
			continue
		}
		for _, agent := range cfg.Agents {
			if agent == nil || agent.Role == "" || slices.Contains(out, agent.Role) {
				continue
			}
			out = append(out, agent.Role)
		}
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
