//go:build faultinject

package failpoints

import (
	"errors"
	"testing"
	"time"
)

func TestInjectEnabled(t *testing.T) {
	if !Enabled() {
		t.Fatalf("Enabled() must be true under -tags faultinject")
	}
}

func TestArmTripHitError(t *testing.T) {
	Reset()
	if !Arm("p.q.before", Effect{Kind: "error", Err: "boom"}) {
		t.Fatalf("Arm() should report true on first install")
	}
	// Second arm on same name reports false (replace semantics).
	if Arm("p.q.before", Effect{Kind: "error", Err: "boom2"}) {
		t.Fatalf("Arm() should report false when replacing")
	}
	got := Trip("p.q.before")
	if got == nil {
		t.Fatalf("Trip() returned nil for armed failpoint")
	}
	if got.Kind != "error" || got.Err != "boom2" {
		t.Fatalf("Trip() = %+v, want Kind=error Err=boom2", got)
	}
	if err := Hit("p.q.before"); err == nil || err.Error() != "boom2" {
		t.Fatalf("Hit() = %v, want error %q", err, "boom2")
	}
}

func TestOnceDisarmsAfterTrip(t *testing.T) {
	Reset()
	Arm("oneshot", Effect{Kind: "error", Err: "only-once", Once: true})
	if got := Trip("oneshot"); got == nil {
		t.Fatalf("first Trip() should fire")
	}
	if got := Trip("oneshot"); got != nil {
		t.Fatalf("second Trip() = %+v, want nil after Once consumed", got)
	}
}

func TestOnceWithHit(t *testing.T) {
	Reset()
	Arm("oneshot.hit", Effect{Kind: "error", Err: "first", Once: true})
	if err := Hit("oneshot.hit"); err == nil {
		t.Fatalf("first Hit should return error")
	}
	if err := Hit("oneshot.hit"); err != nil {
		t.Fatalf("second Hit = %v, want nil after Once", err)
	}
}

func TestMatchRepoNarrowing(t *testing.T) {
	Reset()
	Arm("repo.narrow", Effect{Kind: "error", Err: "x", MatchRepo: "owner/wanted"})

	if got := Trip("repo.narrow", WithRepo("owner/other")); got != nil {
		t.Fatalf("non-matching repo should not trip, got %+v", got)
	}
	if got := Trip("repo.narrow"); got != nil {
		t.Fatalf("missing repo opt should not trip when MatchRepo set, got %+v", got)
	}
	if got := Trip("repo.narrow", WithRepo("owner/wanted")); got == nil {
		t.Fatalf("matching repo should trip")
	}
}

func TestMatchIssueNarrowing(t *testing.T) {
	Reset()
	Arm("issue.narrow", Effect{Kind: "error", Err: "x", MatchIssue: 42})

	if got := Trip("issue.narrow", WithIssue(7)); got != nil {
		t.Fatalf("non-matching issue should not trip, got %+v", got)
	}
	if got := Trip("issue.narrow", WithIssue(42)); got == nil {
		t.Fatalf("matching issue should trip")
	}
}

func TestHitDelay(t *testing.T) {
	Reset()
	Arm("delay.p", Effect{Kind: "delay", Delay: 10 * time.Millisecond})
	start := time.Now()
	if err := Hit("delay.p"); err != nil {
		t.Fatalf("Hit(delay) = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
		t.Fatalf("Hit(delay) returned in %v, expected >= 10ms", elapsed)
	}
}

func TestHitReturnSentinel(t *testing.T) {
	Reset()
	Arm("ret.p", Effect{Kind: "return"})
	err := Hit("ret.p")
	if !errors.Is(err, ErrFailpointReturn) {
		t.Fatalf("Hit(return) = %v, want ErrFailpointReturn", err)
	}
}

func TestHitPanic(t *testing.T) {
	Reset()
	Arm("panic.p", Effect{Kind: "panic", Err: "kaboom"})
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("Hit(panic) should panic")
		}
		if s, ok := r.(string); !ok || s != "kaboom" {
			t.Fatalf("panic value = %v, want %q", r, "kaboom")
		}
	}()
	_ = Hit("panic.p")
	t.Fatalf("unreachable")
}

func TestHitNoEffectArmed(t *testing.T) {
	Reset()
	if err := Hit("not.armed"); err != nil {
		t.Fatalf("Hit(unarmed) = %v, want nil", err)
	}
}

func TestDisarm(t *testing.T) {
	Reset()
	Arm("d.p", Effect{Kind: "error", Err: "x"})
	Disarm("d.p")
	if got := Trip("d.p"); got != nil {
		t.Fatalf("Disarm did not remove entry: %+v", got)
	}
	// Disarm of missing entry is a no-op.
	Disarm("never.existed")
}

// parseEntry coverage — exercises the env-var grammar directly because init()
// can only run once per process and we want each kind / suffix combination
// asserted explicitly.

func TestParseEntryErrorWithMessage(t *testing.T) {
	name, eff, err := parseEntry("p.q.before=error(boom)")
	if err != nil {
		t.Fatalf("parseEntry returned err = %v", err)
	}
	if name != "p.q.before" {
		t.Fatalf("name = %q", name)
	}
	if eff.Kind != "error" || eff.Err != "boom" {
		t.Fatalf("eff = %+v", eff)
	}
}

func TestParseEntryDelay(t *testing.T) {
	_, eff, err := parseEntry("slow=delay(250ms)")
	if err != nil {
		t.Fatalf("parseEntry err = %v", err)
	}
	if eff.Kind != "delay" || eff.Delay != 250*time.Millisecond {
		t.Fatalf("eff = %+v", eff)
	}
}

func TestParseEntryReturnNoArgs(t *testing.T) {
	_, eff, err := parseEntry("short=return")
	if err != nil {
		t.Fatalf("parseEntry err = %v", err)
	}
	if eff.Kind != "return" {
		t.Fatalf("eff = %+v", eff)
	}
}

func TestParseEntryPanic(t *testing.T) {
	_, eff, err := parseEntry("blow=panic(ouch)")
	if err != nil {
		t.Fatalf("parseEntry err = %v", err)
	}
	if eff.Kind != "panic" || eff.Err != "ouch" {
		t.Fatalf("eff = %+v", eff)
	}
}

func TestParseEntrySuffixes(t *testing.T) {
	_, eff, err := parseEntry("p=error(boom,once,issue=54,repo=acme/x)")
	if err != nil {
		t.Fatalf("parseEntry err = %v", err)
	}
	if !eff.Once {
		t.Fatalf("Once not set: %+v", eff)
	}
	if eff.MatchIssue != 54 {
		t.Fatalf("MatchIssue = %d", eff.MatchIssue)
	}
	if eff.MatchRepo != "acme/x" {
		t.Fatalf("MatchRepo = %q", eff.MatchRepo)
	}
	if eff.Err != "boom" {
		t.Fatalf("Err = %q", eff.Err)
	}
}

func TestParseEntryMalformedDoesNotPanic(t *testing.T) {
	// Each case is something the env var might contain. None should panic;
	// each should return a non-nil error so init() logs and skips it.
	cases := []string{
		"",                      // empty
		"no-equals",             // missing '='
		"=lonely",               // empty name
		"p=delay",               // delay without arg
		"p=delay(notaduration)", // bad duration
		"p=unknownkind(x)",      // unknown kind
		"p=error(boom",          // missing close paren
	}
	for _, c := range cases {
		_, _, err := parseEntry(c)
		if err == nil && c != "" {
			t.Errorf("parseEntry(%q) returned nil error, expected one", c)
		}
	}
}

func TestParseEntryBadSuffixIsWarning(t *testing.T) {
	// A suffix token that is recognised in shape (issue=...) but has an
	// invalid value goes through applySuffix and returns an error there;
	// parseEntry catches it, logs to stderr, and still returns a valid
	// effect for the primary kind/message.
	_, eff, err := parseEntry("p=error(boom,issue=notanumber)")
	if err != nil {
		t.Fatalf("parseEntry should not fail on bad suffix value, got %v", err)
	}
	if eff.Err != "boom" {
		t.Fatalf("primary arg lost: %+v", eff)
	}
	if eff.MatchIssue != 0 {
		t.Fatalf("MatchIssue should stay 0 when parse failed: %+v", eff)
	}
}
