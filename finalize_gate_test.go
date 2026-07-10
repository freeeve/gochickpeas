// Dense-gate selection: the gates count DISTINCT staged positions, never
// raw writes -- duplicate re-sets of a few positions must not push a
// column into the dense layout (mirror of rustychickpeas tasks/237's
// dense_gate_counts_distinct_positions_not_staged_writes).
package chickpeas

import "testing"

// TestAggregateThroughSumPerSourceCountPerRel pins the through-path split
// (mirror of rustychickpeas 240's
// aggregate_through_sum_is_per_source_count_is_per_rel): Post0 (w=10)
// reaches Tag2 via TWO parallel hasTag rels plus Tag3; Post1 (w=100)
// reaches Tag2. count(Tag2) stays per-rel at 3; sum(Tag2) counts each
// source once -- 110, not the degree-inflated 120.
func TestAggregateThroughSumPerSourceCountPerRel(t *testing.T) {
	b := NewBuilder(8, 8)
	post0, _ := b.AddNode("Post")
	post1, _ := b.AddNode("Post")
	tag2, _ := b.AddNode("Tag")
	tag3, _ := b.AddNode("Tag")
	_ = b.SetProp(post0, "w", int64(10))
	_ = b.SetProp(post1, "w", int64(100))
	for _, r := range [][2]NodeID{{post0, tag2}, {post0, tag2}, {post0, tag3}, {post1, tag2}} {
		if _, err := b.AddRel(r[0], r[1], "hasTag"); err != nil {
			t.Fatal(err)
		}
	}
	g := b.Finalize()
	res, err := g.Aggregate("Post").Through("hasTag", Outgoing).Sum("w").Run()
	if err != nil {
		t.Fatal(err)
	}
	got := map[NodeID][2]int64{}
	for _, r := range res.Rows {
		if r.Sum == nil {
			t.Fatalf("nil sum for %v", r.Key)
		}
		got[NodeID(r.Key[len(r.Key)-1])] = [2]int64{int64(r.Count), *r.Sum}
	}
	if got[tag2] != [2]int64{3, 110} {
		t.Fatalf("tag2 = %v, want count 3 / sum 110", got[tag2])
	}
	if got[tag3] != [2]int64{1, 10} {
		t.Fatalf("tag3 = %v, want count 1 / sum 10", got[tag3])
	}
}

// TestStrDenseGateCountsDistinctPositions re-sets a string property nine
// times on each of 100 nodes in a 1000-node span: 900 staged writes clear
// the 0.8*span write gate, but only 100 distinct positions are filled, so
// the column must finalize sparse (dense would store a full span-sized
// value array for a 10% fill and diverge from the Rust writer's bytes).
func TestStrDenseGateCountsDistinctPositions(t *testing.T) {
	b := NewBuilder(1000, 1)
	for i := 0; i < 1000; i++ {
		if _, err := b.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	for rep := 0; rep < 9; rep++ {
		for i := 0; i < 100; i++ {
			if err := b.SetProp(NodeID(i*10), "s", "v"); err != nil {
				t.Fatal(err)
			}
		}
	}
	g := b.Finalize()
	key, ok := g.atoms.ID("s")
	if !ok {
		t.Fatal("no atom for key s")
	}
	col, ok := g.columns[PropertyKey(key)]
	if !ok {
		t.Fatal("no column for key s")
	}
	switch col.(type) {
	case denseStrCol:
		t.Fatalf("100 distinct positions over span 1000 finalized dense (%T)", col)
	}

	// The same span written once per position at full 80%+ distinct fill
	// still selects dense -- the intentional str rule is unchanged.
	b2 := NewBuilder(1000, 1)
	for i := 0; i < 1000; i++ {
		if _, err := b2.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 850; i++ {
		if err := b2.SetProp(NodeID(i), "s", "v"); err != nil {
			t.Fatal(err)
		}
	}
	g2 := b2.Finalize()
	key2, _ := g2.atoms.ID("s")
	col2 := g2.columns[PropertyKey(key2)]
	if _, isDense := col2.(denseStrCol); !isDense {
		t.Fatalf("850 distinct positions over span 1000 should stay dense, got %T", col2)
	}
}
