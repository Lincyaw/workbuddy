// Package agentm implements the agent.Backend interface for the AgentM
// pluggable agent SDK (../AgentM). v0.5 ships host-exec only: the worker
// spawns the `agentm` binary as a subprocess, feeds it task context via a
// JSON file, then parses a single stdout line beginning with `RESULT: {…}`
// for the structured outcome. The structured output is validated against
// schemas/agentm-output.schema.json; malformed/missing output is classified
// as an infra failure surfaced through the standard reporter path.
//
// See docs/planned/agentm-runtime.md for the invocation/output contract and
// docs/decisions/2026-05-13-k8s-agentm-otel.md (Block 1) for design context.
// Sandbox / coordinator-managed mode is v0.6.
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

	// Sidecar files: task input + result + session log. We let AgentM write
	// the result/session log inside a per-session temp dir so multiple
	// dispatches don't collide.
	tmpDir, err := os.MkdirTemp("", "workbuddy-agentm-"+id+"-")
	if err != nil {
		return nil, fmt.Errorf("agentm: create temp dir: %w", err)
	}
	taskFile := filepath.Join(tmpDir, "task.json")
	resultFile := filepath.Join(tmpDir, "result.json")
	sessionLog := filepath.Join(tmpDir, "session.jsonl")

	if err := writeTaskFile(taskFile, spec); err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("agentm: write task file: %w", err)
	}

	binary := b.Binary
	if binary == "" {
		binary = DefaultBinary
	}

	args := []string{
		"run",
		"--workspace", workspace,
		"--task-file", taskFile,
		"--session-log", sessionLog,
		"--result-file", resultFile,
	}
	args = append(args, spec.Args...)

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
	s := &session{
		id:         id,
		cmd:        cmd,
		events:     events,
		done:       make(chan struct{}),
		start:      time.Now(),
		tmpDir:     tmpDir,
		taskFile:   taskFile,
		resultFile: resultFile,
		sessionLog: sessionLog,
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

	// Stdout reader: extracts the RESULT: line and forwards every line as a
	// log event for transcript capture.
	go func() {
		defer close(events)
		defer close(s.done)
		// Intentionally do NOT remove tmpDir here: the session-log file is
		// referenced by Result.SessionPath / session_log_path and consumed by
		// the audit layer after Wait returns. Lifecycle cleanup is the
		// caller's responsibility (worker session manager).

		s.scanStdout(stdout)
		stderrWG.Wait()

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

		// Prefer the stdout RESULT: line. Fall back to the result file
		// (the contract permits both; #321's schema description names the
		// stdout RESULT: line as the canonical signal).
		out, parseErr := s.resolveOutput()

		s.mu.Lock()
		s.exitCode = exitCode
		s.duration = time.Since(s.start)
		s.waitErr = waitErr
		s.output = out
		s.parseErr = parseErr
		if out != nil {
			s.finalMsg = strings.TrimSpace(out.FailureReason)
			if out.Success && out.NextLabel != "" {
				s.finalMsg = out.NextLabel
			}
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
	taskFile   string
	resultFile string
	sessionLog string

	mu             sync.Mutex
	exitCode       int
	duration       time.Duration
	waitErr        error
	finalMsg       string
	sessionRef     agent.SessionRef
	output         *Output
	parseErr       error
	resultLineRaw  string
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
	// If the run produced no parseable structured output, surface that as
	// the wait error so the bridge can mark an infra failure. We keep the
	// process exit code separate so the caller can tell apart "agent
	// reported task failure" from "agent never reported anything".
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

func (s *session) Close() error { return nil }

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

func (s *session) scanStdout(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, resultLinePrefix) {
			s.mu.Lock()
			s.resultLineRaw = strings.TrimSpace(strings.TrimPrefix(trimmed, resultLinePrefix))
			s.mu.Unlock()
			body, _ := json.Marshal(map[string]string{"stream": "stdout", "line": line, "kind": "result"})
			emit(s.events, agent.Event{Kind: "task.complete", Body: body})
			continue
		}
		body, _ := json.Marshal(map[string]string{"stream": "stdout", "line": line})
		emit(s.events, agent.Event{Kind: "agent.message", Body: body})
	}
}

// resolveOutput returns the parsed structured output. The stdout RESULT: line
// is the canonical signal (per #321 schema description). If absent, we fall
// back to the --result-file written by AgentM. Returns (nil, err) when
// neither is parseable.
func (s *session) resolveOutput() (*Output, error) {
	s.mu.Lock()
	resultLine := s.resultLineRaw
	resultPath := s.resultFile
	sessionPath := s.sessionLog
	s.mu.Unlock()

	if resultLine != "" {
		out, err := ParseAndValidate([]byte(resultLine))
		if err != nil {
			return nil, fmt.Errorf("agentm: invalid RESULT: line: %w", err)
		}
		if out.SessionLogPath == "" && fileExists(sessionPath) {
			out.SessionLogPath = sessionPath
		}
		return out, nil
	}

	if data, err := os.ReadFile(resultPath); err == nil {
		out, perr := ParseAndValidate(data)
		if perr != nil {
			return nil, fmt.Errorf("agentm: invalid result file %s: %w", resultPath, perr)
		}
		if out.SessionLogPath == "" && fileExists(sessionPath) {
			out.SessionLogPath = sessionPath
		}
		return out, nil
	}

	return nil, fmt.Errorf("agentm: no RESULT: line on stdout and no result file at %s", resultPath)
}

// Output is the host representation of schemas/agentm-output.schema.json.
type Output struct {
	Success        bool   `json:"success"`
	NextLabel      string `json:"next_label"`
	ArtifactPath   string `json:"artifact_path,omitempty"`
	SessionLogPath string `json:"session_log_path,omitempty"`
	FailureReason  string `json:"failure_reason,omitempty"`
}

func emit(ch chan<- agent.Event, evt agent.Event) {
	defer func() { _ = recover() }()
	select {
	case ch <- evt:
	default:
		// Drop on slow consumer to avoid blocking the read goroutine.
	}
}

func writeTaskFile(path string, spec agent.Spec) error {
	doc := map[string]any{
		"schema_version": 1,
		"workspace":      spec.Workdir,
		"prompt":         spec.Prompt,
		"model":          spec.Model,
		"sandbox":        spec.Sandbox,
		"approval":       spec.Approval,
		"tags":           spec.Tags,
	}
	// Pull workbuddy-* env into structured fields so AgentM doesn't have
	// to read them from process env to populate spans/audit.
	if v := spec.Env["WORKBUDDY_ISSUE_NUMBER"]; v != "" {
		doc["issue_number"] = v
	}
	if v := spec.Env["WORKBUDDY_ISSUE_TITLE"]; v != "" {
		doc["issue_title"] = v
	}
	if v := spec.Env["WORKBUDDY_REPO"]; v != "" {
		doc["repo"] = v
	}
	if v := spec.Env["WORKBUDDY_SESSION_ID"]; v != "" {
		doc["session_id"] = v
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}
