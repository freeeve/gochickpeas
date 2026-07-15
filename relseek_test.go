// Bound-pair position seek differential (task 143): AppendRelsBetweenMatch
// must return the exact multiset of stored relationship positions that a
// brute-force enumerate-and-filter of the from-node's run produces -- for
// every direction, across the typed-view (above-floor) and run-view
// (below-floor) tiers and the multi-type scan fallback, with parallel
// relationships preserving multiplicity. This is what proves the seek is a
// pure cost optimization: same relationships, same positions, just the
// lower-degree endpoint scanned.
package chickpeas_test

import (
	"math/rand"
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// enumBetween is the reference: every m-matched dir relationship from u
// whose neighbor is v, by stored position -- the enumerate-and-filter the
// seek replaces.
func enumBetween(g *chickpeas.Snapshot, u, v chickpeas.NodeID, dir chickpeas.Direction, m chickpeas.RelMatch) []uint32 {
	var out []uint32
	for r := range g.RelsMatch(u, dir, m) {
		if r.Neighbor == v {
			out = append(out, r.Pos)
		}
	}
	return out
}

func TestAppendRelsBetweenMatchesEnumeration(t *testing.T) {
	rng := rand.New(rand.NewSource(143))
	const n = 200
	b := chickpeas.NewBuilder(n, n*8)
	ids := make([]chickpeas.NodeID, n)
	for i := range ids {
		ids[i], _ = b.AddNode("N")
	}
	// A rel property populates inToOut, so incoming positions map to the
	// stored (outgoing) frame and the reverse-side seek fires -- the state
	// where a named-rel position is actually read (the IC5 shape). HOT sits
	// above the typed floor (>= idspace/4 rels), COLD below it, so one graph
	// exercises both tiers. Parallel duplicates give multiplicity.
	setW := func(idx int, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SetRelPropAt(idx, "w", int64(idx)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 400; i++ {
		u, v := ids[rng.Intn(n)], ids[rng.Intn(n)]
		setW(b.AddRel(u, v, "HOT"))
		if rng.Intn(3) == 0 { // parallel HOT rel
			setW(b.AddRel(u, v, "HOT"))
		}
	}
	for i := 0; i < 30; i++ {
		u, v := ids[rng.Intn(n)], ids[rng.Intn(n)]
		setW(b.AddRel(u, v, "COLD"))
	}
	g := b.Finalize()

	matchers := map[string]chickpeas.RelMatch{
		"HOT":      g.Match("HOT"),         // above floor -> typed view
		"COLD":     g.Match("COLD"),        // below floor -> run view / edge set
		"HOT+COLD": g.Match("HOT", "COLD"), // multi-type -> scan fallback
	}
	dirs := []chickpeas.Direction{chickpeas.Outgoing, chickpeas.Incoming, chickpeas.Both}

	sorted := func(s []uint32) []uint32 { c := slices.Clone(s); slices.Sort(c); return c }

	for name, m := range matchers {
		for _, dir := range dirs {
			for trial := 0; trial < 3000; trial++ {
				u, v := ids[rng.Intn(n)], ids[rng.Intn(n)]
				want := enumBetween(g, u, v, dir, m)
				got := g.AppendRelsBetweenMatch(nil, u, v, dir, m)
				if !slices.Equal(sorted(want), sorted(got)) {
					t.Fatalf("%s dir=%d u=%d v=%d: seek=%v enumerate=%v (multiset mismatch)",
						name, dir, u, v, sorted(got), sorted(want))
				}
			}
		}
	}
}
