package gql

import (
	"errors"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// Shortest-path execution tests: ANY/ALL SHORTEST selection, weighted COST
// search (property/constant/formula weights and their errors), the
// constant-cost = unweighted equivalence, and the hop-bound exclusion.
// Split from execute_traverse_test.go (which keeps the shared graph
// fixtures/helpers and the expand/quantifier/path-mode tests).

func TestAnyShortest(t *testing.T) {
	g := socialGraph(t)
	rows := runBoth(t, g,
		"MATCH (a:Person {name: 'Alice'}), (b:Person {name: 'Dave'}) MATCH p = ANY SHORTEST (a)-[:KNOWS]->{1,}(b) RETURN length(p) AS l")
	r, ok := rows.Next()
	if !ok {
		t.Fatal("no shortest path")
	}
	if l, _ := func() (int64, bool) { v, _ := r.Get("l"); return v.AsInt() }(); l != 2 {
		t.Fatalf("shortest length = %d, want 2", l)
	}
	if _, more := rows.Next(); more {
		t.Fatal("ANY SHORTEST binds one path per row")
	}
	// Same endpoints: a zero-length path.
	rows = runBoth(t, g,
		"MATCH (a:Person {name: 'Alice'}) MATCH p = ANY SHORTEST (a)-[:KNOWS]->{1,}(a) RETURN length(p) AS l")
	r, _ = rows.Next()
	if v, _ := r.Get("l"); !value.Equal(v, value.Int(0)) {
		t.Fatalf("self path length = %v", v)
	}
}

func TestAllShortestDiamond(t *testing.T) {
	g := socialGraph(t)
	// The directed diamond Alice -> {Bob, Carol} -> Dave has two 2-hop
	// minimum paths; ALL SHORTEST is row-expanding.
	rows := runBoth(t, g,
		"MATCH (a:Person {name: 'Alice'}), (b:Person {name: 'Dave'}) MATCH p = ALL SHORTEST (a)-[:KNOWS]->{1,}(b) RETURN length(p) AS l")
	batch := rows.NextBatch(10)
	if len(batch) != 2 {
		t.Fatalf("all-shortest rows = %d, want 2", len(batch))
	}
	for _, r := range batch {
		if v, _ := r.Get("l"); !value.Equal(v, value.Int(2)) {
			t.Fatalf("path length = %v", v)
		}
	}
}

// weightedTriangle: s -[w=10]-> t direct, s -[w=1]-> m -[w=1]-> t detour
// -- the cheapest route has more hops than the shortest one.
func weightedTriangle(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(4, 4)
	for _, n := range []string{"s", "m", "t"} {
		id, _ := b.AddNode("N")
		_ = b.SetProp(id, "name", n)
	}
	for _, e := range []struct {
		u, v chickpeas.NodeID
		w    float64
	}{{0, 2, 10}, {0, 1, 1}, {1, 2, 1}} {
		if _, err := b.AddRel(e.u, e.v, "R"); err != nil {
			t.Fatal(err)
		}
		if err := b.SetRelProp(e.u, e.v, "R", "w", e.w); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize("name")
}

func TestAnyShortestCost(t *testing.T) {
	g := weightedTriangle(t)
	ends := "MATCH (a:N {name: 's'}), (b:N {name: 't'}) "
	lengthOf := func(q string) int64 {
		rows := runBoth(t, g, q)
		r, ok := rows.Next()
		if !ok {
			t.Fatalf("no path: %s", q)
		}
		l, _ := func() (int64, bool) { v, _ := r.Get("l"); return v.AsInt() }()
		if _, more := rows.Next(); more {
			t.Fatalf("more than one path row: %s", q)
		}
		return l
	}
	// A property weight prefers the cheap 2-hop detour over the direct edge.
	if l := lengthOf(ends + "MATCH p = ANY SHORTEST (a)-[r:R]->{1,}(b) COST r.w RETURN length(p) AS l"); l != 2 {
		t.Fatalf("property-cost length = %d, want 2", l)
	}
	// A constant weight makes every edge equal: hop-minimal, the direct edge.
	if l := lengthOf(ends + "MATCH p = ANY SHORTEST (a)-[:R]->{1,}(b) COST 1 RETURN length(p) AS l"); l != 1 {
		t.Fatalf("constant-cost length = %d, want 1", l)
	}
	// A per-edge formula scales uniformly: the detour still wins.
	if l := lengthOf(ends + "MATCH p = ANY SHORTEST (a)-[r:R]->{1,}(b) COST r.w * 2 RETURN length(p) AS l"); l != 2 {
		t.Fatalf("formula-cost length = %d, want 2", l)
	}
	// relationships(p) reflects the exact edges the search optimized.
	rows := runBoth(t, g, ends+"MATCH p = ANY SHORTEST (a)-[r:R]->{1,}(b) COST r.w RETURN size(rels(p)) AS n")
	r, _ := rows.Next()
	if v, _ := r.Get("n"); !value.Equal(v, value.Int(2)) {
		t.Fatalf("rels(p) size = %v, want 2", v)
	}
}

func TestAnyShortestCostErrors(t *testing.T) {
	g := weightedTriangle(t)
	ends := "MATCH (a:N {name: 's'}), (b:N {name: 't'}) "
	// ALL SHORTEST does not combine with COST.
	if _, err := Run(g, ends+"MATCH p = ALL SHORTEST (a)-[r:R]->{1,}(b) COST r.w RETURN length(p) AS l"); !errors.Is(err, ErrPlan) {
		t.Fatalf("ALL SHORTEST + COST: %v", err)
	}
	// A weight formula may reference only the pattern's rel variable.
	if _, err := Run(g, ends+"MATCH p = ANY SHORTEST (a)-[r:R]->{1,}(b) COST a.w RETURN length(p) AS l"); !errors.Is(err, ErrPlan) {
		t.Fatalf("foreign-var COST: %v", err)
	}
	// An unknown function inside the weight is a bind error, not null.
	if _, err := Run(g, ends+"MATCH p = ANY SHORTEST (a)-[r:R]->{1,}(b) COST nosuchfn(r) RETURN length(p) AS l"); !errors.Is(err, ErrBind) {
		t.Fatalf("unknown-fn COST: %v", err)
	}
	// A per-edge formula needs a named relationship variable.
	if _, err := Run(g, ends+"MATCH p = ANY SHORTEST (a)-[:R]->{1,}(b) COST r.w RETURN length(p) AS l"); !errors.Is(err, ErrPlan) {
		t.Fatalf("unnamed-rel COST: %v", err)
	}
	// COST applies only to a path search, not a plain path bind.
	if _, err := Run(g, "MATCH p = (a:N)-[r:R]->(b) COST r.w RETURN length(p) AS l"); !errors.Is(err, ErrParse) {
		t.Fatalf("path-bind COST: %v", err)
	}
}

// TestConstantCostMatchesUnweighted pins the constant-weight dispatch: a
// constant (or degraded-to-unit) COST makes every path's cost proportional
// to its hops, so the search must agree with the plain minimum-hop form on
// existence and length -- for every constant, including zero and an
// invalid negative (which degrades to unit weights).
func TestConstantCostMatchesUnweighted(t *testing.T) {
	g := weightedTriangle(t)
	ends := "MATCH (a:N {name: 's'}), (b:N {name: 't'}) "
	lengthOf := func(q string) int64 {
		t.Helper()
		rows := runBoth(t, g, q)
		r, ok := rows.Next()
		if !ok {
			t.Fatalf("no path: %s", q)
		}
		v, _ := r.Get("l")
		l, _ := v.AsInt()
		return l
	}
	want := lengthOf(ends + "MATCH p = ANY SHORTEST (a)-[:R]->{1,}(b) RETURN length(p) AS l")
	for _, cost := range []string{"COST 5", "COST 0", "COST -1"} {
		q := ends + "MATCH p = ANY SHORTEST (a)-[:R]->{1,}(b) " + cost + " RETURN length(p) AS l"
		if l := lengthOf(q); l != want {
			t.Fatalf("%s: length %d, want %d (the unweighted answer)", cost, l, want)
		}
	}
}

// TestShortestHopBoundExcludes pins the hop cap for both search kinds: a
// target strictly beyond the bound must yield NO path -- not a truncated
// one -- and the OPTIONAL form must keep the row with a null path. The
// cheap detour makes the weighted answer 2 hops, so a {1,1} cap excludes
// the pair for the property-weighted search exactly when it prefers the
// longer route.
func TestShortestHopBoundExcludes(t *testing.T) {
	// chain: s -> m -> t (no direct edge), so t sits 2 hops from s.
	b := chickpeas.NewBuilder(4, 4)
	for _, n := range []string{"s", "m", "t"} {
		id, _ := b.AddNode("N")
		_ = b.SetProp(id, "name", n)
	}
	for _, e := range [][2]chickpeas.NodeID{{0, 1}, {1, 2}} {
		if _, err := b.AddRel(e[0], e[1], "R"); err != nil {
			t.Fatal(err)
		}
		if err := b.SetRelProp(e[0], e[1], "R", "w", 1.0); err != nil {
			t.Fatal(err)
		}
	}
	g := b.Finalize("name")
	ends := "MATCH (a:N {name: 's'}), (b:N {name: 't'}) "
	for _, form := range []string{
		"MATCH p = ANY SHORTEST (a)-[:R]->{1,1}(b) RETURN length(p) AS l",
		"MATCH p = ANY SHORTEST (a)-[r:R]->{1,1}(b) COST r.w RETURN length(p) AS l",
		"MATCH p = ALL SHORTEST (a)-[:R]->{1,1}(b) RETURN length(p) AS l",
	} {
		rows := runBoth(t, g, ends+form)
		if r, ok := rows.Next(); ok {
			t.Fatalf("beyond-bound target produced a row %v: %s", r.Values(), form)
		}
	}
	// OPTIONAL keeps the row, path null.
	rows := runBoth(t, g, ends+"OPTIONAL MATCH p = ANY SHORTEST (a)-[:R]->{1,1}(b) RETURN p")
	r, ok := rows.Next()
	if !ok {
		t.Fatal("OPTIONAL beyond-bound dropped the row")
	}
	if v, _ := r.Get("p"); !v.IsNull() {
		t.Fatalf("OPTIONAL beyond-bound path = %v, want null", v)
	}
	// Within the bound, both kinds find the 2-hop path.
	for _, form := range []string{
		"MATCH p = ANY SHORTEST (a)-[:R]->{1,2}(b) RETURN length(p) AS l",
		"MATCH p = ANY SHORTEST (a)-[r:R]->{1,2}(b) COST r.w RETURN length(p) AS l",
	} {
		rows := runBoth(t, g, ends+form)
		r, ok := rows.Next()
		if !ok {
			t.Fatalf("within-bound target found no path: %s", form)
		}
		if v, _ := r.Get("l"); !value.Equal(v, value.Int(2)) {
			t.Fatalf("within-bound length = %v, want 2: %s", v, form)
		}
	}
}
