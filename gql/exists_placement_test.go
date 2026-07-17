// Predicate-placement oracle for graph-walking conjuncts (task 115): a
// memoized NOT EXISTS correlated on a pair must not run before the
// into-bound rebind that discards most of the pairs it would be asked
// about. The assertion is the WALK COUNT (compile.SubqueryWalks) -- rows
// are identical under either placement, and a duration can lie on a
// shared box; the walk count is exact and names the defect.
package gql

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/compile"
)

// q18Fixture: tag with two interested persons A and B; ten mutual friends
// each knowing A, B, and one uninterested x_i. From each interested
// person the KNOWS-KNOWS expand reaches 11 distinct partners (B/A plus
// ten x's -- 22 distinct (p1,p2) pairs), of which only the interested one
// survives the into-bound HAS_INTEREST rebind (2 pairs). No direct
// A-KNOWS-B edge, so NOT EXISTS holds and all 20 rows emit.
func q18Fixture(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(64, 128)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	tag, err := b.AddNode("Tag")
	must(err)
	must(b.SetProp(tag, "name", "t"))
	person := func(name string, interested bool) chickpeas.NodeID {
		n, err := b.AddNode("Person")
		must(err)
		must(b.SetProp(n, "name", name))
		if interested {
			_, err = b.AddRel(n, tag, "HAS_INTEREST")
			must(err)
		}
		return n
	}
	a := person("A", true)
	bb := person("B", true)
	for i := range 10 {
		mf := person("mf", false)
		_, err = b.AddRel(a, mf, "KNOWS")
		must(err)
		_, err = b.AddRel(mf, bb, "KNOWS")
		must(err)
		x := person("x", false)
		_ = i
		_, err = b.AddRel(mf, x, "KNOWS")
		must(err)
	}
	return b.Finalize("q18")
}

// TestExistsConjunctPlacement pins both placement regimes.
//
// Case 1 -- the manifest Q18 shape verbatim: the single-hop both-bound
// NOT EXISTS deliberately skips the memo (cheapExistsProbe: a bound-pair
// edge-key probe beats a memo) and is hasSlow, so it evaluates once per
// row REACHING THE LAST LEVEL: 20 evals, the post-shrink row count.
// 40 would mean it ran at the pair's binding level.
//
// Case 2 -- a two-hop NOT EXISTS on a bound pair decorrelates: the inner
// pattern is enumerated ONCE per distinct anchor as a grouped side table
// and every row answers with a map read, so the per-row walk count is
// ZERO (before decorrelation the memo bounded it at one walk per
// surviving distinct pair; a nonzero count now means decor stopped
// firing and the memo path resumed).
func TestExistsConjunctPlacement(t *testing.T) {
	g := q18Fixture(t)
	run := func(q string) int {
		t.Helper()
		before := compile.SubqueryWalks
		rows, err := Run(g, q)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for range rows.All() {
			n++
		}
		if n != 20 {
			t.Fatalf("rows = %d, want 20 (10 mutual friends x 2 directions)", n)
		}
		return compile.SubqueryWalks - before
	}
	base := "MATCH (tag:Tag {name: 't'})<-[:HAS_INTEREST]-(p1:Person)-[:KNOWS]-(mf:Person)-[:KNOWS]-(p2:Person)-[:HAS_INTEREST]->(tag) WHERE p1 <> p2 AND "
	if w := run(base + "NOT EXISTS { (p1)-[:KNOWS]-(p2) } RETURN p1.name AS n1, p2.name AS n2"); w != 20 {
		t.Fatalf("cheap-probe NOT EXISTS evaluated %d times, want 20 (once per post-shrink row; 40 means pre-semijoin placement)", w)
	}
	if w := run(base + "NOT EXISTS { (p1)-[:FRIEND]->(:W)-[:FRIEND]->(p2) } RETURN p1.name AS n1, p2.name AS n2"); w != 0 {
		t.Fatalf("decorrelated 2-hop NOT EXISTS walked %d times, want 0 (the anchor side table answers every row)", w)
	}
}
