package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/launcher"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/workerclient"
)

type staleInferenceKill struct {
	ArtifactPath string
	IdleDuration time.Duration
	PID          int
}

type staleInferenceMonitorDeps struct {
	stat               func(string) (os.FileInfo, error)
	hasRunningChildren func(int) (bool, error)
	killProcessGroup   func(int) error
}

func startStaleInferenceMonitor(
	parent context.Context,
	cfg config.EffectiveStaleInferenceConfig,
	session launcher.Session,
	task *workerclient.Task,
	workerID string,
	evlog *eventlog.EventLogger,
	deps staleInferenceMonitorDeps,
) func() *staleInferenceKill {
	staleSession, ok := session.(launcher.StaleInferenceSession)
	if !ok || !cfg.Enabled {
		return func() *staleInferenceKill { return nil }
	}
	if cfg.IdleThreshold <= 0 || cfg.CheckInterval <= 0 {
		return func() *staleInferenceKill { return nil }
	}
	if deps.stat == nil {
		deps.stat = os.Stat
	}
	if deps.hasRunningChildren == nil {
		deps.hasRunningChildren = hasRunningChildren
	}
	if deps.killProcessGroup == nil {
		deps.killProcessGroup = killProcessGroup
	}

	ctx, cancel := context.WithCancel(parent)
	done := make(chan *staleInferenceKill, 1)
	go func() {
		ticker := time.NewTicker(cfg.CheckInterval)
		defer ticker.Stop()
		defer close(done)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				info := staleSession.StaleInferenceInfo()
				if info.PID <= 0 || strings.TrimSpace(info.ArtifactPath) == "" {
					continue
				}

				fileInfo, err := deps.stat(info.ArtifactPath)
				if err != nil {
					continue
				}
				idle := time.Since(fileInfo.ModTime())
				if idle < cfg.IdleThreshold {
					continue
				}

				children, err := deps.hasRunningChildren(info.PID)
				if err != nil || children {
					continue
				}

				payload := map[string]any{
					"agent":         task.AgentName,
					"artifact_path": info.ArtifactPath,
					"idle_seconds":  int(idle.Seconds()),
					"pid":           info.PID,
					"session_dir":   filepath.Dir(info.ArtifactPath),
					"task_id":       task.TaskID,
					"worker_id":     workerID,
				}
				if evlog != nil {
					evlog.Log(eventlog.TypeAgentStaleInference, task.Repo, task.IssueNum, payload)
				}

				if err := deps.killProcessGroup(info.PID); err != nil && !errors.Is(err, syscall.ESRCH) {
					payload["kill_error"] = err.Error()
					if evlog != nil {
						evlog.Log(eventlog.TypeError, task.Repo, task.IssueNum, payload)
					}
					continue
				}

				done <- &staleInferenceKill{
					ArtifactPath: info.ArtifactPath,
					IdleDuration: idle,
					PID:          info.PID,
				}
				return
			}
		}
	}()

	return func() *staleInferenceKill {
		cancel()
		for kill := range done {
			return kill
		}
		return nil
	}
}

func staleInferenceStatus(task *workerclient.Task, cfg *config.FullConfig, labels []string) string {
	if task == nil || cfg == nil {
		return store.TaskStatusFailed
	}
	workflow := cfg.Workflows[task.Workflow]
	if workflow == nil {
		return store.TaskStatusFailed
	}
	current := workflow.States[task.State]
	if current == nil || strings.TrimSpace(current.EnterLabel) == "" {
		return store.TaskStatusFailed
	}
	if slices.Contains(labels, current.EnterLabel) {
		return store.TaskStatusFailed
	}

	for _, transition := range current.Transitions {
		if strings.TrimSpace(transition.To) == "" {
			continue
		}
		next := workflow.States[transition.To]
		if next == nil || strings.TrimSpace(next.EnterLabel) == "" {
			continue
		}
		if slices.Contains(labels, next.EnterLabel) {
			return store.TaskStatusCompleted
		}
	}
	return store.TaskStatusFailed
}

func hasRunningChildren(pid int) (bool, error) {
	if pid <= 0 {
		return false, fmt.Errorf("invalid pid %d", pid)
	}
	path := fmt.Sprintf("/proc/%d/task/%d/children", pid, pid)
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(data)) != "", nil
}

func killProcessGroup(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	return syscall.Kill(-pid, syscall.SIGKILL)
}
