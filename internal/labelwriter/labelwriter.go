// Package labelwriter applies issue label changes from the coordinator
// process when an agent runs in coordinator-managed mode (runtime=agentm).
//
// CLAUDE.md forbids Go-side label writes as a general rule — agents drive
// the workbuddy state machine by calling `gh issue edit` themselves. This
// package is the single, narrow exception sanctioned by docs/decisions/
// 2026-05-13-k8s-agentm-otel.md Block 2 § Two execution modes: when the
// agent runs inside a sandbox (agent-env) with no credentials, the
// coordinator owns the GH/Gitea write surface and must apply the label
// transition the agent suggests in its structured Result.
//
// This package is intentionally dormant until the dispatch path wires it
// (tracked in #332). It compiles and is unit-tested, but no production
// code path imports ApplyNextLabel yet.
package labelwriter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"go.opentelemetry.io/otel/attribute"

	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/Lincyaw/workbuddy/internal/tracing"
)

// HostKindGitHub identifies repos that talk to api.github.com via the gh CLI.
const HostKindGitHub = "github"

// HostKindGitea identifies repos hosted on a Gitea instance, addressed via
// the Gitea REST API and a token in the GITEA_TOKEN env var.
const HostKindGitea = "gitea"

// registrationLookup loads the coordinator-side registration record for repo.
// Defined narrowly so tests can supply a fake without pulling in store.Store.
type registrationLookup func(repo string) (*store.RepoRegistrationRecord, error)

// commandRunner runs an external command (the gh CLI by default) and returns
// its combined output. Tests swap this for an in-memory fake.
type commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// httpDoer is the minimal interface ApplyNextLabel needs from net/http.Client
// so tests can intercept Gitea API calls.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Writer applies label changes for coordinator-managed agent runs.
//
// All collaborators (registration lookup, gh CLI runner, HTTP client used
// for Gitea) are indirected so that tests can assert exact wire args
// without touching the real GitHub / Gitea API.
type Writer struct {
	lookup   registrationLookup
	lookPath func(string) (string, error)
	run      commandRunner
	http     httpDoer
	// giteaToken, if non-empty, overrides os.Getenv("GITEA_TOKEN").
	// Tests use this so they don't touch process env.
	giteaToken string
}

// New constructs a Writer wired to the real store and exec.LookPath / exec.Command.
func New(s store.Store) *Writer {
	w := &Writer{
		lookup:   func(repo string) (*store.RepoRegistrationRecord, error) { return s.GetRepoRegistration(repo) },
		lookPath: exec.LookPath,
		http:     http.DefaultClient,
	}
	w.run = defaultRun
	return w
}

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

// ApplyNextLabel adds `label` to issue #issueNum in `repo`. It is a no-op
// when label is empty so callers don't have to pre-check the agent's
// Result.Meta map.
//
// The repo's registration is consulted to pick the wire protocol:
//
//   - host_kind=github (or absent): shells out to `gh issue edit <issueNum>
//     --repo <repo> --add-label <label>`. gh resolves auth from the
//     coordinator process's gh login.
//   - host_kind=gitea: POSTs to <gitea_base_url>/api/v1/repos/<repo>/issues/
//     <issueNum>/labels with the GITEA_TOKEN bearer.
//
// The single OTel span emitted by this function carries
// wb.label_change.source="coordinator-managed" so dashboards can separate
// coordinator-applied labels from agent-applied ones (which currently emit
// no such span — they happen inside the agent subprocess).
func (w *Writer) ApplyNextLabel(ctx context.Context, repo string, issueNum int, label string) error {
	if strings.TrimSpace(label) == "" {
		return nil
	}
	if w == nil {
		return errors.New("labelwriter: nil Writer")
	}

	ctx, span := tracing.Start(ctx, "labelwriter.apply_next_label",
		attribute.String("wb.label_change.source", "coordinator-managed"),
		attribute.String("workbuddy.repo", repo),
		attribute.Int("workbuddy.issue.number", issueNum),
		attribute.String("workbuddy.label", label),
	)
	defer span.End()

	kind, giteaBase, err := w.resolveHostKind(repo)
	if err != nil {
		return fmt.Errorf("labelwriter: resolve host kind for %s: %w", repo, err)
	}
	span.SetAttributes(attribute.String("wb.label_change.host_kind", kind))

	switch kind {
	case HostKindGitHub:
		return w.applyViaGH(ctx, repo, issueNum, label)
	case HostKindGitea:
		return w.applyViaGitea(ctx, giteaBase, repo, issueNum, label)
	default:
		return fmt.Errorf("labelwriter: unsupported host_kind %q for repo %s", kind, repo)
	}
}

// resolveHostKind reads the repo's registration ConfigJSON for a host_kind
// field. Missing / unregistered repos default to GitHub so that today's
// only-GitHub deployments don't have to migrate their registrations.
func (w *Writer) resolveHostKind(repo string) (kind, giteaBase string, err error) {
	if w.lookup == nil {
		return HostKindGitHub, "", nil
	}
	rec, err := w.lookup(repo)
	if err != nil {
		return "", "", err
	}
	// rec == nil signals "no registration row" (store.GetRepoRegistration
	// returns nil, nil for sql.ErrNoRows). Coordinator-managed runs on
	// unregistered repos are allowed at bootstrap time — default to
	// GitHub so existing single-host deployments work.
	if rec == nil || strings.TrimSpace(rec.ConfigJSON) == "" {
		return HostKindGitHub, "", nil
	}
	var cfg struct {
		HostKind     string `json:"host_kind"`
		GiteaBaseURL string `json:"gitea_base_url"`
	}
	if err := json.Unmarshal([]byte(rec.ConfigJSON), &cfg); err != nil {
		// Tolerate malformed ConfigJSON: fall back to GitHub.
		return HostKindGitHub, "", nil
	}
	kind = strings.ToLower(strings.TrimSpace(cfg.HostKind))
	if kind == "" {
		kind = HostKindGitHub
	}
	return kind, strings.TrimRight(cfg.GiteaBaseURL, "/"), nil
}

func (w *Writer) applyViaGH(ctx context.Context, repo string, issueNum int, label string) error {
	lookPath := w.lookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	bin, err := lookPath("gh")
	if err != nil {
		return fmt.Errorf("labelwriter: gh CLI not found on PATH: %w", err)
	}
	run := w.run
	if run == nil {
		run = defaultRun
	}
	args := []string{
		"issue", "edit", fmt.Sprintf("%d", issueNum),
		"--repo", repo,
		"--add-label", label,
	}
	out, err := run(ctx, bin, args...)
	if err != nil {
		return fmt.Errorf("labelwriter: gh issue edit %d on %s: %w (output: %s)", issueNum, repo, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (w *Writer) applyViaGitea(ctx context.Context, baseURL, repo string, issueNum int, label string) error {
	if baseURL == "" {
		return fmt.Errorf("labelwriter: gitea_base_url missing for repo %s", repo)
	}
	token := w.giteaToken
	if token == "" {
		token = os.Getenv("GITEA_TOKEN")
	}
	if token == "" {
		return fmt.Errorf("labelwriter: GITEA_TOKEN not set for gitea repo %s", repo)
	}
	url := fmt.Sprintf("%s/api/v1/repos/%s/issues/%d/labels", baseURL, repo, issueNum)
	body, _ := json.Marshal(map[string]any{"labels": []string{label}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("labelwriter: build gitea request: %w", err)
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := w.http
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("labelwriter: gitea POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("labelwriter: gitea POST %s: status %d body=%s", url, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}
