package cmd

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Lincyaw/workbuddy/internal/supervisor"
	"github.com/spf13/cobra"
)

var supervisorCmd = &cobra.Command{
	Use:   "supervisor",
	Short: "Run the local agent supervisor (unix-socket IPC API)",
	Long: `Run a local supervisor process that owns long-lived agent subprocesses on
behalf of (potentially transient) workbuddy worker processes. The supervisor
exposes an HTTP API over a unix socket so callers on the same host can start,
observe, cancel, and tail agent runs without keeping the worker pid alive.

Default socket: $XDG_RUNTIME_DIR/workbuddy-supervisor.sock
Default state:  $XDG_STATE_HOME/workbuddy (or ~/.local/state/workbuddy)`,
	RunE: runSupervisor,
}

func init() {
	supervisorCmd.Flags().String("socket", "", "Unix socket path (default $XDG_RUNTIME_DIR/workbuddy-supervisor.sock)")
	supervisorCmd.Flags().String("state-dir", "", "State directory holding agents/ and supervisor.db (default $XDG_STATE_HOME/workbuddy)")
	supervisorCmd.Flags().Duration("cancel-grace", supervisor.DefaultCancelGrace, "Time to wait between SIGTERM and SIGKILL when cancelling an agent")
	rootCmd.AddCommand(supervisorCmd)
}

func runSupervisor(cmd *cobra.Command, _ []string) error {
	socket, _ := cmd.Flags().GetString("socket")
	stateDir, _ := cmd.Flags().GetString("state-dir")
	grace, _ := cmd.Flags().GetDuration("cancel-grace")

	sup, err := supervisor.New(supervisor.Config{
		SocketPath:  socket,
		StateDir:    stateDir,
		CancelGrace: grace,
	})
	if err != nil {
		return fmt.Errorf("init supervisor: %w", err)
	}
	defer sup.Close()

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("supervisor: listening on %s (state %s, grace %s)",
		sup.SocketPath(), sup.AgentsDir(), grace.Round(time.Millisecond))
	notifySystemdReady()
	if err := sup.Serve(ctx); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// notifySystemdReady sends READY=1 to $NOTIFY_SOCKET when running under a
// Type=notify systemd unit. No-op when NOTIFY_SOCKET is unset (e.g. running
// directly from a shell or under Type=simple).
func notifySystemdReady() {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	conn, err := net.Dial("unixgram", sock)
	if err != nil {
		log.Printf("supervisor: sd_notify dial %s: %v", sock, err)
		return
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("READY=1\n")); err != nil {
		log.Printf("supervisor: sd_notify write: %v", err)
	}
}

// supervisorContextWithCancel exposes a context.WithCancel for tests.
var _ = context.Background
