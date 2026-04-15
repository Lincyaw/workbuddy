package cmd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/Lincyaw/workbuddy/internal/coordinator"
	"github.com/Lincyaw/workbuddy/internal/store"
	"github.com/spf13/cobra"
)

type coordinatorOpts struct {
	dbPath       string
	listenAddr   string
	loopbackOnly bool
}

type tokenCreateOpts struct {
	dbPath   string
	workerID string
	repo     string
	roles    []string
	hostname string
}

type tokenListOpts struct {
	dbPath string
	repo   string
}

type tokenRevokeOpts struct {
	dbPath   string
	workerID string
	kid      string
}

var coordinatorCmd = &cobra.Command{
	Use:   "coordinator",
	Short: "Run the remote coordinator HTTP API",
	RunE:  runCoordinatorCmd,
}

var coordinatorTokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Manage worker authentication tokens",
}

var coordinatorTokenCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create or rotate a worker token",
	RunE:  runCoordinatorTokenCreateCmd,
}

var coordinatorTokenListCmd = &cobra.Command{
	Use:   "list",
	Short: "List worker token metadata",
	RunE:  runCoordinatorTokenListCmd,
}

var coordinatorTokenRevokeCmd = &cobra.Command{
	Use:   "revoke",
	Short: "Revoke a worker token immediately",
	RunE:  runCoordinatorTokenRevokeCmd,
}

func init() {
	coordinatorCmd.Flags().String("db", ".workbuddy/workbuddy.db", "SQLite database path")
	coordinatorCmd.Flags().String("listen", "127.0.0.1:8081", "Coordinator listen address")
	coordinatorCmd.Flags().Bool("loopback-only", false, "Allow auth-free task endpoints for loopback-only dev mode")

	coordinatorTokenCreateCmd.Flags().String("db", ".workbuddy/workbuddy.db", "SQLite database path")
	coordinatorTokenCreateCmd.Flags().String("worker-id", "", "Worker ID")
	coordinatorTokenCreateCmd.Flags().String("repo", "", "Worker repo")
	coordinatorTokenCreateCmd.Flags().StringSlice("roles", nil, "Worker roles")
	coordinatorTokenCreateCmd.Flags().String("hostname", "", "Worker hostname")
	_ = coordinatorTokenCreateCmd.MarkFlagRequired("worker-id")
	_ = coordinatorTokenCreateCmd.MarkFlagRequired("repo")
	_ = coordinatorTokenCreateCmd.MarkFlagRequired("roles")

	coordinatorTokenListCmd.Flags().String("db", ".workbuddy/workbuddy.db", "SQLite database path")
	coordinatorTokenListCmd.Flags().String("repo", "", "Optional repo filter")

	coordinatorTokenRevokeCmd.Flags().String("db", ".workbuddy/workbuddy.db", "SQLite database path")
	coordinatorTokenRevokeCmd.Flags().String("worker-id", "", "Worker ID")
	coordinatorTokenRevokeCmd.Flags().String("kid", "", "Expected key ID")
	_ = coordinatorTokenRevokeCmd.MarkFlagRequired("worker-id")

	coordinatorTokenCmd.AddCommand(coordinatorTokenCreateCmd, coordinatorTokenListCmd, coordinatorTokenRevokeCmd)
	coordinatorCmd.AddCommand(coordinatorTokenCmd)
	rootCmd.AddCommand(coordinatorCmd)
}

func runCoordinatorCmd(cmd *cobra.Command, _ []string) error {
	opts, err := parseCoordinatorFlags(cmd)
	if err != nil {
		return err
	}

	st, err := store.NewStore(opts.dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	handler := coordinator.NewServer(st, coordinator.ServerOptions{
		LoopbackOnly: opts.loopbackOnly,
	})
	srv := &http.Server{
		Addr:    opts.listenAddr,
		Handler: handler,
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		ln, err := net.Listen("tcp", opts.listenAddr)
		if err != nil {
			errCh <- err
			return
		}
		errCh <- srv.Serve(ln)
	}()

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	case <-ctx.Done():
		return srv.Shutdown(context.Background())
	}
}

func parseCoordinatorFlags(cmd *cobra.Command) (*coordinatorOpts, error) {
	dbPath, _ := cmd.Flags().GetString("db")
	listenAddr, _ := cmd.Flags().GetString("listen")
	loopbackOnly, _ := cmd.Flags().GetBool("loopback-only")
	if strings.TrimSpace(listenAddr) == "" {
		return nil, fmt.Errorf("coordinator: --listen is required")
	}
	return &coordinatorOpts{
		dbPath:       dbPath,
		listenAddr:   listenAddr,
		loopbackOnly: loopbackOnly,
	}, nil
}

func runCoordinatorTokenCreateCmd(cmd *cobra.Command, _ []string) error {
	dbPath, _ := cmd.Flags().GetString("db")
	workerID, _ := cmd.Flags().GetString("worker-id")
	repo, _ := cmd.Flags().GetString("repo")
	roles, _ := cmd.Flags().GetStringSlice("roles")
	hostname, _ := cmd.Flags().GetString("hostname")
	if hostname == "" {
		var err error
		hostname, err = os.Hostname()
		if err != nil {
			hostname = "unknown"
		}
	}

	st, err := store.NewStore(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	issued, err := st.IssueWorkerToken(workerID, repo, roles, hostname)
	if err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "worker_id\t%s\nkid\t%s\ntoken\t%s\n", issued.WorkerID, issued.KID, issued.Token)
	return nil
}

func runCoordinatorTokenListCmd(cmd *cobra.Command, _ []string) error {
	dbPath, _ := cmd.Flags().GetString("db")
	repo, _ := cmd.Flags().GetString("repo")

	st, err := store.NewStore(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	records, err := st.ListWorkerTokens(repo)
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "WORKER ID\tREPO\tKID\tSTATUS\tREVOKED")
	for _, rec := range records {
		revoked := "active"
		if rec.RevokedAt != nil {
			revoked = rec.RevokedAt.Format("2006-01-02T15:04:05Z07:00")
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", rec.WorkerID, rec.Repo, rec.KID, rec.Status, revoked)
	}
	return tw.Flush()
}

func runCoordinatorTokenRevokeCmd(cmd *cobra.Command, _ []string) error {
	dbPath, _ := cmd.Flags().GetString("db")
	workerID, _ := cmd.Flags().GetString("worker-id")
	kid, _ := cmd.Flags().GetString("kid")

	st, err := store.NewStore(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if err := st.RevokeWorkerToken(workerID, kid); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "revoked worker_id=%s kid=%s\n", workerID, kid)
	return nil
}
