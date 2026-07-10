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
