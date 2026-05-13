package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/agent"
	"github.com/Lincyaw/workbuddy/internal/agent/agentm"
	"github.com/Lincyaw/workbuddy/internal/agent/agentm/agentmtest"
	"github.com/Lincyaw/workbuddy/internal/config"
)

// fakeGitOps records the PublishArtifact calls so tests can assert the
// bridge wired the right repo/branch/title/body off of the AgentM output.
type fakeGitOps struct {
	calls  []AgentMPublishRequest
	prURL  string
	err    error
	noDiff bool
}

func (f *fakeGitOps) PublishArtifact(_ context.Context, req AgentMPublishRequest) (string, error) {
	f.calls = append(f.calls, req)
	if f.noDiff {
		return "", ErrNoChangesToPublish
	}
	if f.err != nil {
		return "", f.err
	}
	return f.prURL, nil
}

// AC-1-1 / AC-1-2: successful AgentM run with a GitOps adapter must
// trigger PublishArtifact and surface the PR URL on Result.Meta.
func TestAgentMBridge_PublishOnSuccess(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{Mode: agentmtest.ModeSuccess})
	gops := &fakeGitOps{prURL: "https://github.com/Lincyaw/workbuddy/pull/501"}
	rt := NewAgentBridgeRuntime(config.RuntimeAgentM, func() (agent.Backend, error) {
		return &agentm.Backend{Binary: fake}, nil
	})
	rt.GitOps = gops

	work := t.TempDir()
	task := &TaskContext{
		Repo:    "Lincyaw/workbuddy",
		WorkDir: work,
		Issue:   IssueContext{Number: 330, Title: "AgentM publish"},
		Session: SessionContext{ID: "test-session"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := rt.Launch(ctx, &config.AgentConfig{Name: "dev-agent", Runtime: config.RuntimeAgentM}, task)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if len(gops.calls) != 1 {
		t.Fatalf("expected 1 publish call, got %d", len(gops.calls))
	}
	call := gops.calls[0]
	if call.Repo != "Lincyaw/workbuddy" || call.IssueNumber != 330 {
		t.Fatalf("bad call routing: %+v", call)
	}
	if call.Branch != "workbuddy/issue-330" {
		t.Fatalf("branch = %q", call.Branch)
	}
	// AC-1-2: PR body references the originating issue.
	if !contains(call.PRBody, "#330") {
		t.Fatalf("PR body should reference #330, got %q", call.PRBody)
	}
	if res.Meta["pr_url"] != "https://github.com/Lincyaw/workbuddy/pull/501" {
		t.Fatalf("pr_url meta = %q", res.Meta["pr_url"])
	}
	if res.Meta["agentm_publish"] != "published" {
		t.Fatalf("agentm_publish meta = %q", res.Meta["agentm_publish"])
	}
	if IsInfraFailure(res) {
		t.Fatalf("successful publish should not be infra failure")
	}
}

// AC-1-3: failed AgentM runs (success=false) must NOT push/PR. The
// reporter surfaces the failure_reason instead via the existing
// agentm_failure_reason meta key.
func TestAgentMBridge_NoPublishOnFailure(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{
		Mode:          agentmtest.ModeFailure,
		NextLabel:     "status:failed",
		FailureReason: "tests did not pass",
	})
	gops := &fakeGitOps{}
	rt := NewAgentBridgeRuntime(config.RuntimeAgentM, func() (agent.Backend, error) {
		return &agentm.Backend{Binary: fake}, nil
	})
	rt.GitOps = gops

	work := t.TempDir()
	task := &TaskContext{
		Repo:    "Lincyaw/workbuddy",
		WorkDir: work,
		Issue:   IssueContext{Number: 330},
		Session: SessionContext{ID: "test-session"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := rt.Launch(ctx, &config.AgentConfig{Name: "dev-agent", Runtime: config.RuntimeAgentM}, task)
	if err == nil {
		t.Fatal("expected non-nil err for clean failure")
	}
	if len(gops.calls) != 0 {
		t.Fatalf("expected 0 publish calls, got %d", len(gops.calls))
	}
	if res.Meta["agentm_failure_reason"] != "tests did not pass" {
		t.Fatalf("failure_reason meta = %q", res.Meta["agentm_failure_reason"])
	}
	if res.Meta["pr_url"] != "" {
		t.Fatalf("pr_url should be empty on failure, got %q", res.Meta["pr_url"])
	}
}

// Publish-side infrastructure failure: AgentM succeeded but the
// coordinator could not commit/push. We mark the result as infra failure
// so the reporter shows it distinctly.
func TestAgentMBridge_PublishFailureIsInfraFailure(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{Mode: agentmtest.ModeSuccess})
	gops := &fakeGitOps{err: errors.New("commit-push: git push: permission denied")}
	rt := NewAgentBridgeRuntime(config.RuntimeAgentM, func() (agent.Backend, error) {
		return &agentm.Backend{Binary: fake}, nil
	})
	rt.GitOps = gops

	work := t.TempDir()
	task := &TaskContext{
		Repo:    "Lincyaw/workbuddy",
		WorkDir: work,
		Issue:   IssueContext{Number: 330},
		Session: SessionContext{ID: "test-session"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, _ := rt.Launch(ctx, &config.AgentConfig{Name: "dev-agent", Runtime: config.RuntimeAgentM}, task)
	if !IsInfraFailure(res) {
		t.Fatalf("expected infra failure on publish error, meta=%v", res.Meta)
	}
	if !contains(res.Meta[MetaInfraFailureReason], "permission denied") {
		t.Fatalf("infra reason should carry git error, got %q", res.Meta[MetaInfraFailureReason])
	}
}

// No-changes from gitops should NOT escalate to infra failure: AgentM is
// telling us the task was already satisfied.
func TestAgentMBridge_PublishNoChanges(t *testing.T) {
	fake := agentmtest.BuildFake(t, agentmtest.Config{Mode: agentmtest.ModeSuccess})
	gops := &fakeGitOps{noDiff: true}
	rt := NewAgentBridgeRuntime(config.RuntimeAgentM, func() (agent.Backend, error) {
		return &agentm.Backend{Binary: fake}, nil
	})
	rt.GitOps = gops

	work := t.TempDir()
	task := &TaskContext{
		Repo:    "Lincyaw/workbuddy",
		WorkDir: work,
		Issue:   IssueContext{Number: 330},
		Session: SessionContext{ID: "test-session"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := rt.Launch(ctx, &config.AgentConfig{Name: "dev-agent", Runtime: config.RuntimeAgentM}, task)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if IsInfraFailure(res) {
		t.Fatal("no-changes must not be infra failure")
	}
	if res.Meta["agentm_publish"] != "no_changes" {
		t.Fatalf("agentm_publish meta = %q", res.Meta["agentm_publish"])
	}
	if res.Meta["pr_url"] != "" {
		t.Fatalf("pr_url should be empty when no changes, got %q", res.Meta["pr_url"])
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
