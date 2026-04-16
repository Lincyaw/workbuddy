package gha

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClientRunWorkflow_UsesDispatchRunIDAndIngestsArtifacts(t *testing.T) {
	tmp := t.TempDir()
	fixtures := filepath.Join(tmp, "fixtures")
	if err := os.MkdirAll(fixtures, 0o755); err != nil {
		t.Fatal(err)
	}

	writeFixture(t, filepath.Join(fixtures, "dispatch.json"), `{"id":101,"html_url":"https://github.com/owner/repo/actions/runs/101","url":"https://api.github.com/repos/owner/repo/actions/runs/101"}`)
	writeFixture(t, filepath.Join(fixtures, "run-queued.json"), `{"id":101,"html_url":"https://github.com/owner/repo/actions/runs/101","status":"in_progress","conclusion":"","head_branch":"main","created_at":"2026-04-16T00:00:05Z"}`)
	writeFixture(t, filepath.Join(fixtures, "run-completed.json"), `{"id":101,"html_url":"https://github.com/owner/repo/actions/runs/101","status":"completed","conclusion":"success","head_branch":"main","created_at":"2026-04-16T00:00:05Z"}`)
	writeFixture(t, filepath.Join(fixtures, "artifacts.json"), `{"artifacts":[{"id":9001,"name":"workbuddy-session","archive_download_url":"repos/owner/repo/actions/artifacts/9001/zip"}]}`)
	writeFixture(t, filepath.Join(fixtures, "logs.zip"), zipFixture(t, map[string]string{
		"build/1_setup.txt": "setup\n",
		"build/2_agent.txt": "remote runner output\n",
	}))
	writeFixture(t, filepath.Join(fixtures, "artifact.zip"), zipFixture(t, map[string]string{
		"events-v1.jsonl":       "{\"kind\":\"turn.completed\"}\n",
		"codex-exec.jsonl":      "{\"type\":\"message\"}\n",
		"workbuddy-result.json": `{"exit_code":0,"last_message":"remote success","meta":{"pr_url":"https://github.com/owner/repo/pull/47"},"session_path":"events-v1.jsonl"}`,
		"nested/ignored.txt":    "hello\n",
	}))

	calls := filepath.Join(tmp, "gh-calls.txt")
	detailCount := filepath.Join(tmp, "detail-count.txt")
	ghPath := filepath.Join(tmp, "gh")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$GH_CALLS"
case "$*" in
  *"actions/workflows/workbuddy-remote-runner.yml/dispatches"*)
    cat "$GH_FIXTURES/dispatch.json"
    ;;
  *"actions/workflows/workbuddy-remote-runner.yml/runs?event=workflow_dispatch"*)
    echo "run-list lookup should not be used when dispatch returned a run id" >&2
    exit 1
    ;;
  *"actions/runs/101/logs"*)
    cat "$GH_FIXTURES/logs.zip"
    ;;
  *"actions/runs/101/artifacts"*)
    cat "$GH_FIXTURES/artifacts.json"
    ;;
  *"actions/artifacts/9001/zip"*)
    cat "$GH_FIXTURES/artifact.zip"
    ;;
  *"actions/runs/101"*)
    n=0
    if [ -f "$GH_DETAIL_COUNT" ]; then
      n=$(cat "$GH_DETAIL_COUNT")
    fi
    n=$((n+1))
    printf '%s' "$n" > "$GH_DETAIL_COUNT"
    if [ "$n" -eq 1 ]; then
      cat "$GH_FIXTURES/run-queued.json"
    else
      cat "$GH_FIXTURES/run-completed.json"
    fi
    ;;
  *"actions/runs/202"*)
    echo "wrong run polled" >&2
    exit 1
    ;;
  *)
    echo "unexpected gh args: $*" >&2
    exit 1
    ;;
esac
`
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_FIXTURES", fixtures)
	t.Setenv("GH_CALLS", calls)
	t.Setenv("GH_DETAIL_COUNT", detailCount)

	client := NewClient()
	client.now = func() time.Time { return time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC) }
	client.sleep = func(time.Duration) {}

	outcome, err := client.RunWorkflow(context.Background(), Config{
		Repo:         "owner/repo",
		IssueNumber:  47,
		AgentName:    "dev-agent",
		SessionID:    "session-47",
		Workflow:     "workbuddy-remote-runner.yml",
		Ref:          "main",
		PollInterval: time.Millisecond,
		ArtifactDir:  filepath.Join(tmp, "out"),
	})
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}
	if outcome.Run.ID != 101 {
		t.Fatalf("run id = %d", outcome.Run.ID)
	}
	if outcome.Run.Conclusion != "success" {
		t.Fatalf("conclusion = %q", outcome.Run.Conclusion)
	}
	if !strings.Contains(outcome.Logs, "remote runner output") {
		t.Fatalf("logs = %q", outcome.Logs)
	}
	if filepath.Base(outcome.CanonicalSessionPath) != "events-v1.jsonl" {
		t.Fatalf("session path = %q", outcome.CanonicalSessionPath)
	}
	if filepath.Base(outcome.ResultPath) != "workbuddy-result.json" {
		t.Fatalf("result path = %q", outcome.ResultPath)
	}
	if _, err := os.Stat(outcome.LogPath); err != nil {
		t.Fatalf("stat log path: %v", err)
	}
	if _, err := os.Stat(outcome.CanonicalSessionPath); err != nil {
		t.Fatalf("stat session path: %v", err)
	}

	callLog, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	joined := string(callLog)
	for _, want := range []string{
		"actions/workflows/workbuddy-remote-runner.yml/dispatches",
		"return_run_details=true",
		"inputs[session_id]=session-47",
		"actions/runs/101",
		"actions/runs/101/logs",
		"actions/runs/101/artifacts",
		"actions/artifacts/9001/zip",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("call log missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "/runs?event=workflow_dispatch") {
		t.Fatalf("call log should not include workflow run list lookup:\n%s", joined)
	}
}

func TestClientRunWorkflow_FailsWhenSessionArtifactMissing(t *testing.T) {
	tmp := t.TempDir()
	fixtures := filepath.Join(tmp, "fixtures")
	if err := os.MkdirAll(fixtures, 0o755); err != nil {
		t.Fatal(err)
	}

	writeFixture(t, filepath.Join(fixtures, "dispatch.json"), `{"id":404,"html_url":"https://github.com/owner/repo/actions/runs/404","url":"https://api.github.com/repos/owner/repo/actions/runs/404"}`)
	writeFixture(t, filepath.Join(fixtures, "run-completed.json"), `{"id":404,"html_url":"https://github.com/owner/repo/actions/runs/404","status":"completed","conclusion":"success","head_branch":"main","created_at":"2026-04-16T00:00:05Z"}`)
	writeFixture(t, filepath.Join(fixtures, "artifacts.json"), `{"artifacts":[{"id":9002,"name":"workbuddy-session","archive_download_url":"repos/owner/repo/actions/artifacts/9002/zip"}]}`)
	writeFixture(t, filepath.Join(fixtures, "logs.zip"), zipFixture(t, map[string]string{
		"build/agent.txt": "remote runner output\n",
	}))
	writeFixture(t, filepath.Join(fixtures, "artifact.zip"), zipFixture(t, map[string]string{
		"workbuddy-result.json": `{"exit_code":0,"last_message":"remote success"}`,
		"notes.txt":             "artifact uploaded without session capture\n",
	}))

	calls := filepath.Join(tmp, "gh-calls.txt")
	ghPath := filepath.Join(tmp, "gh")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$GH_CALLS"
case "$*" in
  *"actions/workflows/workbuddy-remote-runner.yml/dispatches"*)
    cat "$GH_FIXTURES/dispatch.json"
    ;;
  *"actions/runs/404/logs"*)
    cat "$GH_FIXTURES/logs.zip"
    ;;
  *"actions/runs/404/artifacts"*)
    cat "$GH_FIXTURES/artifacts.json"
    ;;
  *"actions/artifacts/9002/zip"*)
    cat "$GH_FIXTURES/artifact.zip"
    ;;
  *"actions/runs/404"*)
    cat "$GH_FIXTURES/run-completed.json"
    ;;
  *)
    echo "unexpected gh args: $*" >&2
    exit 1
    ;;
esac
`
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_FIXTURES", fixtures)
	t.Setenv("GH_CALLS", calls)

	client := NewClient()
	client.now = func() time.Time { return time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC) }
	client.sleep = func(time.Duration) {}

	_, err := client.RunWorkflow(context.Background(), Config{
		Repo:         "owner/repo",
		IssueNumber:  47,
		AgentName:    "dev-agent",
		SessionID:    "session-47",
		Workflow:     "workbuddy-remote-runner.yml",
		Ref:          "main",
		PollInterval: time.Millisecond,
		ArtifactDir:  filepath.Join(tmp, "out"),
	})
	if err == nil {
		t.Fatal("expected missing session artifact error")
	}
	if !strings.Contains(err.Error(), "missing session capture") {
		t.Fatalf("error = %v", err)
	}
}

func writeFixture(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func zipFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}
