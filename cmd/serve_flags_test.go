package cmd

import "testing"

func TestParseServeFlagsReadsTrustedAuthors(t *testing.T) {
	cmd := serveCmd
	cmd.Flags().Set("max-parallel-tasks", "1")
	cmd.Flags().Set("trusted-authors", "alice,bob")
	t.Cleanup(func() {
		cmd.Flags().Set("max-parallel-tasks", "1")
		cmd.Flags().Set("trusted-authors", "")
	})

	opts, err := parseServeFlags(cmd)
	if err != nil {
		t.Fatalf("parseServeFlags: %v", err)
	}
	if got, want := opts.trustedAuthors, "alice,bob"; got != want {
		t.Fatalf("trustedAuthors = %q, want %q", got, want)
	}
	if !opts.trustedAuthorsSet {
		t.Fatal("expected trustedAuthorsSet to be true")
	}
}
