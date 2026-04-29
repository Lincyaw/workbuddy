package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/eventlog"
	"github.com/Lincyaw/workbuddy/internal/reporter"
	"github.com/Lincyaw/workbuddy/internal/statemachine"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// cycleCapReporterAdapter bridges statemachine.CycleCapReporter to the
// production reporter.Reporter. It exists because internal/reporter must not
// import internal/statemachine (avoids a wiring-package cycle).
type cycleCapReporterAdapter struct {
	rep *reporter.Reporter
}

func (a *cycleCapReporterAdapter) ReportDevReviewCycleCap(ctx context.Context, repo string, issueNum int, info statemachine.CycleCapInfo) error {
	if a == nil || a.rep == nil {
		return nil
	}
	return a.rep.ReportDevReviewCycleCap(ctx, repo, issueNum, info.WorkflowName, info.CycleCount, info.MaxReviewCycles, info.HitAt)
}

// CycleCapTrailLoader reads completed-task events for an issue and converts
// failed/timed-out review-agent runs into a chronological digest the Reporter
// embeds in its needs-human comment. The digest is intentionally built from
// stored events — no agent re-invocation is required (REQ-085 AC-3).
type cycleCapTrailLoader struct {
	store *store.Store
}

// NewCycleCapTrailLoader builds the production trail loader bound to the
// given store. Exposed for wiring in repo_runtime.go.
func NewCycleCapTrailLoader(st *store.Store) reporter.CycleCapTrailLoader {
	return &cycleCapTrailLoader{store: st}
}

func (l *cycleCapTrailLoader) LoadCycleCapTrail(_ context.Context, repo string, issueNum int) ([]reporter.CycleRejectionEntry, string, string, error) {
	if l == nil || l.store == nil {
		return nil, "", "", nil
	}
	events, err := l.store.QueryEventsFiltered(store.EventQueryFilter{
		Repo:     repo,
		IssueNum: issueNum,
		Type:     eventlog.TypeCompleted,
	})
	if err != nil {
		return nil, "", "", fmt.Errorf("load cycle-cap trail events: %w", err)
	}
	out := make([]reporter.CycleRejectionEntry, 0, len(events))
	for _, ev := range events {
		var payload struct {
			AgentName string `json:"agent_name"`
			ExitCode  int    `json:"exit_code"`
		}
		if strings.TrimSpace(ev.Payload) == "" {
			continue
		}
		if err := json.Unmarshal([]byte(ev.Payload), &payload); err != nil {
			continue
		}
		// Only review-agent failures count toward the rejection trail. A
		// dev-agent failure is a separate signal handled by the per-agent
		// failure cap; surfacing it here would muddy the digest.
		if payload.AgentName != "review-agent" {
			continue
		}
		if payload.ExitCode == 0 {
			continue
		}
		out = append(out, reporter.CycleRejectionEntry{
			Timestamp: ev.TS.UTC(),
			Summary:   fmt.Sprintf("review-agent rejected (exit=%d)", payload.ExitCode),
		})
	}
	return out, "", "", nil
}
