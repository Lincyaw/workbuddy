package app

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Lincyaw/workbuddy/internal/store"
)

func TestRecoverCoordinatorIssueClaimsSweepsOnlyStaleCoordinatorRows(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "recover.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const currentPID = 4242
	seed := []struct {
		repo   string
		issue  int
		worker string
	}{
		{repo: "owner/repo", issue: 1, worker: BuildIssueClaimerID("coordinator-host-a", 111)},
		{repo: "owner/repo", issue: 2, worker: BuildIssueClaimerID("coordinator-host-b", 222)},
		{repo: "owner/repo", issue: 3, worker: BuildIssueClaimerID("coordinator-host-current", currentPID)},
		{repo: "owner/repo", issue: 4, worker: "machine-ddq"},
	}
	for _, tc := range seed {
		if _, err := st.AcquireIssueClaim(tc.repo, tc.issue, tc.worker, time.Hour); err != nil {
			t.Fatalf("AcquireIssueClaim(%s#%d): %v", tc.repo, tc.issue, err)
		}
	}

	if err := RecoverCoordinatorIssueClaims(st, currentPID); err != nil {
		t.Fatalf("RecoverCoordinatorIssueClaims: %v", err)
	}

	for _, issueNum := range []int{1, 2} {
		claim, err := st.QueryIssueClaim("owner/repo", issueNum)
		if err != nil {
			t.Fatalf("QueryIssueClaim stale #%d: %v", issueNum, err)
		}
		if claim != nil {
			t.Fatalf("expected stale coordinator claim #%d to be deleted, got %+v", issueNum, claim)
		}
	}

	kept := map[int]string{
		3: BuildIssueClaimerID("coordinator-host-current", currentPID),
		4: "machine-ddq",
	}
	for issueNum, workerID := range kept {
		claim, err := st.QueryIssueClaim("owner/repo", issueNum)
		if err != nil {
			t.Fatalf("QueryIssueClaim kept #%d: %v", issueNum, err)
		}
		if claim == nil || claim.WorkerID != workerID {
			t.Fatalf("expected issue #%d to keep worker %q, got %+v", issueNum, workerID, claim)
		}
	}
}
