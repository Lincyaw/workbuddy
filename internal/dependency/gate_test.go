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
			blocked, err := IsBlocked(st, "o/r", 1)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !blocked {
				t.Fatalf("verdict %q should gate dispatch", verdict)
			}
		})
	}
}

func TestIsBlocked_AllowsReadyAndOverride(t *testing.T) {
	for _, verdict := range []string{store.DependencyVerdictReady, store.DependencyVerdictOverride} {
		t.Run(verdict, func(t *testing.T) {
			st := stubGateStore{state: &store.IssueDependencyState{Verdict: verdict}}
			blocked, err := IsBlocked(st, "o/r", 1)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if blocked {
				t.Fatalf("verdict %q should permit dispatch", verdict)
			}
		})
	}
}

func TestIsBlocked_NoStateIsNotBlocked(t *testing.T) {
	st := stubGateStore{state: nil}
	blocked, err := IsBlocked(st, "o/r", 1)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if blocked {
		t.Fatal("missing dependency state should not gate dispatch")
	}
}

func TestIsBlocked_NilStoreNotBlocked(t *testing.T) {
	blocked, err := IsBlocked(nil, "o/r", 1)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if blocked {
		t.Fatal("nil store should not gate dispatch")
	}
}

func TestIsBlocked_WrapsStoreError(t *testing.T) {
	sentinel := errors.New("boom")
	st := stubGateStore{err: sentinel}
	_, err := IsBlocked(st, "o/r", 1)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}
