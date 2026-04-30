package launcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/Lincyaw/workbuddy/internal/supervisor"
	supclient "github.com/Lincyaw/workbuddy/internal/supervisor/client"
)

// NewInProcessSupervisor starts a supervisor inside the current process,
// bound to a localhost TCP port (no unix socket), and returns a client for
// it. This is the launch model for one-shot CLI commands like `workbuddy
// run` and for unit tests, where running a separate `workbuddy supervisor`
// would be overkill. The returned shutdown function stops the HTTP server
// and closes the supervisor.
//
// In long-lived deployments (worker, serve), callers connect to a
// pre-existing supervisor over a unix socket via supclient.NewUnix instead;
// the whole point of the supervisor is that its lifetime is independent of
// the worker's.
func NewInProcessSupervisor(stateDir string, cancelGrace time.Duration) (*supclient.Client, func(), error) {
	if stateDir == "" {
		return nil, nil, errors.New("launcher: in-process supervisor requires a stateDir")
	}
	sup, err := supervisor.New(supervisor.Config{
		// SocketPath unused: we serve over a localhost TCP listener so we
		// don't depend on a writable XDG_RUNTIME_DIR or fight stale unix
		// sockets in CI.
		SocketPath:  stateDir + "/.unused.sock",
		StateDir:    stateDir,
		CancelGrace: cancelGrace,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("init in-process supervisor: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = sup.Close()
		return nil, nil, fmt.Errorf("listen 127.0.0.1: %w", err)
	}
	srv := &http.Server{Handler: sup.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		_ = srv.Serve(ln)
	}()
	cli := supclient.NewHTTP("http://"+ln.Addr().String(), &http.Client{Timeout: 0})
	shutdown := func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(shutCtx)
		cancel()
		_ = sup.Close()
	}
	return cli, shutdown, nil
}
