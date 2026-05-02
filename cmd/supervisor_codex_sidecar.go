package cmd

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// codexSidecarOpts configures the long-lived `codex app-server` child
// supervised by `workbuddy supervisor`. The sidecar lets the codex backend
// live independently of the worker process so worker redeploy / upgrade /
// SIGTERM does not kill the codex runtime; this is the abstraction-layer
// piece of REQ-127 (worker lifecycle decoupled from runtime lifecycle).
//
// Transport is WebSocket: codex 0.125.0's `--listen unix://PATH` is
// advertised in --help but rejected at runtime, so we use the only
// non-stdio transport codex actually supports.
type codexSidecarOpts struct {
	// binary is the codex CLI path. Empty disables the sidecar entirely.
	binary string
	// listen is the host:port the codex `app-server --listen ws://`
	// child binds. Worker dials ws://<listen>.
	listen string
	// bypassApprovalsSandbox flips the codex `--dangerously-bypass-approvals-and-sandbox`
	// flag. Required for non-interactive workbuddy use; off by default
	// so test invocations don't accidentally inherit the flag.
	bypassApprovalsSandbox bool
	// minBackoff/maxBackoff bound the restart delay after a codex crash.
	// Empty values fall back to 1s / 30s.
	minBackoff time.Duration
	maxBackoff time.Duration
}

// defaultCodexSidecarListen returns the canonical host:port the codex
// sidecar binds and the worker dials. Loopback only — codex's WebSocket
// transport has no auth layer, so the sidecar must never bind to a
// reachable interface.
func defaultCodexSidecarListen() string {
	return "127.0.0.1:7177"
}

// defaultCodexSidecarURL returns the WebSocket URL the worker dials by
// default. Composed from defaultCodexSidecarListen.
func defaultCodexSidecarURL() string {
	return "ws://" + defaultCodexSidecarListen()
}

// preflightCodexSidecar validates the configuration. Called synchronously
// from supervisor startup so a misconfiguration (bad binary path, malformed
// listen address) aborts startup instead of leaving the supervisor up but
// the sidecar silently dead. Returns the resolved opts (defaults filled).
func preflightCodexSidecar(opts codexSidecarOpts) (codexSidecarOpts, error) {
	opts.binary = strings.TrimSpace(opts.binary)
	if opts.binary == "" {
		// Sidecar disabled — preflight is a no-op.
		return opts, nil
	}
	if strings.TrimSpace(opts.listen) == "" {
		opts.listen = defaultCodexSidecarListen()
	}
	if opts.minBackoff <= 0 {
		opts.minBackoff = 1 * time.Second
	}
	if opts.maxBackoff <= 0 {
		opts.maxBackoff = 30 * time.Second
	}
	if _, err := exec.LookPath(opts.binary); err != nil {
		return opts, fmt.Errorf("codex-sidecar: binary %q not found or not executable: %w", opts.binary, err)
	}
	host, _, err := net.SplitHostPort(opts.listen)
	if err != nil {
		return opts, fmt.Errorf("codex-sidecar: --codex-listen %q must be host:port: %w", opts.listen, err)
	}
	if !isLoopbackMgmtHost(host) {
		// Codex's WebSocket transport has no auth. Refuse to bind on
		// non-loopback even if the operator asked: the failure mode
		// is a network-reachable arbitrary-code-exec endpoint.
		return opts, fmt.Errorf("codex-sidecar: --codex-listen host must be loopback; got %q", host)
	}
	return opts, nil
}

// runCodexSidecar runs the codex app-server child for the lifetime of ctx.
// On crash the child is restarted with exponential backoff. Returns nil
// on clean shutdown (ctx cancel). Pre-flight checks happen in
// preflightCodexSidecar — call that first; this function trusts opts.
//
// The function does not return until ctx is cancelled. Run it in its own
// goroutine after preflight has succeeded.
func runCodexSidecar(ctx context.Context, opts codexSidecarOpts) error {
	if opts.binary == "" {
		log.Printf("[codex-sidecar] disabled (no --codex-binary set)")
		return nil
	}

	log.Printf("[codex-sidecar] starting binary=%s listen=ws://%s bypass=%v", opts.binary, opts.listen, opts.bypassApprovalsSandbox)

	// stableUptime is the window after which we treat the process as
	// "healthy enough" and reset backoff to min. Anything below this is
	// considered crash-loop territory.
	const stableUptime = 30 * time.Second

	backoff := opts.minBackoff
	for {
		if ctx.Err() != nil {
			return nil
		}
		args := []string{}
		if opts.bypassApprovalsSandbox {
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		}
		args = append(args, "app-server", "--listen", "ws://"+opts.listen)

		// Note: deliberately NOT exec.CommandContext — that path only
		// SIGKILLs the leader on ctx cancel, which leaves codex's tool
		// subprocesses orphaned and (worse) holding the leader's
		// stdout/stderr pipes open, so cmd.Wait() blocks forever. We
		// own the kill ourselves and target the whole process group.
		cmd := exec.Command(opts.binary, args...)
		cmd.Env = os.Environ()
		// Run codex in its own process group so we can kill the whole
		// tree (codex + any tool subprocesses it spawned) cleanly.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stderr = newPrefixedWriter(os.Stderr, "[codex-sidecar] ")
		cmd.Stdout = newPrefixedWriter(os.Stderr, "[codex-sidecar] ")

		startedAt := time.Now()
		if err := cmd.Start(); err != nil {
			log.Printf("[codex-sidecar] start failed: %v", err)
			if !sleepOrCancel(ctx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff, opts.maxBackoff)
			continue
		}
		pgid := cmd.Process.Pid // matches Setpgid leader
		log.Printf("[codex-sidecar] up pid=%d", pgid)

		// Watch ctx in a goroutine: when supervisor is shutting down,
		// SIGTERM the whole process group, give it a brief grace
		// period, then SIGKILL. Without this, an orphaned tool child
		// holding the pipe fds keeps cmd.Wait blocked indefinitely.
		// The watcherCtx lets us stop the watcher when codex exits on
		// its own, so we don't accidentally signal the next pgid (pid
		// reuse) on the next iteration.
		watcherCtx, stopWatcher := context.WithCancel(ctx)
		go func() {
			<-watcherCtx.Done()
			if ctx.Err() == nil {
				return
			}
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
			select {
			case <-time.After(2 * time.Second):
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			case <-watcherCtx.Done():
			}
		}()

		waitErr := cmd.Wait()
		stopWatcher()
		uptime := time.Since(startedAt)

		if ctx.Err() != nil {
			log.Printf("[codex-sidecar] shutdown after %s (ctx cancelled)", uptime.Round(time.Millisecond))
			return nil
		}
		if waitErr == nil {
			log.Printf("[codex-sidecar] exited cleanly after %s; restarting", uptime.Round(time.Millisecond))
		} else {
			log.Printf("[codex-sidecar] exited with %v after %s; restarting", waitErr, uptime.Round(time.Millisecond))
		}
		// A long-lived process means whatever caused the crash now is
		// not the same crash-loop pattern that drove the backoff up.
		// Reset to min so the next failure starts fresh.
		if uptime >= stableUptime {
			backoff = opts.minBackoff
		}
		if !sleepOrCancel(ctx, backoff) {
			return nil
		}
		backoff = nextBackoff(backoff, opts.maxBackoff)
	}
}

// sleepOrCancel sleeps for d, returning false if ctx is cancelled first.
func sleepOrCancel(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// nextBackoff doubles cur with a cap at maxBackoff.
func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		return max
	}
	return next
}

// prefixedWriter writes lines to an underlying writer with a fixed prefix
// before each newline-delimited record, so codex's stderr lands under a
// recognizable [codex-sidecar] tag in journalctl.
type prefixedWriter struct {
	w      *os.File
	prefix []byte
	buf    []byte
}

func newPrefixedWriter(w *os.File, prefix string) *prefixedWriter {
	return &prefixedWriter{w: w, prefix: []byte(prefix)}
}

func (p *prefixedWriter) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		line := p.buf[:i+1]
		_, _ = p.w.Write(p.prefix)
		_, _ = p.w.Write(line)
		p.buf = p.buf[i+1:]
	}
	return len(b), nil
}

