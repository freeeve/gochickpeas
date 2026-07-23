package plan

import "testing"

// TestLess4 covers the lexicographic anchor-ordering comparator across every
// tier: the disconnected flag dominates, then the anchor cost, then the
// expansion fan-out, with the written index as the final deterministic
// tie-break, and fully-equal keys are not less.
func TestLess4(t *testing.T) {
	// Tier 1: the disconnected/startable flag dominates every other field.
	if !less4(0, 9, 9, 9, 1, 0, 0, 0) {
		t.Fatal("a smaller disconnected flag must sort first")
	}
	if less4(1, 0, 0, 0, 0, 9, 9, 9) {
		t.Fatal("a larger disconnected flag must not sort first")
	}
	// Tier 2: equal flag -> the smaller anchor cost wins.
	if !less4(0, 5, 9, 9, 0, 9, 0, 0) {
		t.Fatal("the smaller cost must win when the flag ties")
	}
	if less4(0, 9, 0, 0, 0, 5, 9, 9) {
		t.Fatal("the larger cost must lose when the flag ties")
	}
	// Tier 3: equal flag and cost -> the smaller fan-out wins.
	if !less4(0, 5, 1.0, 9, 0, 5, 2.0, 0) {
		t.Fatal("the smaller fan-out must win when flag and cost tie")
	}
	if less4(0, 5, 2.0, 0, 0, 5, 1.0, 9) {
		t.Fatal("the larger fan-out must lose when flag and cost tie")
	}
	// Tier 4: all else equal -> the smaller written index wins.
	if !less4(0, 5, 1.0, 1, 0, 5, 1.0, 2) {
		t.Fatal("the smaller index must break a full tie")
	}
	// Fully equal keys are not strictly less than each other.
	if less4(0, 5, 1.0, 3, 0, 5, 1.0, 3) {
		t.Fatal("equal keys must not be less")
	}
}
