package supervisor

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

const (
	// DefaultCancelGrace is the time SIGTERM gets to take effect before SIGKILL.
	DefaultCancelGrace = 10 * time.Second
)

// Config configures a Supervisor.
type Config struct {
	// SocketPath is the unix socket to bind. If empty, DefaultSocketPath is used.
	SocketPath string
	// StateDir is the on-disk root holding agent stdout/stderr (under
	// agents/) and the supervisor sqlite DB. Defaults to
	// $XDG_STATE_HOME/workbuddy or ~/.local/state/workbuddy.
	StateDir string
	// CancelGrace overrides DefaultCancelGrace.
	CancelGrace time.Duration
}

// DefaultSocketPath returns unix:/run/user/$UID/workbuddy-supervisor.sock,
// falling back to $TMPDIR/workbuddy-supervisor-$UID.sock when XDG_RUNTIME_DIR
// is unavailable.
func DefaultSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "workbuddy-supervisor.sock")
	}
	uid := os.Getuid()
	return filepath.Join(os.TempDir(), fmt.Sprintf("workbuddy-supervisor-%d.sock", uid))
}

// DefaultStateDir returns $XDG_STATE_HOME/workbuddy, falling back to
// ~/.local/state/workbuddy.
func DefaultStateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "workbuddy"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "workbuddy"), nil
}

// Supervisor manages agent subprocesses and serves the IPC HTTP API.
type Supervisor struct {
	cfg       Config
	agentsDir string
	dbPath    string
	db        *sql.DB

	mu     sync.RWMutex
	agents map[string]*Agent

	// execCommand allows tests to substitute exec.Command.
	execCommand func(name string, args ...string) *exec.Cmd
}

// New constructs a Supervisor and prepares its on-disk state. It does not
// start the HTTP server; call Serve for that.
func New(cfg Config) (*Supervisor, error) {
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSocketPath()
	}
	if cfg.StateDir == "" {
		dir, err := DefaultStateDir()
		if err != nil {
			return nil, err
		}
		cfg.StateDir = dir
	}
	if cfg.CancelGrace == 0 {
		cfg.CancelGrace = DefaultCancelGrace
	}
	agentsDir := filepath.Join(cfg.StateDir, "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir agents dir: %w", err)
	}
	dbPath := filepath.Join(cfg.StateDir, "supervisor.db")
	db, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}
	s := &Supervisor{
		cfg:         cfg,
		agentsDir:   agentsDir,
		dbPath:      dbPath,
		db:          db,
		agents:      make(map[string]*Agent),
		execCommand: exec.Command,
	}
	if err := s.recoverFromDB(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases supervisor resources but does NOT terminate live agents:
// the whole point of the supervisor is that its lifetime is independent of
// the agent subprocesses.
func (s *Supervisor) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// SocketPath returns the configured socket path.
func (s *Supervisor) SocketPath() string { return s.cfg.SocketPath }

// AgentsDir returns the directory holding per-agent stdout/stderr files.
func (s *Supervisor) AgentsDir() string { return s.agentsDir }

// recoverFromDB rebuilds the in-memory agent map from SQLite, validating that
// any "running" agent's pid still belongs to the same process by comparing
// /proc/<pid>/stat starttime ticks. Stale rows are flipped to status=exited.
func (s *Supervisor) recoverFromDB() error {
	rows, err := loadAgents(s.db)
	if err != nil {
		return fmt.Errorf("load agents: %w", err)
	}
	for _, r := range rows {
		a := &Agent{
			ID:         r.AgentID,
			Runtime:    r.Runtime,
			Workdir:    r.Workdir,
			SessionID:  r.SessionID,
			StartedAt:  r.StartedAt,
			StdoutPath: r.StdoutPath,
			StderrPath: r.StderrPath,
			pid:        r.PID,
			startTicks: r.StartTicks,
			recovered:  true,
		}
		if r.Status == "exited" {
			a.exited = true
			if r.ExitCode != nil {
				a.exitCode = *r.ExitCode
			}
		} else if !s.pidIsAlive(r.PID, r.StartTicks) {
			// Process is gone (or pid was reused by a different process).
			// Flip the row to exited with code -1 to mean "unknown".
			a.exited = true
			a.exitCode = -1
			_ = updateAgentExit(s.db, r.AgentID, -1)
		} else {
			// Still running, but we don't own the cmd — set up a watcher.
			a.doneCh = make(chan struct{})
			go s.watchRecovered(a)
		}
		// Seed the line counter from the persisted stdout file so callers
		// can pick up `from_offset` past existing content.
		a.stdoutLines.Store(countFileLines(a.StdoutPath))
		s.agents[a.ID] = a
	}
	return nil
}

func (s *Supervisor) pidIsAlive(pid int, wantTicks uint64) bool {
	got, err := readProcStarttime(pid)
	if err != nil {
		return false
	}
	return got == wantTicks
}

// watchRecovered polls /proc for a recovered agent until it exits.
func (s *Supervisor) watchRecovered(a *Agent) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		if !s.pidIsAlive(a.pid, a.startTicks) {
			a.markExited(-1)
			_ = updateAgentExit(s.db, a.ID, -1)
			return
		}
	}
}

func countFileLines(path string) int64 {
	if path == "" {
		return 0
	}
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	var n int64
	buf := make([]byte, 32*1024)
	for {
		c, err := f.Read(buf)
		for i := 0; i < c; i++ {
			if buf[i] == '\n' {
				n++
			}
		}
		if err != nil {
			break
		}
	}
	return n
}

// StartAgentRequest is the body of POST /agents.
type StartAgentRequest struct {
	Runtime   string            `json:"runtime"`
	Args      []string          `json:"args"`
	Workdir   string            `json:"workdir,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	SessionID string            `json:"session_id,omitempty"`
}

// StartAgentResponse is returned by POST /agents.
type StartAgentResponse struct {
	AgentID   string    `json:"agent_id"`
	StartedAt time.Time `json:"started_at"`
}

// AgentStatus is the response shape for GET /agents/:id.
type AgentStatus struct {
	AgentID   string    `json:"agent_id"`
	Status    string    `json:"status"`
	ExitCode  *int      `json:"exit_code,omitempty"`
	SessionID string    `json:"session_id"`
	PID       int       `json:"pid"`
	Runtime   string    `json:"runtime,omitempty"`
	Workdir   string    `json:"workdir,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// StartAgent launches a new subprocess. The runtime+args are exec'd as-is;
// stdout/stderr are redirected to per-agent files (no pipes), and the child
// is placed in its own session via setsid so the supervisor can exit
// independently (KillMode=process friendly).
func (s *Supervisor) StartAgent(req StartAgentRequest) (*Agent, error) {
	if req.Runtime == "" {
		return nil, errors.New("runtime is required")
	}
	id := uuid.NewString()
	stdoutPath := filepath.Join(s.agentsDir, id+".stdout")
	stderrPath := filepath.Join(s.agentsDir, id+".stderr")
	stdoutF, err := os.Create(stdoutPath)
	if err != nil {
		return nil, fmt.Errorf("create stdout file: %w", err)
	}
	stderrF, err := os.Create(stderrPath)
	if err != nil {
		_ = stdoutF.Close()
		return nil, fmt.Errorf("create stderr file: %w", err)
	}

	cmd := s.execCommand(req.Runtime, req.Args...)
	cmd.Dir = req.Workdir
	cmd.Stdout = stdoutF
	cmd.Stderr = stderrF
	if len(req.Env) > 0 {
		envv := os.Environ()
		for k, v := range req.Env {
			envv = append(envv, k+"="+v)
		}
		cmd.Env = envv
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		_ = stdoutF.Close()
		_ = stderrF.Close()
		return nil, fmt.Errorf("start: %w", err)
	}

	pid := cmd.Process.Pid
	ticks, terr := readProcStarttime(pid)
	if terr != nil {
		// Not fatal — short-lived child may have exited already. Use 0 and
		// let the wait goroutine reconcile.
		ticks = 0
	}

	now := time.Now().UTC()
	a := &Agent{
		ID:         id,
		Runtime:    req.Runtime,
		Workdir:    req.Workdir,
		SessionID:  req.SessionID,
		StartedAt:  now,
		StdoutPath: stdoutPath,
		StderrPath: stderrPath,
		cmd:        cmd,
		pid:        pid,
		startTicks: ticks,
		doneCh:     make(chan struct{}),
	}
	if err := insertAgent(s.db, agentRow{
		AgentID: id, PID: pid, StartTicks: ticks, StartedAt: now,
		Status: "running", SessionID: req.SessionID, Runtime: req.Runtime,
		Workdir: req.Workdir, StdoutPath: stdoutPath, StderrPath: stderrPath,
	}); err != nil {
		_ = cmd.Process.Kill()
		_ = stdoutF.Close()
		_ = stderrF.Close()
		return nil, fmt.Errorf("persist agent: %w", err)
	}

	s.mu.Lock()
	s.agents[id] = a
	s.mu.Unlock()

	go func() {
		err := cmd.Wait()
		// Close the file handles after Wait so the kernel-side fd refs
		// drop; the file itself stays on disk for tail/SSE replay.
		_ = stdoutF.Close()
		_ = stderrF.Close()
		exitCode := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				exitCode = ee.ExitCode()
			} else {
				exitCode = -1
			}
		}
		a.markExited(exitCode)
		_ = updateAgentExit(s.db, a.ID, exitCode)
	}()

	return a, nil
}

// CancelAgent sends SIGTERM, then SIGKILL after the configured grace.
func (s *Supervisor) CancelAgent(id string) error {
	a := s.lookup(id)
	if a == nil {
		return errAgentNotFound
	}
	return a.cancel(s.cfg.CancelGrace)
}

// Status returns the AgentStatus for id.
func (s *Supervisor) Status(id string) (AgentStatus, bool) {
	a := s.lookup(id)
	if a == nil {
		return AgentStatus{}, false
	}
	status, exitCode := a.snapshotStatus()
	return AgentStatus{
		AgentID:   a.ID,
		Status:    status,
		ExitCode:  exitCode,
		SessionID: a.SessionID,
		PID:       a.pid,
		Runtime:   a.Runtime,
		Workdir:   a.Workdir,
		StartedAt: a.StartedAt,
	}, true
}

// List returns a stable snapshot of every agent the supervisor knows about.
func (s *Supervisor) List() []AgentStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]AgentStatus, 0, len(s.agents))
	for _, a := range s.agents {
		status, exitCode := a.snapshotStatus()
		out = append(out, AgentStatus{
			AgentID: a.ID, Status: status, ExitCode: exitCode,
			SessionID: a.SessionID, PID: a.pid, Runtime: a.Runtime,
			Workdir: a.Workdir, StartedAt: a.StartedAt,
		})
	}
	return out
}

func (s *Supervisor) lookup(id string) *Agent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.agents[id]
}

var errAgentNotFound = errors.New("agent not found")

// StreamEvents writes SSE-formatted stdout lines for the given agent to w,
// starting from the (1-indexed) line number `fromOffset`. It blocks until the
// agent exits (and the file is fully drained) or ctx is cancelled.
func (s *Supervisor) StreamEvents(ctx context.Context, w io.Writer, flusher http.Flusher, agentID string, fromOffset int64) error {
	a := s.lookup(agentID)
	if a == nil {
		return errAgentNotFound
	}
	f, err := os.Open(a.StdoutPath)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	var lineNum int64
	// Skip first fromOffset lines.
	for lineNum < fromOffset {
		_, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		lineNum++
	}
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 && strings.HasSuffix(line, "\n") {
			lineNum++
			payload, _ := json.Marshal(map[string]any{
				"offset": lineNum,
				"line":   strings.TrimRight(line, "\n"),
			})
			if _, werr := fmt.Fprintf(w, "data: %s\n\n", payload); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
			continue
		}
		// EOF (or partial line). Decide whether to wait or stop.
		if status, _ := a.snapshotStatus(); status == "exited" {
			// Drain finished and process is done.
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
		_ = err // intentionally ignored: we'll retry the read after the wait
	}
}

// Serve binds the unix socket and runs the HTTP API until ctx is cancelled.
func (s *Supervisor) Serve(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.cfg.SocketPath), 0o755); err != nil {
		return fmt.Errorf("mkdir socket parent: %w", err)
	}
	// Refuse to overwrite an existing live socket; remove a stale one.
	_ = os.Remove(s.cfg.SocketPath)
	ln, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", s.cfg.SocketPath, err)
	}
	if err := os.Chmod(s.cfg.SocketPath, 0o600); err != nil {
		return fmt.Errorf("chmod socket: %w", err)
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Handler returns an http.Handler for the supervisor IPC API. Tests can wire
// it to httptest.Server without binding a unix socket.
func (s *Supervisor) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/agents", s.handleAgents)
	mux.HandleFunc("/agents/", s.handleAgentByID)
	return mux
}

func (s *Supervisor) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req StartAgentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
		a, err := s.StartAgent(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusCreated, StartAgentResponse{AgentID: a.ID, StartedAt: a.StartedAt})
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"agents": s.List()})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Supervisor) handleAgentByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/agents/")
	if rest == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}
	switch sub {
	case "":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		st, ok := s.Status(id)
		if !ok {
			http.Error(w, "agent not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, st)
	case "cancel":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := s.CancelAgent(id); err != nil {
			if errors.Is(err, errAgentNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "events":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		fromOffset := int64(0)
		if v := r.URL.Query().Get("from_offset"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				http.Error(w, "invalid from_offset", http.StatusBadRequest)
				return
			}
			fromOffset = n
		}
		// Resolve the agent before flipping to streaming response so the
		// client gets a real 404 rather than an empty 200 stream when the
		// supervisor doesn't know the id (issue #234 worker resume relies
		// on this to mark tasks failed instead of pretending in-flight).
		if _, ok := s.Status(id); !ok {
			http.Error(w, "agent not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
		err := s.StreamEvents(r.Context(), w, flusher, id, fromOffset)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, errAgentNotFound) {
			// Best-effort error reporting for an already-streaming response.
			fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
		}
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
