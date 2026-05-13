package launcher

import (
	"context"

	"github.com/Lincyaw/workbuddy/internal/labelwriter"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	"github.com/Lincyaw/workbuddy/internal/store"
)

// agentmLabelWriterAdapter implements runtimepkg.AgentMLabelWriter on top
// of internal/labelwriter. It is the v0.6 coordinator-managed label-write
// bridge (REQ-146 / #332): AgentM declares success, gitops opens the PR,
// then the bridge invokes this adapter to flip the issue label per
// Result.Meta["agentm_next_label"]. Per docs/decisions/
// 2026-05-13-k8s-agentm-otel.md (Block 2) and CLAUDE.md's explicit
// exception, only AgentM participates — claude-code and codex keep
// calling `gh issue edit` from inside the agent subprocess.
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
