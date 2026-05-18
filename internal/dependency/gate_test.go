package dependency

import (
	"errors"
	"testing"

	"github.com/Lincyaw/workbuddy/internal/store"
)

type stubGateStore struct {
	state *store.IssueDependencyState
	err   error
}

func (s stubGateStore) QueryIssueDependencyState(_ string, _ int) (*store.IssueDependencyState, error) {
	return s.state, s.err
}

func TestIsBlocked_TreatsBlockedAndNeedsHumanAsGated(t *testing.T) {
	for _, verdict := range []string{store.DependencyVerdictBlocked, store.DependencyVerdictNeedsHuman} {
		t.Run(verdict, func(t *testing.T) {
			st := stubGateStore{state: &store.IssueDependencyState{Verdict: verdict}}
			blocked, gotVerdict, err := IsBlocked(st, "o/r", 1)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !blocked {
				t.Fatalf("verdict %q should gate dispatch", verdict)
			}
			if gotVerdict != verdict {
				t.Fatalf("verdict roundtrip = %q, want %q", gotVerdict, verdict)
			}
		})
	}
}

func TestIsBlocked_AllowsReadyAndOverride(t *testing.T) {
	for _, verdict := range []string{store.DependencyVerdictReady, store.DependencyVerdictOverride} {
		t.Run(verdict, func(t *testing.T) {
			st := stubGateStore{state: &store.IssueDependencyState{Verdict: verdict}}
			blocked, gotVerdict, err := IsBlocked(st, "o/r", 1)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if blocked {
				t.Fatalf("verdict %q should permit dispatch", verdict)
			}
			if gotVerdict != verdict {
				t.Fatalf("verdict roundtrip = %q, want %q", gotVerdict, verdict)
			}
		})
	}
}

func TestIsBlocked_NoStateIsNotBlocked(t *testing.T) {
	st := stubGateStore{state: nil}
	blocked, verdict, err := IsBlocked(st, "o/r", 1)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if blocked {
		t.Fatal("missing dependency state should not gate dispatch")
	}
	if verdict != "" {
		t.Fatalf("verdict = %q, want empty string when no row exists", verdict)
	}
}

func TestIsBlocked_NilStoreNotBlocked(t *testing.T) {
	blocked, verdict, err := IsBlocked(nil, "o/r", 1)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if blocked {
		t.Fatal("nil store should not gate dispatch")
	}
	if verdict != "" {
		t.Fatalf("verdict = %q, want empty string when store is nil", verdict)
	}
}

func TestIsBlocked_WrapsStoreError(t *testing.T) {
	sentinel := errors.New("boom")
	st := stubGateStore{err: sentinel}
	blocked, verdict, err := IsBlocked(st, "o/r", 1)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
	if blocked {
		t.Fatal("error path must not report blocked=true")
	}
	if verdict != "" {
		t.Fatalf("verdict = %q, want empty string on error path (nothing was observed)", verdict)
	}
}
