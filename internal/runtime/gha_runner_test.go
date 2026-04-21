package runtime

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	launcherevents "github.com/Lincyaw/workbuddy/internal/launcher/events"
	"github.com/Lincyaw/workbuddy/internal/launcher/runners/gha"
)

// fakeRunner lets a test script the response of each gh api call by
// matching on a substring of the joined args.
type fakeRunner struct {
	rules []fakeRule
}

type fakeRule struct {
	match string
	body  []byte
	err   error
}

func (f *fakeRunner) Run(_ context.Context, _ []byte, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	for _, r := range f.rules {
		if strings.Contains(joined, r.match) {
			return r.body, r.err
		}
	}
	return nil, fmt.Errorf("fakeRunner: no rule matched: %s", joined)
}

func newSession(t *testing.T, runner gha.CommandRunner) (*GHASession, chan launcherevents.Event) {
	t.Helper()
	tmp := t.TempDir()
	task := &TaskContext{
		Repo:     "owner/repo",
		RepoRoot: tmp,
		WorkDir:  tmp,
		Issue:    IssueContext{Number: 7},
		Session:  SessionContext{ID: "sess-1"},
	}
	agent := &config.AgentConfig{
		Name:    "dev-agent",
		Runtime: config.RunnerGitHubActions,
		GitHubActions: config.GitHubActionsRunnerConfig{
			Workflow:     "workbuddy-remote-runner.yml",
			Ref:          "main",
			PollInterval: time.Millisecond,
		},
		Timeout: time.Minute,
	}
	s := &GHASession{Agent: agent, Task: task, Client: gha.NewClientWithRunner(runner)}
	return s, make(chan launcherevents.Event, 64)
}

func drain(ch chan launcherevents.Event) {
	go func() {
		for range ch {
		}
	}()
}

func TestGHASession_DispatchFailureIsInfra(t *testing.T) {
	runner := &fakeRunner{rules: []fakeRule{
		{match: "workflows/workbuddy-remote-runner.yml/dispatches", err: errors.New("boom: 500")},
	}}
	s, ch := newSession(t, runner)
	drain(ch)
	result, err := s.Run(context.Background(), ch)
	if err == nil {
		t.Fatal("expected error")
	}
	if result == nil {
		t.Fatal("expected non-nil result with infra meta")
	}
	if !IsInfraFailure(result) {
		t.Fatalf("expected infra failure meta, got %#v", result.Meta)
	}
	if got := result.Meta[MetaInfraFailureReason]; got != "gha: workflow dispatch failed" {
		t.Fatalf("reason = %q, want gha: workflow dispatch failed", got)
	}
}

func TestGHASession_PollFailureIsInfra(t *testing.T) {
	runner := &fakeRunner{rules: []fakeRule{
		{match: "workflows/workbuddy-remote-runner.yml/dispatches", body: []byte(`{"id":101,"html_url":"u","url":"u"}`)},
		{match: "actions/runs/101", err: errors.New("transient 502")},
	}}
	s, ch := newSession(t, runner)
	drain(ch)
	result, err := s.Run(context.Background(), ch)
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsInfraFailure(result) {
		t.Fatalf("expected infra failure, got %#v", result.Meta)
	}
	if got := result.Meta[MetaInfraFailureReason]; got != "gha: workflow poll failed" {
		t.Fatalf("reason = %q", got)
	}
}

func TestGHASession_ArtifactDownloadFailureIsInfra(t *testing.T) {
	runner := &fakeRunner{rules: []fakeRule{
		{match: "workflows/workbuddy-remote-runner.yml/dispatches", body: []byte(`{"id":101,"html_url":"u","url":"u"}`)},
		{match: "actions/runs/101/logs", body: zipBytes(t, map[string]string{"1.txt": "hi\n"})},
		{match: "actions/runs/101/artifacts", err: errors.New("404: artifacts listing")},
		{match: "actions/runs/101", body: []byte(`{"id":101,"html_url":"u","status":"completed","conclusion":"success","head_branch":"main","created_at":"2026-04-16T00:00:05Z"}`)},
	}}
	s, ch := newSession(t, runner)
	drain(ch)
	result, err := s.Run(context.Background(), ch)
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsInfraFailure(result) {
		t.Fatalf("expected infra failure, got %#v", result.Meta)
	}
	if got := result.Meta[MetaInfraFailureReason]; got != "gha: artifact download failed" {
		t.Fatalf("reason = %q", got)
	}
}

func TestGHASession_ArtifactParseFailureIsInfra(t *testing.T) {
	// Happy path through dispatch/poll/logs/artifacts, but the
	// workbuddy-result.json payload is malformed so resultFromOutcome fails.
	artifact := zipBytes(t, map[string]string{
		"events-v1.jsonl":       "{\"kind\":\"turn.completed\"}\n",
		"workbuddy-result.json": `{not json`,
	})
	runner := &fakeRunner{rules: []fakeRule{
		{match: "workflows/workbuddy-remote-runner.yml/dispatches", body: []byte(`{"id":101,"html_url":"u","url":"u"}`)},
		{match: "actions/runs/101/logs", body: zipBytes(t, map[string]string{"1.txt": "hi\n"})},
		{match: "actions/runs/101/artifacts", body: []byte(`{"artifacts":[{"id":1,"name":"workbuddy-session","archive_download_url":"repos/owner/repo/actions/artifacts/1/zip"}]}`)},
		{match: "actions/artifacts/1/zip", body: artifact},
		{match: "actions/runs/101", body: []byte(`{"id":101,"html_url":"u","status":"completed","conclusion":"success","head_branch":"main","created_at":"2026-04-16T00:00:05Z"}`)},
	}}
	s, ch := newSession(t, runner)
	drain(ch)
	result, err := s.Run(context.Background(), ch)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if result == nil || !IsInfraFailure(result) {
		t.Fatalf("expected infra failure on artifact parse, got %#v", result)
	}
	if got := result.Meta[MetaInfraFailureReason]; got != "gha: artifact parse" {
		t.Fatalf("reason = %q", got)
	}
}

func TestGHASession_AgentNonZeroExitIsNotInfra(t *testing.T) {
	// Workflow ran to completion; the remote agent itself reported
	// exit_code=2. This must NOT be classified as infra failure.
	artifact := zipBytes(t, map[string]string{
		"events-v1.jsonl":       "{\"kind\":\"turn.completed\"}\n",
		"workbuddy-result.json": `{"exit_code":2,"last_message":"agent failed","session_path":"events-v1.jsonl"}`,
	})
	runner := &fakeRunner{rules: []fakeRule{
		{match: "workflows/workbuddy-remote-runner.yml/dispatches", body: []byte(`{"id":101,"html_url":"u","url":"u"}`)},
		{match: "actions/runs/101/logs", body: zipBytes(t, map[string]string{"1.txt": "hi\n"})},
		{match: "actions/runs/101/artifacts", body: []byte(`{"artifacts":[{"id":1,"name":"workbuddy-session","archive_download_url":"repos/owner/repo/actions/artifacts/1/zip"}]}`)},
		{match: "actions/artifacts/1/zip", body: artifact},
		{match: "actions/runs/101", body: []byte(`{"id":101,"html_url":"u","status":"completed","conclusion":"failure","head_branch":"main","created_at":"2026-04-16T00:00:05Z"}`)},
	}}
	s, ch := newSession(t, runner)
	drain(ch)
	result, err := s.Run(context.Background(), ch)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result == nil {
		t.Fatal("result nil")
	}
	if result.ExitCode != 2 {
		t.Fatalf("exit code = %d, want 2", result.ExitCode)
	}
	if IsInfraFailure(result) {
		t.Fatalf("agent-reported non-zero exit should not be infra failure: %#v", result.Meta)
	}
}

func TestGHAInfraReason_TimeoutAndCancelled(t *testing.T) {
	if got := ghaInfraReason(errors.New("ctx deadline"), "timeout"); got != "gha: workflow timed out" {
		t.Fatalf("timeout reason = %q", got)
	}
	if got := ghaInfraReason(errors.New("ctx cancel"), "cancelled"); got != "gha: run cancelled" {
		t.Fatalf("cancelled reason = %q", got)
	}
	if got := ghaInfraReason(errors.New("unknown"), "exec"); got != "gha: runtime failure" {
		t.Fatalf("fallback reason = %q", got)
	}
}

// zipBytes builds an in-memory zip archive with the given file map.
func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

