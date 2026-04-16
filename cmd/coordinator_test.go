package cmd

import (
	"testing"
	"time"
)

func TestParseCoordinatorFlagsRejectsNonLoopbackBypass(t *testing.T) {
	cmd := coordinatorCmd
	cmd.Flags().Set("listen", "0.0.0.0:8081")
	cmd.Flags().Set("loopback-only", "true")
	t.Cleanup(func() {
		cmd.Flags().Set("listen", "127.0.0.1:8081")
		cmd.Flags().Set("loopback-only", "false")
	})

	_, err := parseCoordinatorFlags(cmd)
	if err == nil {
		t.Fatal("expected error for non-loopback listen address")
	}
}

func TestParseCoordinatorFlagsAllowsLoopbackBypass(t *testing.T) {
	tests := []string{
		"127.0.0.1:8081",
		"[::1]:8081",
		"localhost:8081",
	}

	for _, listenAddr := range tests {
		t.Run(listenAddr, func(t *testing.T) {
			cmd := coordinatorCmd
			cmd.Flags().Set("listen", listenAddr)
			cmd.Flags().Set("loopback-only", "true")
			t.Cleanup(func() {
				cmd.Flags().Set("listen", "127.0.0.1:8081")
				cmd.Flags().Set("loopback-only", "false")
			})

			opts, err := parseCoordinatorFlags(cmd)
			if err != nil {
				t.Fatalf("parseCoordinatorFlags: %v", err)
			}
			if opts.listenAddr != listenAddr {
				t.Fatalf("listenAddr = %q, want %q", opts.listenAddr, listenAddr)
			}
			if !opts.loopbackOnly {
				t.Fatal("expected loopbackOnly to be true")
			}
		})
	}
}

func TestParseCoordinatorFlagsReadsNewOptions(t *testing.T) {
	cmd := coordinatorCmd
	cmd.Flags().Set("listen", "127.0.0.1:8081")
	cmd.Flags().Set("config-dir", " .github/workbuddy/ ")
	cmd.Flags().Set("port", "8123")
	cmd.Flags().Set("poll-interval", "42s")
	cmd.Flags().Set("auth", "true")
	t.Cleanup(func() {
		cmd.Flags().Set("config-dir", ".github/workbuddy")
		cmd.Flags().Set("port", "0")
		cmd.Flags().Set("poll-interval", "0s")
		cmd.Flags().Set("auth", "false")
	})

	opts, err := parseCoordinatorFlags(cmd)
	if err != nil {
		t.Fatalf("parseCoordinatorFlags: %v", err)
	}
	if got, want := opts.configDir, ".github/workbuddy/"; got != want {
		t.Fatalf("configDir = %q, want %q", got, want)
	}
	if got, want := opts.port, 8123; got != want {
		t.Fatalf("port = %d, want %d", got, want)
	}
	if got, want := opts.pollInterval, 42*time.Second; got != want {
		t.Fatalf("pollInterval = %s, want %s", got, want)
	}
	if !opts.auth {
		t.Fatal("expected auth to be true")
	}
}
