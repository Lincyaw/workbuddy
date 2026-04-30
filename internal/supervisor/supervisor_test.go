package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func newTestSupervisor(t *testing.T) (*Supervisor, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(Config{
		SocketPath:  filepath.Join(dir, "sup.sock"),
		StateDir:    dir,
		CancelGrace: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

// waitForStatus polls until the agent reaches a target status or timeout.
func waitForStatus(t *testing.T, s *Supervisor, id, want string, timeout time.Duration) AgentStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, ok := s.Status(id)
		if ok && st.Status == want {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, _ := s.Status(id)
	t.Fatalf("agent %s did not reach status=%s within %s (last: %+v)", id, want, timeout, st)
	return AgentStatus{}
}

func TestStartAgentNormalExit(t *testing.T) {
	s, _ := newTestSupervisor(t)
	a, err := s.StartAgent(StartAgentRequest{
		Runtime: "/bin/sh",
		Args:    []string{"-c", "echo hello; echo world; exit 0"},
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	st := waitForStatus(t, s, a.ID, "exited", 3*time.Second)
	if st.ExitCode == nil || *st.ExitCode != 0 {
		t.Fatalf("expected exit_code=0, got %+v", st.ExitCode)
	}
}

func TestStartAgentNonZeroExit(t *testing.T) {
	s, _ := newTestSupervisor(t)
	a, err := s.StartAgent(StartAgentRequest{
		Runtime: "/bin/sh",
		Args:    []string{"-c", "exit 7"},
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	st := waitForStatus(t, s, a.ID, "exited", 3*time.Second)
	if st.ExitCode == nil || *st.ExitCode != 7 {
		t.Fatalf("expected exit_code=7, got %+v", st.ExitCode)
	}
}

func TestCancelAgentSendsSIGTERM(t *testing.T) {
	s, _ := newTestSupervisor(t)
	// trap SIGTERM and exit with a recognisable code.
	a, err := s.StartAgent(StartAgentRequest{
		Runtime: "/bin/sh",
		Args:    []string{"-c", `trap 'exit 42' TERM; while :; do sleep 0.1; done`},
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	// give the trap a moment to install
	time.Sleep(200 * time.Millisecond)
	if err := s.CancelAgent(a.ID); err != nil {
		t.Fatalf("CancelAgent: %v", err)
	}
	st := waitForStatus(t, s, a.ID, "exited", 3*time.Second)
	if st.ExitCode == nil || *st.ExitCode != 42 {
		t.Fatalf("expected SIGTERM-handled exit_code=42, got %+v", st.ExitCode)
	}
}

func TestCancelAgentEscalatesToSIGKILL(t *testing.T) {
	s, _ := newTestSupervisor(t)
	// Ignore TERM so cancel must escalate.
	a, err := s.StartAgent(StartAgentRequest{
		Runtime: "/bin/sh",
		Args:    []string{"-c", `trap '' TERM; while :; do sleep 0.1; done`},
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := s.CancelAgent(a.ID); err != nil {
		t.Fatalf("CancelAgent: %v", err)
	}
	st := waitForStatus(t, s, a.ID, "exited", 3*time.Second)
	// SIGKILL → exit code -1 (Go reports -1 for signalled exits).
	if st.ExitCode == nil || *st.ExitCode == 0 {
		t.Fatalf("expected non-zero exit after SIGKILL, got %+v", st.ExitCode)
	}
}

func TestEventsSSEDeliversStdout(t *testing.T) {
	s, _ := newTestSupervisor(t)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	// Slow producer so the events stream is exercised while the agent is
	// still running.
	body := `{"runtime":"/bin/sh","args":["-c","for i in 1 2 3; do echo line-$i; sleep 0.1; done"]}`
	resp, err := http.Post(srv.URL+"/agents", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /agents: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var sa StartAgentResponse
	if err := json.NewDecoder(resp.Body).Decode(&sa); err != nil {
		t.Fatalf("decode: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/agents/"+sa.AgentID+"/events", nil)
	streamResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer streamResp.Body.Close()

	got := collectSSELines(t, streamResp.Body, 3, 4*time.Second)
	if len(got) != 3 {
		t.Fatalf("want 3 lines, got %d (%v)", len(got), got)
	}
	for i, line := range got {
		want := fmt.Sprintf("line-%d", i+1)
		if line.Line != want || line.Offset != int64(i+1) {
			t.Fatalf("event %d = %+v, want offset=%d line=%q", i, line, i+1, want)
		}
	}
}

func TestEventsSSEFromOffsetSkips(t *testing.T) {
	s, _ := newTestSupervisor(t)
	a, err := s.StartAgent(StartAgentRequest{
		Runtime: "/bin/sh",
		Args:    []string{"-c", "echo a; echo b; echo c"},
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	waitForStatus(t, s, a.ID, "exited", 3*time.Second)

	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/agents/" + a.ID + "/events?from_offset=2")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()
	got := collectSSELines(t, resp.Body, 1, 2*time.Second)
	if len(got) != 1 || got[0].Line != "c" || got[0].Offset != 3 {
		t.Fatalf("want only [c]@3, got %+v", got)
	}
}

func TestRecoverFromDB(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		SocketPath:  filepath.Join(dir, "sup.sock"),
		StateDir:    dir,
		CancelGrace: 200 * time.Millisecond,
	}
	s1, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a, err := s1.StartAgent(StartAgentRequest{
		Runtime: "/bin/sh",
		Args:    []string{"-c", "echo persisted; exit 0"},
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	waitForStatus(t, s1, a.ID, "exited", 3*time.Second)
	_ = s1.Close()

	// Fresh supervisor over the same state.
	s2, err := New(cfg)
	if err != nil {
		t.Fatalf("New (recovery): %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	st, ok := s2.Status(a.ID)
	if !ok {
		t.Fatalf("recovered supervisor lost agent %s", a.ID)
	}
	if st.Status != "exited" || st.ExitCode == nil || *st.ExitCode != 0 {
		t.Fatalf("recovered status wrong: %+v", st)
	}
	// Stdout file is still readable; SSE replay should work.
	srv := httptest.NewServer(s2.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/agents/" + a.ID + "/events")
	if err != nil {
		t.Fatalf("GET events post-recover: %v", err)
	}
	defer resp.Body.Close()
	got := collectSSELines(t, resp.Body, 1, 2*time.Second)
	if len(got) != 1 || got[0].Line != "persisted" {
		t.Fatalf("post-recover replay: %+v", got)
	}
}

func TestRecoverFromDBStaleRunningRowFlippedToExited(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{StateDir: dir, SocketPath: filepath.Join(dir, "x.sock")}
	s1, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Manually insert a row that claims to be running for a pid that does
	// not exist (or cannot match the recorded start_ticks).
	bogusPID := 1
	row := agentRow{
		AgentID: "bogus", PID: bogusPID, StartTicks: 999999999,
		StartedAt: time.Now(), Status: "running",
		StdoutPath: filepath.Join(dir, "agents", "bogus.stdout"),
		StderrPath: filepath.Join(dir, "agents", "bogus.stderr"),
	}
	if err := os.WriteFile(row.StdoutPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := insertAgent(s1.db, row); err != nil {
		t.Fatalf("insertAgent: %v", err)
	}
	_ = s1.Close()

	s2, err := New(cfg)
	if err != nil {
		t.Fatalf("New (recover): %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	st, ok := s2.Status("bogus")
	if !ok {
		t.Fatalf("missing")
	}
	if st.Status != "exited" {
		t.Fatalf("stale row should be flipped to exited, got %+v", st)
	}
}

func TestListAgentsHTTP(t *testing.T) {
	s, _ := newTestSupervisor(t)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	for i := 0; i < 2; i++ {
		_, err := s.StartAgent(StartAgentRequest{
			Runtime: "/bin/sh", Args: []string{"-c", "exit 0"},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	resp, err := http.Get(srv.URL + "/agents")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Agents []AgentStatus `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Agents) != 2 {
		t.Fatalf("want 2 agents, got %d", len(out.Agents))
	}
}

func TestSetsidIsolatesChildFromSupervisorSignals(t *testing.T) {
	// A child started with Setsid should be in its own process group, so a
	// signal sent to the supervisor's process group does not also kill it.
	s, _ := newTestSupervisor(t)
	a, err := s.StartAgent(StartAgentRequest{
		Runtime: "/bin/sh",
		Args:    []string{"-c", "sleep 1; exit 0"},
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	// Confirm the child's pgid is the child pid (i.e. it leads its own group).
	pgid, err := syscall.Getpgid(a.pid)
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	if pgid != a.pid {
		t.Fatalf("expected pgid==pid (own session), got pgid=%d pid=%d", pgid, a.pid)
	}
	waitForStatus(t, s, a.ID, "exited", 3*time.Second)
}

type sseLine struct {
	Offset int64  `json:"offset"`
	Line   string `json:"line"`
}

func collectSSELines(t *testing.T, body interface{ Read(p []byte) (int, error) }, want int, timeout time.Duration) []sseLine {
	t.Helper()
	type readResult struct {
		lines []sseLine
		err   error
	}
	ch := make(chan readResult, 1)
	go func() {
		var lines []sseLine
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var l sseLine
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &l); err != nil {
				ch <- readResult{lines, err}
				return
			}
			lines = append(lines, l)
			if len(lines) >= want {
				ch <- readResult{lines, nil}
				return
			}
		}
		ch <- readResult{lines, scanner.Err()}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("scan: %v", r.err)
		}
		return r.lines
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %d sse lines", want)
		return nil
	}
}
