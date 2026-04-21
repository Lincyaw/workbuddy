package app

import (
	"fmt"
	"log"
	"time"

	"github.com/Lincyaw/workbuddy/internal/alertbus"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// RecoverTasks marks tasks that were running when the process died as failed
// and logs the count of pending tasks that will be re-dispatched through the
// next poll cycle. It is a startup step shared by both serve and coordinator
// topologies.
func RecoverTasks(st *store.Store, alertBus *alertbus.Bus) error {
	running, err := st.QueryTasks(store.TaskStatusRunning)
	if err != nil {
		return fmt.Errorf("query running tasks: %w", err)
	}
	for _, t := range running {
		log.Printf("[app] recovery: marking task %s as failed (was running)", t.ID)
		if err := st.UpdateTaskStatus(t.ID, store.TaskStatusFailed); err != nil {
			log.Printf("[app] recovery: failed to mark task %s: %v", t.ID, err)
		}
		if alertBus != nil {
			alertBus.Publish(alertbus.AlertEvent{
				Kind:      alertbus.KindOrphanedTask,
				Severity:  alertbus.SeverityWarn,
				Repo:      t.Repo,
				IssueNum:  t.IssueNum,
				AgentName: t.AgentName,
				Timestamp: time.Now().Unix(),
				Payload: map[string]any{
					"task_id": t.ID,
					"status":  store.TaskStatusFailed,
				},
			})
		}
	}

	pending, err := st.QueryTasks(store.TaskStatusPending)
	if err != nil {
		return fmt.Errorf("query pending tasks: %w", err)
	}
	if len(pending) > 0 {
		log.Printf("[app] recovery: %d pending tasks will be re-routed via next poll cycle", len(pending))
	}

	return nil
}
