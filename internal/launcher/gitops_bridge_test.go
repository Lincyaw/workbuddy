package launcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/gitops"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
)

// scriptedRunner returns canned (stdout, stderr, err) results for
// sequential calls so the adapter test asserts both halves of
// PublishArtifact (git commit/push then gh pr create) without touching
// the network. The .git/ precondition check still runs against a real
// temp directory.
type scriptedRunner struct {
	calls []string
	idx   int
	out   []scriptedResult
}

type scriptedResult struct {
	stdout string
	stderr string
	err    error
}

func (s *scriptedRunner) Run(_ context.Context, _ string, _ []string, name string, args ...string) (string, string, error) {
	s.calls = append(s.calls, name+" "+strings.Join(args, " "))
	if s.idx >= len(s.out) {
		return "", "", nil
	}
	res := s.out[s.idx]
	s.idx++
	return res.stdout, res.stderr, res.err
}

func TestAgentMGitOpsAdapter_HappyPath(t *testing.T) {
	sr := &scriptedRunner{out: []scriptedResult{
		{}, // git checkout
		{}, // git add
		{stdout: " M feature.txt\n"},             // git status --porcelain
		{}, // git commit
		{}, // git push
		{stdout: "https://example.com/pull/9\n"}, // gh pr create
	}}
	client := &gitops.Client{Runner: sr}
	adapter := NewAgentMGitOpsAdapter(client, gitops.Author{})

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	prURL, err := adapter.PublishArtifact(context.Background(), runtimepkg.AgentMPublishRequest{
		Repo:          "Lincyaw/workbuddy",
		IssueNumber:   330,
		Branch:        "workbuddy/issue-330",
		CommitMessage: "msg",
		PRTitle:       "title",
		PRBody:        "body",
		RepoLocalPath: dir,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if prURL != "https://example.com/pull/9" {
		t.Fatalf("pr url = %q", prURL)
	}
	if len(sr.calls) != 6 {
		t.Fatalf("expected 6 subprocess calls, got %d: %v", len(sr.calls), sr.calls)
	}
}

func TestAgentMGitOpsAdapter_TranslatesNoChanges(t *testing.T) {
	sr := &scriptedRunner{out: []scriptedResult{
		{}, // checkout
		{}, // add
		{}, // status (empty stdout -> no changes)
	}}
	client := &gitops.Client{Runner: sr}
	adapter := NewAgentMGitOpsAdapter(client, gitops.Author{})

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	_, err := adapter.PublishArtifact(context.Background(), runtimepkg.AgentMPublishRequest{
		Repo:          "r/r",
		IssueNumber:   1,
		Branch:        "b",
		CommitMessage: "m",
		PRTitle:       "t",
		RepoLocalPath: dir,
	})
	if !errors.Is(err, runtimepkg.ErrNoChangesToPublish) {
		t.Fatalf("err = %v, want ErrNoChangesToPublish", err)
	}
}
