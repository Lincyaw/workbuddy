package control

import (
	"testing"
)

func TestDecide_Complete(t *testing.T) {
	c := &Controller{MaxRounds: 5}
	delta := &Delta{AllMet: true}
	d := c.Decide(delta, 1, nil)
	if d.Action != ActionComplete {
		t.Fatalf("expected ActionComplete, got %d", d.Action)
	}
}

func TestDecide_Resume(t *testing.T) {
	c := &Controller{MaxRounds: 5}
	delta := &Delta{Missing: []string{"branch not pushed"}}
	d := c.Decide(delta, 1, nil)
	if d.Action != ActionResume {
		t.Fatalf("expected ActionResume, got %d", d.Action)
	}
	if d.Round != 1 {
		t.Fatalf("round = %d, want 1", d.Round)
	}
}

func TestDecide_Exhausted(t *testing.T) {
	c := &Controller{MaxRounds: 3}
	delta := &Delta{Missing: []string{"branch not pushed"}}
	d := c.Decide(delta, 3, nil)
	if d.Action != ActionBlock {
		t.Fatalf("expected ActionBlock on exhaustion, got %d", d.Action)
	}
	if d.Message == "" {
		t.Fatalf("expected block message")
	}
}

func TestDecide_Stuck(t *testing.T) {
	c := &Controller{MaxRounds: 10}
	delta := &Delta{Missing: []string{"PR not open"}}
	prevDelta := &Delta{Missing: []string{"PR not open"}}

	// Round 2: not stuck yet (need >= 3 rounds)
	d := c.Decide(delta, 2, prevDelta)
	if d.Action != ActionResume {
		t.Fatalf("round 2 should resume, got %d", d.Action)
	}

	// Round 3: stuck
	d = c.Decide(delta, 3, prevDelta)
	if d.Action != ActionBlock {
		t.Fatalf("round 3 with same unmet should block, got %d", d.Action)
	}
}

func TestDecide_DefaultMaxRounds(t *testing.T) {
	c := &Controller{} // MaxRounds = 0 → defaults to 5
	delta := &Delta{Missing: []string{"branch not pushed"}}
	d := c.Decide(delta, 5, nil)
	if d.Action != ActionBlock {
		t.Fatalf("expected block at default max (5), got %d", d.Action)
	}
}

func TestSameUnmet(t *testing.T) {
	a := &Delta{Missing: []string{"PR not open", "branch not pushed"}}
	b := &Delta{Missing: []string{"branch not pushed", "PR not open"}}
	if !sameUnmet(a, b) {
		t.Fatal("same set in different order should match")
	}

	c := &Delta{Missing: []string{"PR not open"}}
	if sameUnmet(a, c) {
		t.Fatal("different missing sets should not match")
	}
}
