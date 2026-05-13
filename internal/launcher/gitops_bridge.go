package launcher

import (
	"context"
	"errors"
	"fmt"

	"github.com/Lincyaw/workbuddy/internal/gitops"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
)

// agentmGitOpsAdapter implements runtimepkg.AgentMGitOps on top of
// internal/gitops. It is the v0.6 coordinator-managed publish bridge:
// AgentM declares success, the bridge invokes this adapter, the adapter
// shells out to `git` + `gh` to commit and open a PR. Per
// docs/decisions/2026-05-13-k8s-agentm-otel.md (Block 2) only AgentM
// participates — claude-code and codex remain self-managed.
type agentmGitOpsAdapter struct {
	client *gitops.Client
	author gitops.Author
}

// NewAgentMGitOpsAdapter builds the production adapter that the AgentM
// bridge consults after a successful run. Passing a nil client falls back
// to the gitops package zero value (ExecRunner + `git`/`gh` binaries on
// PATH); passing an empty author falls back to gitops.DefaultBotAuthor.
func NewAgentMGitOpsAdapter(client *gitops.Client, author gitops.Author) runtimepkg.AgentMGitOps {
	if client == nil {
		client = &gitops.Client{}
	}
	if author.Name == "" || author.Email == "" {
		author = gitops.DefaultBotAuthor
	}
	return &agentmGitOpsAdapter{client: client, author: author}
}

func (a *agentmGitOpsAdapter) PublishArtifact(ctx context.Context, req runtimepkg.AgentMPublishRequest) (string, error) {
	if err := a.client.CommitAndPush(ctx, req.RepoLocalPath, req.Branch, req.CommitMessage, a.author); err != nil {
		if errors.Is(err, gitops.ErrNoChanges) {
			return "", runtimepkg.ErrNoChangesToPublish
		}
		return "", fmt.Errorf("commit-push: %w", err)
	}
	prURL, err := a.client.OpenPR(ctx, req.Repo, req.Branch, req.PRTitle, req.PRBody)
	if err != nil {
		return "", fmt.Errorf("open-pr: %w", err)
	}
	return prURL, nil
}
