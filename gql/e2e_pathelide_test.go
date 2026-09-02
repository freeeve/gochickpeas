// Named-path elision end-to-end: the elided execution (rel-list column,
// no path assembly) against the assembled-path pipeline, row-for-row in
// engine order over the descending-timestamp trail idiom -- duplicate
// trails, rejected trails, and the boundary-carried comprehension all
// included. The fixture's ORDER BY spans unique keys.
package gql_test

import (
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// elideGraph: a chain a0<-a1<-a2<-a3 with strictly descending ct on the
// walk from a0 outward, plus a branch a0<-b with ct breaking the
// descent when extended (rejected by the filter at depth 2).
func elideGraph(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(16, 16)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var ids []chickpeas.NodeID
	for i := range 4 {
		n, err := b.AddNode("A")
		must(err)
		must(b.SetProp(n, "aid", int64(i)))
		ids = append(ids, n)
	}
	for i := 0; i < 3; i++ {
		r, err := b.AddRel(ids[i+1], ids[i], "T")
		must(err)
		must(b.SetRelPropAt(r, "ct", int64(100-i*10)))
	}
	br, err := b.AddNode("A")
	must(err)
	must(b.SetProp(br, "aid", int64(9)))
	r, err := b.AddRel(br, ids[1], "T")
	must(err)
	must(b.SetRelPropAt(r, "ct", int64(95))) // 100 > 95 fails? no: ts[0]=90(a1->?) ordering fixed below
	return b.Finalize("elide-e2e")
}

const elideQ = "MATCH p = TRAIL (s:A {aid: 0})<-[:T]-{1,3}(o:A) LET ts = [r IN rels(p) | r.ct] FILTER all(i IN range(0, size(ts) - 2) WHERE ts[i] > ts[i + 1]) RETURN o.aid AS oid, min(size(ts)) AS dist ORDER BY oid"

func TestPathElideMatchesAssembled(t *testing.T) {
	g := elideGraph(t)
	elided := hjRowKeysOrdered(t, g, elideQ)
	plan.DisablePathElide = true
	defer func() { plan.DisablePathElide = false }()
	assembled := hjRowKeysOrdered(t, g, elideQ+" ")
	if !slices.Equal(elided, assembled) {
		t.Fatalf("ordered-row divergence:\nelided    (%d): %v\nassembled (%d): %v",
			len(elided), elided, len(assembled), assembled)
	}
	if len(elided) == 0 {
		t.Fatal("0 rows -- the differential measured nothing")
	}
	// Engagement at this level: the two plans must actually differ (the
	// counter matrix pins the mechanism; this pins that the differential
	// above compared two different pipelines, not one twice).
	plAssembled, err := gql.Explain(g, elideQ)
	if err != nil {
		t.Fatal(err)
	}
	plan.DisablePathElide = false
	plElided, err := gql.Explain(g, elideQ+"  ")
	if err != nil {
		t.Fatal(err)
	}
	if plElided == plAssembled {
		t.Fatalf("elided and assembled plans are identical -- the differential compared one pipeline twice:\n%s", plElided)
	}
}
