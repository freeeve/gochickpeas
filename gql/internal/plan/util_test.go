package plan

import (
	"testing"

	"github.com/freeeve/gochickpeas/nodeset"
)

// TestSetLenSlice covers the nil-tolerant nodeset helpers: a nil set reads as
// empty, and a populated set reports its length and materialized ids.
func TestSetLenSlice(t *testing.T) {
	if setLen(nil) != 0 {
		t.Fatal("setLen(nil) must be 0")
	}
	if setSlice(nil) != nil {
		t.Fatal("setSlice(nil) must be nil")
	}

	s := nodeset.New()
	s.Insert(5)
	s.Insert(9)
	if got := setLen(s); got != 2 {
		t.Fatalf("setLen = %d, want 2", got)
	}
	got := setSlice(s)
	seen := map[uint32]bool{}
	for _, id := range got {
		seen[id] = true
	}
	if len(got) != 2 || !seen[5] || !seen[9] {
		t.Fatalf("setSlice = %v, want {5,9}", got)
	}
}
