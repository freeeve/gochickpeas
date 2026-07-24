// Core type helper tests.
package chickpeas

import "testing"

// TestDirectionStringAndReverse pins Direction's Stringer and Reverse: each
// value renders its lowercase name (invalid for an out-of-range value), and
// Reverse swaps Outgoing/Incoming while leaving Both fixed.
func TestDirectionStringAndReverse(t *testing.T) {
	for _, c := range []struct {
		d    Direction
		want string
	}{
		{Outgoing, "outgoing"},
		{Incoming, "incoming"},
		{Both, "both"},
		{Direction(99), "invalid"},
	} {
		if got := c.d.String(); got != c.want {
			t.Fatalf("Direction(%d).String() = %q, want %q", c.d, got, c.want)
		}
	}
	if Outgoing.Reverse() != Incoming || Incoming.Reverse() != Outgoing || Both.Reverse() != Both {
		t.Fatal("Reverse must swap Outgoing/Incoming and fix Both")
	}
}
