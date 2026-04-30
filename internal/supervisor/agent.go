package supervisor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Agent is a single supervised subprocess.
type Agent struct {
	ID         string
	Runtime    string
	Workdir    string
	SessionID  string
	StartedAt  time.Time
	StdoutPath string
	StderrPath string

	cmd        *exec.Cmd
	pid        int
	startTicks uint64

	mu       sync.RWMutex
	exited   bool
	exitCode int
	doneCh   chan struct{} // closed when wait returns

	// recovered indicates this Agent was rebuilt from SQLite after a
	// supervisor restart and we do NOT own the os/exec.Cmd handle. Cancel
	// must signal by pid; status updates come from polling /proc.
	recovered bool

	// stdoutLines counts the number of '\n'-terminated lines observed in
	// the stdout file so far. Bumped atomically by the file watcher; SSE
	// consumers use it to know when more bytes are available.
	stdoutLines atomic.Int64
}

func (a *Agent) snapshotStatus() (status string, exitCode *int) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.exited {
		ec := a.exitCode
		return "exited", &ec
	}
	return "running", nil
}

func (a *Agent) markExited(code int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.exited {
		return
	}
	a.exited = true
	a.exitCode = code
	if a.doneCh != nil {
		select {
		case <-a.doneCh:
		default:
			close(a.doneCh)
		}
	}
}

// signal sends sig to the agent process, working for both owned (cmd-based)
// and recovered agents. It does nothing once the agent has exited.
func (a *Agent) signal(sig os.Signal) error {
	a.mu.RLock()
	if a.exited {
		a.mu.RUnlock()
		return nil
	}
	pid := a.pid
	a.mu.RUnlock()
	if pid <= 0 {
		return errors.New("agent has no pid")
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(sig)
}

// cancel implements graceful cancel: SIGTERM, then SIGKILL after grace.
func (a *Agent) cancel(grace time.Duration) error {
	if err := a.signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("sigterm: %w", err)
	}
	if grace <= 0 {
		grace = 5 * time.Second
	}
	a.mu.RLock()
	done := a.doneCh
	a.mu.RUnlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-time.After(grace):
	}
	_ = a.signal(syscall.SIGKILL)
	return nil
}
