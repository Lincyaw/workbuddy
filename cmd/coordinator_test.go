package cmd

import "testing"

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
