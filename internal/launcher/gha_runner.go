package launcher

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	"github.com/Lincyaw/workbuddy/internal/launcher/runners/gha"
)

type ghaSession struct {
	agent  *config.AgentConfig
	task   *TaskContext
	client *gha.Client
}

func newGHASession(agent *config.AgentConfig, task *TaskContext) Session {
	return &ghaSession{
		agent:  agent,
		task:   task,
		client: gha.NewClient(),
	}
}

func (s *ghaSession) Run(ctx context.Context, events chan<- launcherevents.Event) (*Result, error) {
	timeout := s.agent.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ref, err := s.resolveRef(execCtx)
	if err != nil {
		return nil, err
	}
	artifactDir := filepath.Join(sessionArtifactDir(s.task), "remote-runner")
	var seq uint64
	emitEvent(events, &seq, s.task.Session.ID, s.task.Session.ID, launcherevents.KindTurnStarted, launcherevents.TurnStartedPayload{TurnID: s.task.Session.ID}, nil)
	emitEvent(events, &seq, s.task.Session.ID, s.task.Session.ID, launcherevents.KindLog, launcherevents.LogPayload{
		Stream: "stdout",
		Line:   fmt.Sprintf("dispatching GitHub Actions workflow %s on ref %s", s.agent.GitHubActions.Workflow, ref),
	}, nil)

	start := time.Now()
	outcome, runErr := s.client.RunWorkflow(execCtx, gha.Config{
		Repo:         s.task.Repo,
		IssueNumber:  s.task.Issue.Number,
		AgentName:    s.agent.Name,
		SessionID:    s.task.Session.ID,
		Workflow:     s.agent.GitHubActions.Workflow,
		Ref:          ref,
		PollInterval: s.agent.GitHubActions.PollInterval,
		ArtifactDir:  artifactDir,
	})
	duration := time.Since(start)
	if runErr != nil {
		code := "exec"
		if execCtx.Err() == context.DeadlineExceeded {
			code = "timeout"
		} else if ctx.Err() != nil {
			code = "cancelled"
		}
		result := &Result{
			ExitCode: -1,
			Duration: duration,
			Meta:     map[string]string{"runner": config.RunnerGitHubActions},
		}
		if outcome != nil {
			result.Stdout = outcome.Logs
			result.SessionPath = outcome.LogPath
		}
		if code == "timeout" {
			result.Meta["timeout"] = "true"
		}
		emitEvent(events, &seq, s.task.Session.ID, s.task.Session.ID, launcherevents.KindError, launcherevents.ErrorPayload{Code: code, Message: runErr.Error(), Recoverable: false}, nil)
		emitEvent(events, &seq, s.task.Session.ID, s.task.Session.ID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: s.task.Session.ID, Status: "error"}, nil)
		return result, runErr
	}

	result, err := s.resultFromOutcome(outcome, duration)
	if err != nil {
		emitEvent(events, &seq, s.task.Session.ID, s.task.Session.ID, launcherevents.KindError, launcherevents.ErrorPayload{Code: "artifact_parse", Message: err.Error(), Recoverable: false}, nil)
		emitEvent(events, &seq, s.task.Session.ID, s.task.Session.ID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: s.task.Session.ID, Status: "error"}, nil)
		return nil, err
	}
	if err := validateOutputContract(s.agent, result); err != nil {
		emitOutputContractFailure(events, &seq, s.task.Session.ID, s.task.Session.ID, err)
		return result, err
	}
	emitEvent(events, &seq, s.task.Session.ID, s.task.Session.ID, launcherevents.KindLog, launcherevents.LogPayload{
		Stream: "stdout",
		Line:   fmt.Sprintf("workflow run %d completed with conclusion %s", outcome.Run.ID, outcome.Run.Conclusion),
	}, nil)
	status := "ok"
	if result.ExitCode != 0 {
		status = "error"
		emitEvent(events, &seq, s.task.Session.ID, s.task.Session.ID, launcherevents.KindError, launcherevents.ErrorPayload{
			Code:        "gha_conclusion",
			Message:     fmt.Sprintf("workflow run %d concluded %s", outcome.Run.ID, outcome.Run.Conclusion),
			Recoverable: false,
		}, nil)
	}
	emitEvent(events, &seq, s.task.Session.ID, s.task.Session.ID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: s.task.Session.ID, Status: status}, nil)
	return result, nil
}

func (s *ghaSession) resultFromOutcome(outcome *gha.Outcome, duration time.Duration) (*Result, error) {
	result := &Result{
		Duration:    duration,
		Stdout:      outcome.Logs,
		SessionPath: outcome.CanonicalSessionPath,
		Meta: map[string]string{
			"runner":  config.RunnerGitHubActions,
			"run_id":  fmt.Sprintf("%d", outcome.Run.ID),
			"run_url": outcome.Run.HTMLURL,
		},
	}
	if result.SessionPath == "" {
		result.SessionPath = outcome.LogPath
	}

	switch outcome.Run.Conclusion {
	case "", "success":
		result.ExitCode = 0
	default:
		result.ExitCode = 1
		result.Stderr = outcome.Logs
	}

	if outcome.ResultPath == "" {
		return result, nil
	}
	var payload struct {
		ExitCode       *int                              `json:"exit_code"`
		Stdout         string                            `json:"stdout"`
		Stderr         string                            `json:"stderr"`
		LastMessage    string                            `json:"last_message"`
		Meta           map[string]string                 `json:"meta"`
		SessionPath    string                            `json:"session_path"`
		RawSessionPath string                            `json:"raw_session_path"`
		SessionRef     SessionRef                        `json:"session_ref"`
		TokenUsage     *launcherevents.TokenUsagePayload `json:"token_usage"`
	}
	data, err := os.ReadFile(outcome.ResultPath)
	if err != nil {
		return nil, fmt.Errorf("launcher: github-actions: read result artifact: %w", err)
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("launcher: github-actions: parse result artifact: %w", err)
	}
	if payload.ExitCode != nil {
		result.ExitCode = *payload.ExitCode
	}
	if payload.Stdout != "" {
		result.Stdout = payload.Stdout
	}
	if payload.Stderr != "" {
		result.Stderr = payload.Stderr
	}
	if payload.LastMessage != "" {
		result.LastMessage = payload.LastMessage
	}
	if payload.Meta != nil {
		for k, v := range payload.Meta {
			result.Meta[k] = v
		}
	}
	if payload.SessionPath != "" {
		if candidate := findDownloadedPath(outcome.Files, payload.SessionPath); candidate != "" {
			result.SessionPath = candidate
		}
	}
	if payload.RawSessionPath != "" {
		if candidate := findDownloadedPath(outcome.Files, payload.RawSessionPath); candidate != "" {
			result.RawSessionPath = candidate
		}
	}
	result.SessionRef = payload.SessionRef
	result.TokenUsage = payload.TokenUsage
	if result.RawSessionPath == "" && outcome.CanonicalSessionPath != "" && outcome.CanonicalSessionPath != result.SessionPath {
		result.RawSessionPath = outcome.CanonicalSessionPath
	}
	return result, nil
}

func findDownloadedPath(files []string, reported string) string {
	reported = filepath.Base(strings.TrimSpace(reported))
	if reported == "" {
		return ""
	}
	for _, file := range files {
		if filepath.Base(file) == reported {
			return file
		}
	}
	return ""
}

func (s *ghaSession) resolveRef(ctx context.Context) (string, error) {
	if ref := strings.TrimSpace(s.agent.GitHubActions.Ref); ref != "" {
		return ref, nil
	}
	repoDir := s.task.WorkDir
	if repoDir == "" {
		repoDir = s.task.RepoRoot
	}
	if repoDir == "" {
		return "main", nil
	}
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("launcher: github-actions: resolve ref: %s: %w", strings.TrimSpace(string(out)), err)
	}
	ref := strings.TrimSpace(string(out))
	if ref == "" {
		return "main", nil
	}
	return ref, nil
}

func (s *ghaSession) SetApprover(Approver) error { return ErrNotSupported }

func (s *ghaSession) Close() error { return nil }

func sessionArtifactDir(task *TaskContext) string {
	baseDir := task.RepoRoot
	if baseDir == "" {
		baseDir = task.WorkDir
	}
	if baseDir == "" {
		baseDir = "."
	}
	return filepath.Join(baseDir, ".workbuddy", "sessions", task.Session.ID)
}
