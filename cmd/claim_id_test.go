package cmd

import "testing"

func TestBuildIssueClaimerID(t *testing.T) {
	got := buildIssueClaimerID("coordinator-host", 4321)
	want := "coordinator-host-pid-4321"
	if got != want {
		t.Fatalf("buildIssueClaimerID() = %q, want %q", got, want)
	}
}
