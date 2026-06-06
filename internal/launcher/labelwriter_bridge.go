package launcher

import (
	"context"

	"github.com/Lincyaw/workbuddy/internal/labelwriter"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// agentmLabelWriterAdapter implements runtimepkg.AgentMLabelWriter on top
// of internal/labelwriter. It is the coordinator-managed label-write
// bridge (REQ-146 / #332): after a runtime emits a next_label, the bridge
// invokes this adapter to flip the issue label per
// Result.Meta["agentm_next_label"]. The writer is wired only onto runtimes
// whose Capabilities().ManagesOwnLabels == false (currently agentm); the
// agent self-publishes its branch/PR in autonomous mode (no Go-side gitops
// publish gate). Runtimes that manage their own labels (claude-code, codex)
// keep calling `gh issue edit` from inside the agent subprocess and never
// get this adapter. See docs/decisions/2026-06-06-runtime-strategy-and-convergence.md §1/§3.
type agentmLabelWriterAdapter struct {
	writer *labelwriter.Writer
}

// NewAgentMLabelWriterAdapter builds the production adapter that the
// AgentM bridge consults after a successful gitops publish. A nil store
// yields an adapter whose ApplyNextLabel is a no-op so that unit tests
// (and bootstrap paths that build a launcher without a store) don't
// panic. Production callers always pass the real store.
func NewAgentMLabelWriterAdapter(s store.Store) runtimepkg.AgentMLabelWriter {
	if s == nil {
		return &agentmLabelWriterAdapter{}
	}
	return &agentmLabelWriterAdapter{writer: labelwriter.New(s)}
}

func (a *agentmLabelWriterAdapter) ApplyNextLabel(ctx context.Context, repo string, issueNum int, label string) error {
	if a == nil || a.writer == nil {
		return nil
	}
	return a.writer.ApplyNextLabel(ctx, repo, issueNum, label)
}
