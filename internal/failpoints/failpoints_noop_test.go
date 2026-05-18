//go:build !faultinject

package failpoints

import "testing"

func TestNoopEnabled(t *testing.T) {
	if Enabled() {
		t.Fatalf("Enabled() must be false in the default build")
	}
}

func TestNoopTripAlwaysNil(t *testing.T) {
	// Arming has no effect in the no-op build, so Trip must return nil
	// regardless of what we ask for.
	Arm("anything", Effect{Kind: "error", Err: "ignored"})
	if got := Trip("anything"); got != nil {
		t.Fatalf("Trip() = %v, want nil in no-op build", got)
	}
	if got := Trip("anything", WithRepo("a/b"), WithIssue(42)); got != nil {
		t.Fatalf("Trip(with opts) = %v, want nil", got)
	}
}

func TestNoopHitAlwaysNil(t *testing.T) {
	Arm("p", Effect{Kind: "error", Err: "should never surface"})
	if err := Hit("p"); err != nil {
		t.Fatalf("Hit() = %v, want nil in no-op build", err)
	}
}

func TestNoopArmReportsFalse(t *testing.T) {
	if Arm("x", Effect{Kind: "error"}) {
		t.Fatalf("Arm() must return false in no-op build")
	}
}

func TestNoopDisarmReset(t *testing.T) {
	// Must not panic.
	Disarm("missing")
	Reset()
}
