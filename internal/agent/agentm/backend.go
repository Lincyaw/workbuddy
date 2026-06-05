// Package agentm implements the agent.Backend interface for the AgentM
// pluggable agent SDK (../AgentM). The worker spawns the real `agentm` CLI
// as a subprocess:
//
//	agentm "<prompt>" --scenario <name> [-e module[:json] ...] \
//	    [--cwd <workspace>] [--max-turns N]
//
// AgentM streams the agent's activity to stdout and prints a final summary
// line beginning with a `====` banner; the agent's final human-readable text
// appears as the last assistant block(s) before that banner. There is no
// `RESULT:` line in this CLI mode, so a clean exit-0 run is SUCCESS whose
// FinalMsg is that trailing assistant text. The backend captures the full
// stdout stream into a session-log file for audit.
//
// Backwards-compatible coordinator-managed mode: if a run DOES emit a
// `RESULT: {…}` line (validated against schemas/agentm-output.schema.json),
// the backend parses it into a structured Output so the bridge's GitOps /
// LabelWriter path (REQ-142 / REQ-146) keeps working. A malformed RESULT
// line is still an infra failure; the absence of any RESULT line is NOT.
//
// See docs/planned/agentm-runtime.md for the invocation/output contract and
// docs/decisions/2026-05-13-k8s-agentm-otel.md (Block 1) for design context.
package agentm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Lincyaw/workbuddy/internal/agent"
	"github.com/google/uuid"
)

// resultLinePrefix is the literal stdout marker AgentM uses to signal the
// structured outcome. Anything before this prefix is logged as conversation
// transcript; the rest of the matching line is parsed as JSON.
const resultLinePrefix = "RESULT:"

// DefaultBinary is the executable name workbuddy looks for on PATH when
// dispatching `runtime: agentm`. Mirrors the entry in
// internal/validate/semantics.go runtimeBinaries.
const DefaultBinary = "agentm"

// Backend is the workbuddy-level agent.Backend for AgentM. It spawns one
// subprocess per NewSession.
type Backend struct {
	// Binary overrides the executable name (default: "agentm"). Set this
	// from tests pointing at a fake.
	Binary string
}

// NewBackend returns a Backend wired to the production binary name. Tests
// construct Backend{Binary: …} directly.
func NewBackend() *Backend { return &Backend{Binary: DefaultBinary} }

func (b *Backend) NewSession(ctx context.Context, spec agent.Spec) (agent.Session, error) {
	id := uuid.New().String()

	workspace := spec.Workdir
	if workspace == "" {
		workspace = "."
	}

	// We capture the AgentM CLI's stdout transcript into a per-session
	// session-log file so audit always has something, since the CLI has no
	// --session-log flag in this invocation mode.
	tmpDir, err := os.MkdirTemp("", "workbuddy-agentm-"+id+"-")
	if err != nil {
		return nil, fmt.Errorf("agentm: create temp dir: %w", err)
	}
	sessionLog := filepath.Join(tmpDir, "session.log")

	binary := b.Binary
	if binary == "" {
		binary = DefaultBinary
	}

	args := buildArgs(spec, workspace)

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = workspace
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	cmd.Env = os.Environ()
	// Spec.Env already carries scoped GH_TOKEN, WORKBUDDY_*, and
	// TRACEPARENT/WORKBUDDY_RUN_ID set by the bridge.
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("agentm: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("agentm: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("agentm: start %s: %w", binary, err)
	}

	events := make(chan agent.Event, 64)
	logFile, err := os.OpenFile(sessionLog, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("agentm: open session log: %w", err)
	}
	s := &session{
		id:         id,
		cmd:        cmd,
		events:     events,
		done:       make(chan struct{}),
		start:      time.Now(),
		tmpDir:     tmpDir,
		sessionLog: sessionLog,
		logFile:    logFile,
		sessionRef: agent.SessionRef{ID: id, Kind: "agentm"},
	}

	// Stderr drained into log events so they surface in session audit.
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			body, _ := json.Marshal(map[string]string{"stream": "stderr", "line": line})
			emit(events, agent.Event{Kind: "log", Body: body})
		}
	}()

	// Stdout reader: streams every line into the session-log file (audit
	// transcript), tracks the trailing assistant text for FinalMsg, and
	// captures any RESULT: line for the coordinator-managed path.
	go func() {
		defer close(events)
		// tmpDir holds the session-log file, which must outlive the process
		// exit because Wait() / SessionLogPath() read it after the process
		// returns. The bridge (internal/runtime/agent_bridge.go) copies it
		// into the durable worker session dir, then the worker calls Close()
		// once the run is finalized — Close() removes tmpDir. If Close()
		// raced ahead of process exit, the cleanup is deferred to here via
		// removeTmpDir, which is a no-op until both done is closed and
		// Close() ran.
		defer func() {
			close(s.done)
			s.removeTmpDir()
		}()

		s.scanStdout(stdout)
		stderrWG.Wait()
		if s.logFile != nil {
			_ = s.logFile.Close()
		}

		waitErr := cmd.Wait()
		exitCode := 0
		if waitErr != nil {
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = -1
			}
		}

		out, parseErr := s.resolveOutput()

		s.mu.Lock()
		s.exitCode = exitCode
		s.duration = time.Since(s.start)
		s.waitErr = waitErr
		s.output = out
		s.parseErr = parseErr
		switch {
		case out != nil:
			// Structured RESULT: line present (coordinator-managed path).
			s.finalMsg = strings.TrimSpace(out.FailureReason)
			if out.Success && out.NextLabel != "" {
				s.finalMsg = out.NextLabel
			}
		default:
			// CLI mode: FinalMsg is the agent's trailing assistant text
			// captured before the `====` summary banner.
			s.finalMsg = strings.TrimSpace(s.finalText.String())
		}
		s.mu.Unlock()
	}()

	return s, nil
}

func (b *Backend) Shutdown(_ context.Context) error { return nil }

type session struct {
	id     string
	cmd    *exec.Cmd
	events chan agent.Event
	done   chan struct{}
	start  time.Time

	tmpDir     string
	sessionLog string
	logFile    *os.File

	mu            sync.Mutex
	exitCode      int
	duration      time.Duration
	waitErr       error
	finalMsg      string
	sessionRef    agent.SessionRef
	output        *Output
	parseErr      error
	resultLineRaw string
	// finalText accumulates the trailing human-readable assistant text from
	// the CLI stdout stream — everything printed before the `====` summary
	// banner. Guarded by mu (written from scanStdout, read at finalize).
	finalText strings.Builder

	// closeRequested is set by Close(); tmpDirGone makes removal idempotent.
	// Both are guarded by mu. tmpDir is removed only once both the process
	// has exited (done closed) and Close() has been called, whichever order
	// they happen in — see removeTmpDir.
	closeRequested bool
	tmpDirGone     bool
}

func (s *session) ID() string                 { return s.id }
func (s *session) Events() <-chan agent.Event { return s.events }

func (s *session) Wait(ctx context.Context) (agent.Result, error) {
	select {
	case <-s.done:
	case <-ctx.Done():
		return agent.Result{}, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res := agent.Result{
		ExitCode:   s.exitCode,
		FinalMsg:   s.finalMsg,
		Duration:   s.duration,
		SessionRef: s.sessionRef,
	}
	// A malformed RESULT: line (parseErr set) is surfaced as the wait error
	// so the bridge marks an infra failure. The ABSENCE of a RESULT line is
	// NOT an error — CLI-mode runs report their outcome purely via exit code
	// and the trailing assistant text (FinalMsg). A clean RESULT line with
	// success=false is surfaced as a task failure.
	if s.parseErr != nil {
		return res, s.parseErr
	}
	if s.output != nil && !s.output.Success {
		return res, fmt.Errorf("agentm: task failure: %s", s.output.FailureReason)
	}
	return res, s.waitErr
}

func (s *session) Interrupt(ctx context.Context) error {
	if s.cmd.Process == nil {
		return nil
	}
	pgid := -s.cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-s.done:
		return nil
	case <-timer.C:
		_ = syscall.Kill(pgid, syscall.SIGKILL)
	case <-ctx.Done():
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		return ctx.Err()
	}
	select {
	case <-s.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// Close marks the session as finalized and removes the per-session temp dir
// (task.json/result.json/session.jsonl). The worker calls this after audit
// has persisted the run, so the durable session artifacts are already copied
// out of tmpDir by the bridge. Close is safe to call multiple times and from
// either the running or completed state: if the process is still running,
// removal is deferred until the stdout-reader goroutine exits.
func (s *session) Close() error {
	s.mu.Lock()
	s.closeRequested = true
	s.mu.Unlock()
	s.removeTmpDir()
	return nil
}

// removeTmpDir deletes tmpDir exactly once, but only after both the process
// has exited (s.done closed) and Close() has been requested. It is called
// from both Close() and the stdout-reader goroutine's deferred cleanup so
// that whichever happens last performs the removal — no leak regardless of
// ordering.
func (s *session) removeTmpDir() {
	select {
	case <-s.done:
	default:
		return // process still running; nothing to clean yet
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tmpDirGone || !s.closeRequested || s.tmpDir == "" {
		return
	}
	_ = os.RemoveAll(s.tmpDir)
	s.tmpDirGone = true
}

// Output exposes the parsed structured output for callers that need to
// route on next_label / failure_reason / artifact_path. Returns (nil, err)
// if the run produced no parseable RESULT: line. Safe to call after Wait
// returns.
func (s *session) Output() (*Output, error) {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.parseErr != nil {
		return nil, s.parseErr
	}
	return s.output, nil
}

// SessionLogPath returns the path to the session JSONL the run wrote (or
// would have written, in the fake-binary case). Returns "" if no session
// log was emitted.
func (s *session) SessionLogPath() string {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.output != nil && s.output.SessionLogPath != "" {
		return s.output.SessionLogPath
	}
	if fileExists(s.sessionLog) {
		return s.sessionLog
	}
	return ""
}

// summaryBanner is the `"="*60` line AgentM's _print_final emits before the
// `messages=…`/`tokens:…` summary. Everything after it is run accounting, not
// agent output, so it terminates final-text accumulation.
const summaryBanner = "============================================================"

func (s *session) scanStdout(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	// para accumulates the current run of human-readable assistant lines.
	// On a blank line we flush para into lastPara (the most recent finished
	// paragraph). FinalMsg is the last paragraph emitted before the summary
	// banner. Lines after the banner (summary/session_id) are ignored for
	// final-text purposes but still written to the transcript.
	var para []string
	var lastPara string
	inSummary := false

	flush := func() {
		if len(para) > 0 {
			lastPara = strings.Join(para, "\n")
			para = para[:0]
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		// Persist the raw line to the session-log transcript regardless of
		// how it is classified below.
		if s.logFile != nil {
			_, _ = s.logFile.WriteString(line + "\n")
		}
		trimmed := strings.TrimSpace(line)

		// Backwards-compatible coordinator-managed signal: a RESULT: line
		// short-circuits the CLI-text path entirely.
		if strings.HasPrefix(trimmed, resultLinePrefix) {
			s.mu.Lock()
			s.resultLineRaw = strings.TrimSpace(strings.TrimPrefix(trimmed, resultLinePrefix))
			s.mu.Unlock()
			body, _ := json.Marshal(map[string]string{"stream": "stdout", "line": line, "kind": "result"})
			emit(s.events, agent.Event{Kind: "task.complete", Body: body})
			continue
		}

		if trimmed == summaryBanner {
			inSummary = true
			flush()
		}

		if !inSummary {
			switch {
			case trimmed == "":
				flush()
			case isTranscriptDecoration(trimmed):
				// Tool calls (→ …), tool results (⇐ …), injected messages,
				// and AgentM's trailing `session_id=…  (resume with: …)` are
				// not agent prose; they break the current paragraph but are
				// not themselves final text.
				flush()
			default:
				para = append(para, line)
			}
		}

		body, _ := json.Marshal(map[string]string{"stream": "stdout", "line": line})
		emit(s.events, agent.Event{Kind: "agent.message", Body: body})
	}
	flush()

	s.mu.Lock()
	s.finalText.WriteString(lastPara)
	s.mu.Unlock()
}

// isTranscriptDecoration reports whether a stdout line is AgentM streaming
// chrome (tool calls / tool results / injected messages / the resume hint)
// rather than agent prose. These break a paragraph but never become FinalMsg.
func isTranscriptDecoration(trimmed string) bool {
	return strings.HasPrefix(trimmed, "→ ") ||
		strings.HasPrefix(trimmed, "⇐") ||
		strings.HasPrefix(trimmed, "[injected") ||
		strings.HasPrefix(trimmed, "session_id=")
}

// resolveOutput returns the parsed structured output from a RESULT: line if
// one was emitted (coordinator-managed mode). A malformed RESULT: line is a
// (nil, err) infra failure. The ABSENCE of a RESULT: line is the normal CLI
// case: returns (nil, nil) so the run is treated as success carrying its
// trailing assistant text as FinalMsg.
func (s *session) resolveOutput() (*Output, error) {
	s.mu.Lock()
	resultLine := s.resultLineRaw
	sessionPath := s.sessionLog
	s.mu.Unlock()

	if resultLine != "" {
		// Parse first WITHOUT the conditional schema checks: session_log_path
		// is a host-side artifact path the sandboxed agent cannot know, so the
		// backend fills it from the run's own session log before validating
		// the completed object against the full contract.
		out, err := ParseResult([]byte(resultLine))
		if err != nil {
			return nil, fmt.Errorf("agentm: invalid RESULT: line: %w", err)
		}
		if out.SessionLogPath == "" && fileExists(sessionPath) {
			out.SessionLogPath = sessionPath
		}
		if err := ValidateOutput(out); err != nil {
			return nil, fmt.Errorf("agentm: invalid RESULT: line: %w", err)
		}
		return out, nil
	}

	return nil, nil
}

// Output is the host representation of schemas/agentm-output.schema.json.
type Output struct {
	Success        bool   `json:"success"`
	NextLabel      string `json:"next_label"`
	ArtifactPath   string `json:"artifact_path,omitempty"`
	SessionLogPath string `json:"session_log_path,omitempty"`
	FailureReason  string `json:"failure_reason,omitempty"`
}

// emit is lossy by design: if the buffered events channel is full (a slow or
// stalled consumer) the event is dropped rather than blocking the stdout/
// stderr reader goroutines. Blocking here would risk deadlocking the reader
// against a wedged consumer and prevent the process from being reaped.
//
// Routing is never affected by a drop: the canonical RESULT: line is captured
// into s.resultLineRaw inside scanStdout BEFORE emit runs, and the trailing
// assistant text is accumulated into s.finalText there too. Only the
// transcript rendering on the secondary events stream can lose a line. The
// full stdout transcript is written to the session-log file independent of
// this channel, so the conversation is preserved for audit either way.
func emit(ch chan<- agent.Event, evt agent.Event) {
	defer func() { _ = recover() }()
	select {
	case ch <- evt:
	default:
		// Drop on slow consumer — see doc comment above.
	}
}

// buildArgs assembles the real AgentM CLI argv:
//
//	--prompt "<prompt>" --scenario <name> [-e module[:json] ...] \
//	    [--model M] [--cwd <workspace>] [--max-turns N]
//
// The prompt is passed via --prompt (agentm no longer accepts a bare
// positional argument). --scenario is emitted only when the spec names one
// (otherwise AgentM picks its own default). Each extension becomes a
// `-e module` flag, or `-e module:<json>` when it carries config.
// The system prompt is just another extension — no special-casing here.
func buildArgs(spec agent.Spec, workspace string) []string {
	var args []string
	if spec.ResumeSessionID != "" {
		args = append(args, "--resume", spec.ResumeSessionID)
		if spec.Prompt != "" {
			args = append(args, "--prompt", spec.Prompt)
		}
	} else {
		args = append(args, "--prompt", spec.Prompt)
	}
	if scenario := strings.TrimSpace(spec.Scenario); scenario != "" {
		args = append(args, "--scenario", scenario)
	}
	for _, ext := range spec.Extensions {
		module := strings.TrimSpace(ext.Module)
		if module == "" {
			continue
		}
		if len(ext.Config) > 0 {
			if cfg, err := json.Marshal(ext.Config); err == nil {
				args = append(args, "-e", module+":"+string(cfg))
				continue
			}
		}
		args = append(args, "-e", module)
	}
	if model := strings.TrimSpace(spec.Model); model != "" {
		args = append(args, "--model", model)
	}
	if workspace != "" && workspace != "." {
		args = append(args, "--cwd", workspace)
	}
	if spec.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(spec.MaxTurns))
	}
	return args
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}
