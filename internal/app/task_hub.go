package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Lincyaw/workbuddy/internal/router"
	"github.com/Lincyaw/workbuddy/internal/tasknotify"
)

// DefaultMaxParallelTasks is the cap used by the embedded worker when the
// caller leaves --max-parallel-tasks unset.
const DefaultMaxParallelTasks = 4

// DefaultWorkerParallelism picks the natural worker concurrency:
// min(NumCPU, DefaultMaxParallelTasks).
func DefaultWorkerParallelism() int {
	if runtime.NumCPU() < DefaultMaxParallelTasks {
		return runtime.NumCPU()
	}
	return DefaultMaxParallelTasks
}

// PublishTaskCompletion pushes a task completion event onto the shared hub
// so any SSE clients watching /tasks/watch are notified.
func PublishTaskCompletion(hub *tasknotify.Hub, task router.WorkerTask, status string, exitCode int, startedAt, completedAt time.Time) {
	if hub == nil {
		return
	}
	hub.Publish(tasknotify.TaskEvent{
		TaskID:      task.TaskID,
		Repo:        task.Repo,
		IssueNum:    task.IssueNum,
		AgentName:   task.AgentName,
		Status:      status,
		ExitCode:    exitCode,
		DurationMS:  completedAt.Sub(startedAt).Milliseconds(),
		StartedAt:   startedAt,
		CompletedAt: completedAt,
	})
}

// NewTaskWatchHandler returns an SSE handler that streams one task_complete
// event and then closes. Supports repo and issue query-string filters.
func NewTaskWatchHandler(hub *tasknotify.Hub) http.HandlerFunc {
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
