// Typed-adjacency parity: the routed view must reproduce the scan path's
// neighbors, order, types, and property-read positions exactly. MatchType
// never carries the typed view, so it drives the scan side of the A/B on
// the same snapshot.
package chickpeas_test

import (
	"math/rand"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// TestNeighborIDSetContract pins the 062 split (mirror of rustychickpeas
// 242 item 2): AppendNeighborsMatch returns a deduplicated ASCENDING id
// set -- parallel same-type rels and Both-direction double-sightings
// collapse -- while AppendNeighborsEach and the Rels iterators preserve
// per-relationship multiplicity in CSR order.
func TestNeighborIDSetContract(t *testing.T) {
	b := chickpeas.NewBuilder(8, 8)
	a, _ := b.AddNode("N")
	x, _ := b.AddNode("N")
	y, _ := b.AddNode("N")
	// Two parallel a->x rels, one a->y, one y->a (seen under Both).
	for _, r := range [][2]chickpeas.NodeID{{a, x}, {a, x}, {a, y}, {y, a}} {
		if _, err := b.AddRel(r[0], r[1], "R"); err != nil {
			t.Fatal(err)
		}
	}
	g := b.Finalize()
	m := g.Match("R")

	set := g.AppendNeighborsMatch(nil, a, chickpeas.Both, m)
	want := []chickpeas.NodeID{x, y} // deduped (x once, y once despite out+in), ascending
	if len(set) != 2 || set[0] != want[0] || set[1] != want[1] {
		t.Fatalf("set contract: %v, want %v", set, want)
	}

	each := g.AppendNeighborsEach(nil, a, chickpeas.Both, m)
	if len(each) != 4 { // x, x, y (out) + y (in): per-rel multiplicity
		t.Fatalf("each contract: %v, want 4 entries", each)
	}

	rels := 0
	for range g.RelsMatch(a, chickpeas.Both, m) {
		rels++
	}
	if rels != 4 {
		t.Fatalf("RelsMatch multiplicity: %d, want 4", rels)
	}

	// A prefix in dst stays untouched and unsorted-into.
	pre := g.AppendNeighborsMatch([]chickpeas.NodeID{99}, a, chickpeas.Outgoing, m)
	if pre[0] != 99 || len(pre) != 3 {
		t.Fatalf("prefix preserved: %v", pre)
	}
}

// TestLabelDenseMatchesSet checks the dense word bitmap agrees with the
// label set exactly, and that below-threshold labels return nil.
func TestLabelDenseMatchesSet(t *testing.T) {
	b := chickpeas.NewBuilder(600, 1)
	for i := 0; i < 600; i++ {
		label := "Rare"
		if i%2 == 0 {
			label = "Common" // 50% >= idspace/8: dense
		}
		if _, err := b.AddNode(label); err != nil {
			t.Fatal(err)
		}
	}
	g := b.Finalize()
	if g.LabelDense("Rare") == nil {
		// Rare is 50% too -- both clear the 1/8 floor here; use a truly
		// sparse label instead.
	}
	words := g.LabelDense("Common")
	if words == nil {
		t.Fatal("Common should be dense")
	}
	set, _ := g.NodesWithLabel("Common")
	for id := 0; id < 600; id++ {
		inBits := words[id>>6]>>(id&63)&1 == 1
		if inBits != set.Contains(uint32(id)) {
			t.Fatalf("id %d: bits %v set %v", id, inBits, set.Contains(uint32(id)))
		}
	}
	if g.LabelDense("NoSuchLabel") != nil {
		t.Fatal("unknown label should return nil")
	}
	// A genuinely sparse label stays nil.
	b2 := chickpeas.NewBuilder(1000, 1)
	for i := 0; i < 1000; i++ {
		label := "Big"
		if i < 20 {
			label = "Tiny"
		}
		if _, err := b2.AddNode(label); err != nil {
			t.Fatal(err)
		}
	}
	if b2.Finalize().LabelDense("Tiny") != nil {
		t.Fatal("2% label should not build a dense bitmap")
	}
}

func TestTypedAdjacencyMatchesScan(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	const n = 400
	b := chickpeas.NewBuilder(n, n*8)
	for i := 0; i < n; i++ {
		if _, err := b.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	types := []string{"HOT", "COLD", "WARM"}
	for i := 0; i < n*6; i++ {
		u, v := chickpeas.NodeID(rng.Intn(n)), chickpeas.NodeID(rng.Intn(n))
		// HOT dominates so it clears the typed floor; the others stay under.
		ty := types[0]
		if i%7 == 0 {
			ty = types[1+i%2]
		}
		idx, err := b.AddRel(u, v, ty)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SetRelPropAt(idx, "w", int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	g := b.Finalize()

	hot, ok := g.RelType("HOT")
	if !ok {
		t.Fatal("no HOT type")
	}
	scan := chickpeas.MatchType(hot) // never routes through the typed view
	typed := g.Match("HOT")          // resolves the typed holder

	for _, dir := range []chickpeas.Direction{chickpeas.Outgoing, chickpeas.Incoming, chickpeas.Both} {
		for id := 0; id < n; id++ {
			node := chickpeas.NodeID(id)
			var a, bb []chickpeas.NodeID
			a = g.AppendNeighborsMatch(a, node, dir, scan)
			bb = g.AppendNeighborsMatch(bb, node, dir, typed)
			if len(a) != len(bb) {
				t.Fatalf("dir %v node %d: %d vs %d neighbors", dir, id, len(a), len(bb))
			}
			for i := range a {
				if a[i] != bb[i] {
					t.Fatalf("dir %v node %d neighbor %d: %d vs %d", dir, id, i, a[i], bb[i])
				}
			}
			// Rels parity: neighbor, type, direction, and property position.
			type ref struct {
				nb  chickpeas.NodeID
				ty  chickpeas.RelType
				d   chickpeas.Direction
				pos uint32
			}
			var ra, rb []ref
			for r := range g.RelsMatch(node, dir, scan) {
				ra = append(ra, ref{r.Neighbor, r.Type, r.Direction, r.Pos})
			}
			for r := range g.RelsMatch(node, dir, typed) {
				rb = append(rb, ref{r.Neighbor, r.Type, r.Direction, r.Pos})
			}
			if len(ra) != len(rb) {
				t.Fatalf("dir %v node %d: %d vs %d rels", dir, id, len(ra), len(rb))
			}
			for i := range ra {
				if ra[i] != rb[i] {
					t.Fatalf("dir %v node %d rel %d: %+v vs %+v", dir, id, i, ra[i], rb[i])
				}
			}
		}
	}

	// CountNeighborsMatch parity: for every (u, v, dir, type) the count
	// equals the number of times v appears in the matched enumeration --
	// including parallel-rel multiplicity, both side-picks, and the
	// below-floor scan fallback.
	for _, ty := range types {
		m := g.Match(ty)
		for _, dir := range []chickpeas.Direction{chickpeas.Outgoing, chickpeas.Incoming, chickpeas.Both} {
			for trial := 0; trial < 500; trial++ {
				u := chickpeas.NodeID(rng.Intn(n))
				v := chickpeas.NodeID(rng.Intn(n))
				want := 0
				for nb := range g.NeighborsMatch(u, dir, m) {
					if nb == v {
						want++
					}
				}
				if got := g.CountNeighborsMatch(u, v, dir, m); got != want {
					t.Fatalf("%s dir %v count(%d,%d) = %d, want %d", ty, dir, u, v, got, want)
				}
			}
		}
	}

	// Below-floor types keep the scan path but stay correct through Match.
	for _, ty := range []string{"COLD", "WARM"} {
		m := g.Match(ty)
		tt, _ := g.RelType(ty)
		sm := chickpeas.MatchType(tt)
		for id := 0; id < n; id++ {
			var a, bb []chickpeas.NodeID
			a = g.AppendNeighborsMatch(a, chickpeas.NodeID(id), chickpeas.Both, sm)
			bb = g.AppendNeighborsMatch(bb, chickpeas.NodeID(id), chickpeas.Both, m)
			if len(a) != len(bb) {
				t.Fatalf("%s node %d: %d vs %d", ty, id, len(a), len(bb))
			}
		}
	}
}
