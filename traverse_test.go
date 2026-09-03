package chickpeas

import "testing"

// TestNeighborsAllocFree pins the documented contract that a direct
// `for range` over Neighbors is allocation-free: the yield closure and
// the type-test dispatch must both stay on the stack, including on the
// below-floor fallback scan that tests every relationship of a node.
func TestNeighborsAllocFree(t *testing.T) {
	b := NewBuilder(8, 16)
	for i := 0; i < 8; i++ {
		if _, err := b.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	for i := NodeID(0); i < 7; i++ {
		if _, err := b.AddRel(i, i+1, "KNOWS"); err != nil {
			t.Fatal(err)
		}
		if _, err := b.AddRel(i, 7-i, "LIKES"); err != nil {
			t.Fatal(err)
		}
	}
	g := b.Finalize("alloc")
	m := g.Match("KNOWS")
	var total int
	// Warm the lazy typed views outside the measured runs.
	for id := NodeID(0); id < 8; id++ {
		for range g.NeighborsMatch(id, Both, m) {
			total++
		}
	}
	allocs := testing.AllocsPerRun(100, func() {
		for id := NodeID(0); id < 8; id++ {
			for range g.NeighborsMatch(id, Both, m) {
				total++
			}
		}
	})
	if allocs != 0 {
		t.Fatalf("NeighborsMatch loop allocated %.0f/run, want 0", allocs)
	}
	if total == 0 {
		t.Fatal("traversal yielded nothing")
	}
}
