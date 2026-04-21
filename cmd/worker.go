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
	workerCmd.Flags().String("token", "", "Bearer token for Coordinator authentication (defaults to WORKBUDDY_AUTH_TOKEN)")
	workerCmd.Flags().String("role", "", "Comma-separated worker roles (default: roles from local agent config)")
	workerCmd.Flags().String("runtime", config.RuntimeClaudeCode, "Worker runtime capability: claude-code or codex")
	workerCmd.Flags().String("repo", "", "Repository in OWNER/NAME form (backward-compatible alias; path defaults to cwd)")
	workerCmd.Flags().String("repos", "", "Comma-separated OWNER/NAME=/path repo bindings")
	workerCmd.Flags().String("id", "", "Stable worker ID (default: hostname)")
	workerCmd.Flags().String("mgmt-addr", defaultWorkerMgmtAddr, "Local-only worker management listen address")
	workerCmd.Flags().Int("concurrency", 1, "Maximum concurrent tasks per worker")
	_ = workerCmd.MarkFlagRequired("coordinator")

	workerUnregisterCmd.Flags().String("coordinator", "", "Coordinator base URL")
	workerUnregisterCmd.Flags().String("token", "", "Bearer token for Coordinator authentication (defaults to WORKBUDDY_AUTH_TOKEN)")
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
	token, _ := cmd.Flags().GetString("token")
	workerID, _ := cmd.Flags().GetString("id")
	token = resolveWorkerToken(token)
	if token == "" {
		return fmt.Errorf("worker unregister: --token or WORKBUDDY_AUTH_TOKEN is required")
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
	token = resolveWorkerToken(token)
	if token == "" {
		return nil, fmt.Errorf("worker: --token or WORKBUDDY_AUTH_TOKEN is required")
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

func resolveWorkerToken(token string) string {
	token = strings.TrimSpace(token)
	if token != "" {
		return token
	}
	return strings.TrimSpace(os.Getenv("WORKBUDDY_AUTH_TOKEN"))
}

func runWorkerWithOpts(opts *workerOpts, lnch *runtimepkg.Registry, reader workerIssueReader, parentCtx ...context.Context) error {
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
	recorder := workersession.NewRecorder(localStore, opts.sessionsDir)
	rep.SetEventRecorder(recorder)
	rep.SetVerifier(reporter.NewGHClaimVerifier())
	bindings := newWorkerRepoBindingStore(repoBindings)
	workspaces := newWorkerWorkspaceSet()
	executor := workerexec.NewExecutor(lnch, reader)

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
			distributed := workerexec.NewDistributedWorker(workerexec.DistributedDeps{
				Config:            cfg,
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
