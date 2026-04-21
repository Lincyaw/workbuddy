package runtime

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

type GHASession struct {
	Agent  *config.AgentConfig
	Task   *TaskContext
	Client *gha.Client
}

func NewGHASession(agent *config.AgentConfig, task *TaskContext) Session {
	return &GHASession{
		Agent:  agent,
		Task:   task,
		Client: gha.NewClient(),
	}
}

func (s *GHASession) Run(ctx context.Context, events chan<- launcherevents.Event) (*Result, error) {
	timeout := s.Agent.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ref, err := s.resolveRef(execCtx)
	if err != nil {
		return nil, err
	}
	artifactDir := filepath.Join(SessionArtifactDir(s.Task), "remote-runner")
	var seq uint64
	EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindTurnStarted, launcherevents.TurnStartedPayload{TurnID: s.Task.Session.ID}, nil)
	EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindLog, launcherevents.LogPayload{
		Stream: "stdout",
		Line:   fmt.Sprintf("dispatching GitHub Actions workflow %s on ref %s", s.Agent.GitHubActions.Workflow, ref),
	}, nil)

	start := time.Now()
	outcome, runErr := s.Client.RunWorkflow(execCtx, gha.Config{
		Repo:         s.Task.Repo,
		IssueNumber:  s.Task.Issue.Number,
		AgentName:    s.Agent.Name,
		SessionID:    s.Task.Session.ID,
		Workflow:     s.Agent.GitHubActions.Workflow,
		Ref:          ref,
		PollInterval: s.Agent.GitHubActions.PollInterval,
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
		// Classify every pre-completion failure (dispatch / poll / logs / artifact
		// download) as an infrastructure failure: the agent never produced a
		// verdict, so downstream retry/escalation must not treat this as an
		// agent-reported failure.
		MarkInfraFailure(result, ghaInfraReason(runErr, code))
		EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindError, launcherevents.ErrorPayload{Code: code, Message: runErr.Error(), Recoverable: false}, nil)
		EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: s.Task.Session.ID, Status: "error"}, nil)
		return result, runErr
	}

	result, err := s.resultFromOutcome(outcome, duration)
	if err != nil {
		// Artifact parse failures mean the runner succeeded but we cannot read
		// its verdict; treat as infra failure so retries do not count it as a
		// legitimate agent failure.
		parseResult := &Result{
			ExitCode: -1,
			Duration: duration,
			Stderr:   err.Error(),
			Meta: map[string]string{
				"runner": config.RunnerGitHubActions,
			},
		}
		if outcome != nil {
			parseResult.Stdout = outcome.Logs
			parseResult.SessionPath = outcome.CanonicalSessionPath
			if parseResult.SessionPath == "" {
				parseResult.SessionPath = outcome.LogPath
			}
		}
		MarkInfraFailure(parseResult, "gha: artifact parse")
		EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindError, launcherevents.ErrorPayload{Code: "artifact_parse", Message: err.Error(), Recoverable: false}, nil)
		EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: s.Task.Session.ID, Status: "error"}, nil)
		return parseResult, err
	}
	if err := ValidateOutputContract(s.Agent, result); err != nil {
		EmitOutputContractFailure(events, &seq, s.Task.Session.ID, s.Task.Session.ID, err, EmitEvent)
		return result, err
	}
	EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindLog, launcherevents.LogPayload{
		Stream: "stdout",
		Line:   fmt.Sprintf("workflow run %d completed with conclusion %s", outcome.Run.ID, outcome.Run.Conclusion),
	}, nil)
	status := "ok"
	if result.ExitCode != 0 {
		status = "error"
		EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindError, launcherevents.ErrorPayload{
			Code:        "gha_conclusion",
			Message:     fmt.Sprintf("workflow run %d concluded %s", outcome.Run.ID, outcome.Run.Conclusion),
			Recoverable: false,
		}, nil)
	}
	EmitEvent(events, &seq, s.Task.Session.ID, s.Task.Session.ID, launcherevents.KindTurnCompleted, launcherevents.TurnCompletedPayload{TurnID: s.Task.Session.ID, Status: status}, nil)
	return result, nil
}

func (s *GHASession) resultFromOutcome(outcome *gha.Outcome, duration time.Duration) (*Result, error) {
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
		return nil, fmt.Errorf("runtime: github-actions: read result artifact: %w", err)
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("runtime: github-actions: parse result artifact: %w", err)
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
		if candidate := FindDownloadedPath(outcome.Files, payload.SessionPath); candidate != "" {
			result.SessionPath = candidate
		}
	}
	if payload.RawSessionPath != "" {
		if candidate := FindDownloadedPath(outcome.Files, payload.RawSessionPath); candidate != "" {
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

func FindDownloadedPath(files []string, reported string) string {
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

func (s *GHASession) resolveRef(ctx context.Context) (string, error) {
	if ref := strings.TrimSpace(s.Agent.GitHubActions.Ref); ref != "" {
		return ref, nil
	}
	repoDir := s.Task.WorkDir
	if repoDir == "" {
		repoDir = s.Task.RepoRoot
	}
	if repoDir == "" {
		return "main", nil
	}
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("runtime: github-actions: resolve ref: %s: %w", strings.TrimSpace(string(out)), err)
	}
	ref := strings.TrimSpace(string(out))
	if ref == "" {
		return "main", nil
	}
	return ref, nil
}

func (s *GHASession) SetApprover(Approver) error { return ErrNotSupported }

func (s *GHASession) Close() error { return nil }

// ghaInfraReason maps a RunWorkflow error to the infra-failure reason string.
// It uses the typed FailureStage attached by the gha client; timeout/cancelled
// contexts fall back to a generic reason so the caller's `code` still reflects
// the context state.
func ghaInfraReason(err error, code string) string {
	switch code {
	case "timeout":
		return "gha: workflow timed out"
	case "cancelled":
		return "gha: run cancelled"
	}
	switch gha.StageOf(err) {
	case gha.StageDispatch:
		return "gha: workflow dispatch failed"
	case gha.StagePoll:
		return "gha: workflow poll failed"
	case gha.StageLogs:
		return "gha: logs download failed"
	case gha.StageArtifacts:
		return "gha: artifact download failed"
	}
	return "gha: runtime failure"
}

func SessionArtifactDir(task *TaskContext) string {
	baseDir := task.RepoRoot
	if baseDir == "" {
		baseDir = task.WorkDir
	}
	if baseDir == "" {
		baseDir = "."
	}
	return filepath.Join(baseDir, ".workbuddy", "sessions", task.Session.ID)
}
