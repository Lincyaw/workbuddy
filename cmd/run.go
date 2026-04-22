package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/store"
	workersession "github.com/Lincyaw/workbuddy/internal/worker/session"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

type runOpts struct {
	runtime    string
	prompt     string
	promptFile string
	workdir    string
	sandbox    string
	approval   string
	model      string
	timeout    time.Duration
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run a runtime directly through workbuddy",
	Long:  "Start a workbuddy runtime session directly, optionally asking Codex to review current repository changes.",
	RunE:  runRuntimeCmd,
}

func init() {
	runCmd.Flags().String("runtime", config.RuntimeCodex, "Runtime to start (e.g. codex, claude-code)")
	runCmd.Flags().StringP("prompt", "p", "", "Prompt to send to the runtime")
	runCmd.Flags().String("prompt-file", "", "Read prompt from file")
	runCmd.Flags().String("workdir", ".", "Working directory for the runtime session")
	runCmd.Flags().String("sandbox", "danger-full-access", "Runtime sandbox policy")
	runCmd.Flags().String("approval", "never", "Runtime approval policy")
	runCmd.Flags().String("model", "", "Optional runtime model override")
	runCmd.Flags().Duration("timeout", 30*time.Minute, "Runtime timeout")
	rootCmd.AddCommand(runCmd)
}

func runRuntimeCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseRunFlags(cmd)
	if err != nil {
		return err
	}
	return runRuntimeWithOpts(cmd.Context(), opts, launcher.NewLauncher(), os.Stdout, os.Stderr)
}

func parseRunFlags(cmd *cobra.Command) (*runOpts, error) {
	runtimeName, _ := cmd.Flags().GetString("runtime")
	prompt, _ := cmd.Flags().GetString("prompt")
	promptFile, _ := cmd.Flags().GetString("prompt-file")
	workdir, _ := cmd.Flags().GetString("workdir")
	sandbox, _ := cmd.Flags().GetString("sandbox")
	approval, _ := cmd.Flags().GetString("approval")
	model, _ := cmd.Flags().GetString("model")
	timeout, _ := cmd.Flags().GetDuration("timeout")

	if prompt != "" && promptFile != "" {
		return nil, fmt.Errorf("run: specify only one of --prompt or --prompt-file")
	}
	if strings.TrimSpace(runtimeName) == "" {
		return nil, fmt.Errorf("run: --runtime is required")
	}
	if workdir == "" {
		workdir = "."
	}
	return &runOpts{runtime: runtimeName, prompt: prompt, promptFile: promptFile, workdir: workdir, sandbox: sandbox, approval: approval, model: model, timeout: timeout}, nil
}

func runRuntimeWithOpts(ctx context.Context, opts *runOpts, lnch *runtimepkg.Registry, stdout, stderr io.Writer) error {
	workdir, err := filepath.Abs(opts.workdir)
	if err != nil {
		return fmt.Errorf("run: resolve workdir: %w", err)
	}
	prompt, err := resolveRunPrompt(opts)
	if err != nil {
		return err
	}

	agent := &config.AgentConfig{
		Name:    "cli-runtime",
		Runtime: opts.runtime,
		Prompt:  prompt,
		Policy: config.PolicyConfig{
			Sandbox:  opts.sandbox,
			Approval: opts.approval,
			Model:    opts.model,
			Timeout:  opts.timeout,
		},
		Timeout: opts.timeout,
	}
	if _, err := config.NormalizeAgentConfig(agent); err != nil {
		return err
	}

	task := &runtimepkg.TaskContext{
		Repo:     filepath.Base(workdir),
		RepoRoot: workdir,
		WorkDir:  workdir,
		Session:  runtimepkg.SessionContext{ID: "session-" + uuid.NewString()},
	}

	storePath := filepath.Join(workdir, ".workbuddy", "run.db")
	sessionsDir := filepath.Join(workdir, ".workbuddy", "sessions")
	st, err := store.NewStore(storePath)
	if err != nil {
		return fmt.Errorf("run: init local session store: %w", err)
	}
	defer func() { _ = st.Close() }()
	lnch.SetSessionManager(runtimepkg.NewSessionManager(sessionsDir, st))

	sess, err := lnch.Start(ctx, agent, task)
	if err != nil {
		if handle := task.SessionHandle(); handle != nil {
			_ = handle.Close(store.TaskStatusFailed)
		}
		return err
	}
	defer func() { _ = sess.Close() }()

	var result *runtimepkg.Result
	var runErr error
	defer func() {
		if handle := task.SessionHandle(); handle != nil {
			status := store.TaskStatusCompleted
			if runErr != nil || (result != nil && result.ExitCode != 0) {
				status = store.TaskStatusFailed
			}
			if result != nil && result.Meta != nil && result.Meta["timeout"] == "true" {
				status = store.TaskStatusTimeout
			}
			_ = handle.Close(status)
		}
	}()

	eventsCh := make(chan launcherevents.Event, 64)
	eventsPath, waitEvents := workersession.Stream(task, eventsCh)
	result, runErr = sess.Run(ctx, eventsCh)
	close(eventsCh)
	if err := waitEvents(); err != nil {
		return fmt.Errorf("run: capture session events: %w", err)
	}
	if runErr != nil {
		return runErr
	}
	if result == nil {
		return fmt.Errorf("run: runtime returned nil result")
	}
	if eventsPath != "" {
		if result.RawSessionPath == "" {
			result.RawSessionPath = result.SessionPath
		}
		result.SessionPath = eventsPath
	}

	if result.LastMessage != "" {
		_, _ = fmt.Fprintln(stdout, result.LastMessage)
	} else if result.Stdout != "" {
		_, _ = fmt.Fprintln(stdout, result.Stdout)
	}
	_, _ = fmt.Fprintf(stderr, "session=%s\nruntime=%s\nartifact=%s\n", task.Session.ID, agent.Runtime, result.SessionPath)
	if result.ExitCode != 0 {
		return fmt.Errorf("run: runtime exited with code %d", result.ExitCode)
	}
	return nil
}

func resolveRunPrompt(opts *runOpts) (string, error) {
	if opts.promptFile != "" {
		data, err := os.ReadFile(opts.promptFile)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return "", actionableError("run", fmt.Sprintf("prompt file %q was not found", opts.promptFile), "Create the file or pass the prompt directly with --prompt")
			}
			return "", fmt.Errorf("run: read prompt file %q: %w", opts.promptFile, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	if strings.TrimSpace(opts.prompt) != "" {
		return strings.TrimSpace(opts.prompt), nil
	}
	return "", fmt.Errorf("run: provide --prompt or --prompt-file")
}
