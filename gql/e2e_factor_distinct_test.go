// Distinct-aggregate factorization end-to-end: the factored execution
// (DISTINCT partition phase + per-partition COUNT{} + sum) against the
// direct count(DISTINCT rel) pipeline, row-for-row in engine order --
// the fixture's ORDER BY spans unique keys, so sequence is contractual.
// Corners pinned: duplicate prefix rows collapsing per group, a shared
// partition value across groups, and a null partition value (its group
// must surface with score 0, matching count(DISTINCT null) = 0).
package gql_test

import (
	"slices"
	"strings"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// factorGraph: p0 authors mA,mB (both tagged t0 -- duplicate prefix
// rows), p1 authors mC (tagged t1), p2 authors mD (untagged -- null
// partition value), p3 authors mE (tagged t0 -- shares t0 with p0's
// group). Every message tags at most one tag, so HAS_TAG is functional
// from Message and the rewrite qualifies. Expected scores: t0 carries
// 3 creator edges (mA, mB, mE), t1 carries 1 (mC), untagged 0.
func factorGraph(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(32, 32)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var ps []chickpeas.NodeID
	for i := range 4 {
		p, err := b.AddNode("Person")
		must(err)
		must(b.SetProp(p, "pid", int64(i)))
		ps = append(ps, p)
	}
	t0, err := b.AddNode("Tag")
	must(err)
	t1, err := b.AddNode("Tag")
	must(err)
	addMsg := func(author chickpeas.NodeID, tag *chickpeas.NodeID) {
		m, err := b.AddNode("Message")
		must(err)
		_, err = b.AddRel(m, author, "HAS_CREATOR")
		must(err)
		if tag != nil {
			_, err = b.AddRel(m, *tag, "HAS_TAG")
			must(err)
		}
	}
	addMsg(ps[0], &t0)
	addMsg(ps[0], &t0)
	addMsg(ps[1], &t1)
	addMsg(ps[2], nil)
	addMsg(ps[3], &t0)
	return b.Finalize("factor-fixture")
}

const factorE2EQ = "MATCH (m1:Message)-[:HAS_CREATOR]->(p1:Person) OPTIONAL MATCH (m1)-[:HAS_TAG]->(tg:Tag) OPTIONAL MATCH (tg)<-[:HAS_TAG]-(m2:Message)-[c:HAS_CREATOR]->(p3:Person) RETURN p1.pid AS pid, count(DISTINCT c) AS score ORDER BY pid ASC"

func TestFactorDistinctMatchesDirect(t *testing.T) {
	g := factorGraph(t)
	factored := hjRowKeysOrdered(t, g, factorE2EQ)
	plF, err := gql.Explain(g, factorE2EQ)
	if err != nil {
		t.Fatal(err)
	}
	plan.DisableFactorDistinct = true
	defer func() { plan.DisableFactorDistinct = false }()
	direct := hjRowKeysOrdered(t, g, factorE2EQ+" ")
	plD, err := gql.Explain(g, factorE2EQ+" ")
	if err != nil {
		t.Fatal(err)
	}
	// Engagement: the factored plan splits into extra segments; the
	// pinned leg must NOT (a silent double-direct run would pass any
	// differential vacuously).
	if fs, ds := strings.Count(plF, "Segment"), strings.Count(plD, "Segment"); fs <= ds {
		t.Fatalf("factored plan has %d segments vs direct %d -- rewrite did not engage:\n%s", fs, ds, plF)
	}
	if !slices.Equal(factored, direct) {
		t.Fatalf("ordered-row divergence:\nfactored (%d): %v\ndirect   (%d): %v\nplan:\n%s",
			len(factored), factored, len(direct), direct, plF)
	}
	// Content: duplicate prefix rows did not double-count (p0 = 3, not
	// 6), the shared tag scores both its groups (p3 = 3), and the null
	// partition group surfaces with 0 (p2).
	want := []string{
		gjRowKey(value.Int(0), value.Int(3)),
		gjRowKey(value.Int(1), value.Int(1)),
		gjRowKey(value.Int(2), value.Int(0)),
		gjRowKey(value.Int(3), value.Int(3)),
	}
	if !slices.Equal(factored, want) {
		t.Fatalf("rows = %v, want %v", factored, want)
	}
}
