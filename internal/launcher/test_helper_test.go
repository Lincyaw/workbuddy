package launcher

import (
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/config"
	runtimepkg "github.com/Lincyaw/workbuddy/internal/runtime"
	supclient "github.com/Lincyaw/workbuddy/internal/supervisor/client"
)

// newTestLauncher returns a Launcher backed by an in-process supervisor
// rooted at t.TempDir(). The supervisor is shut down when the test ends.
// Tests that previously called NewLauncher() use this helper instead so the
// claude/sh runtimes have a real IPC backend without requiring an external
// `workbuddy supervisor` process.
func newTestLauncher(t *testing.T) *Launcher {
	t.Helper()
	return NewLauncher(newTestSupervisorClient(t), nil)
}

func newTestSupervisorClient(t *testing.T) *supclient.Client {
	t.Helper()
	cli, shutdown, err := NewInProcessSupervisor(t.TempDir(), 200*time.Millisecond)
	if err != nil {
		t.Fatalf("in-process supervisor: %v", err)
	}
	t.Cleanup(shutdown)
	return cli
}

// newTestProcessSession constructs a ProcessSession with a per-test
// in-process supervisor client. Tests use this in place of the previous
// production helper of the same name (which is gone now that ProcessSession
// requires a *supclient.Client).
func newTestProcessSession(t *testing.T, runtimeName string, agent *config.AgentConfig, task *TaskContext, finder runtimepkg.SessionFinder) Session {
	t.Helper()
	return runtimepkg.NewProcessSession(newTestSupervisorClient(t), nil, runtimeName, agent, task, finder)
}
