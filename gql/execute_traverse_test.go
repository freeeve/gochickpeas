// M17 traversal tests: expand (incl. semijoin rebinds and named rels),
// quantified paths (GQL: {m,n}, + is {1,}, * is {0,}), OPTIONAL MATCH,
// named paths, and ANY/ALL SHORTEST -- the Rust execute.rs expansion
// subset translated to GQL under the dual-path harness.
package gql

import (
	"errors"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// replyForest is the Rust fixture: root <- a, root <- b, a <- c (replyOf
// points child -> parent).
func replyForest(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(4, 4)
	for _, n := range []string{"root", "a", "b", "c"} {
		id, _ := b.AddNode("Msg")
		_ = b.SetProp(id, "name", n)
	}
	for _, e := range [][2]chickpeas.NodeID{{1, 0}, {2, 0}, {3, 1}} {
		if _, err := b.AddRel(e[0], e[1], "replyOf"); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize("name")
}

func TestExpandOutgoing(t *testing.T) {
	g := socialGraph(t)
	wantStrs(t, strCol(t, g,
		"MATCH (p:Person {name: 'Alice'})-[:KNOWS]->(f:Person) RETURN f.name AS name", "name"),
		"Bob", "Carol")
}

func TestExpandIncomingAndChained(t *testing.T) {
	g := socialGraph(t)
	// Incoming expand (aggregation lands in M18, so assert row-level).
	wantStrs(t, strCol(t, g,
		"MATCH (c:Company {name: 'Acme'})<-[:WORKS_AT]-(p:Person) RETURN p.name AS name", "name"),
		"Alice", "Bob")
	// Two hops chained in one pattern.
	wantStrs(t, strCol(t, g,
		"MATCH (a:Person {name: 'Alice'})-[:KNOWS]->(f)-[:WORKS_AT]->(c:Company) RETURN c.name AS name", "name"),
		"Acme", "Globex")
}

func TestCarriedNodeAndMultipleMatchClauses(t *testing.T) {
	g := socialGraph(t)
	// Carry the node across a projection boundary, then expand from it.
	wantStrs(t, strCol(t, g,
		"MATCH (p:Person {name: 'Alice'}) RETURN p AS p NEXT MATCH (p)-[:KNOWS]->(f:Person) RETURN f.name AS name", "name"),
		"Bob", "Carol")
	// Two MATCH clauses in one segment: the second anchors on a.
	wantStrs(t, strCol(t, g,
		"MATCH (a:Person {name: 'Alice'}) MATCH (a)-[:KNOWS]->(f:Person) RETURN f.name AS name", "name"),
		"Bob", "Carol")
}

func TestSemijoinRebind(t *testing.T) {
	g := socialGraph(t)
	// Both endpoints bound before the edge test: the rebind expand probes
	// the memoized reverse-neighbor set.
	wantStrs(t, strCol(t, g,
		"MATCH (a:Person {name: 'Alice'}), (b:Person {name: 'Bob'}) MATCH (a)-[:KNOWS]->(b) RETURN b.name AS name", "name"),
		"Bob")
	// A missing edge yields nothing (Dave does not know Bob).
	if got := strCol(t, g,
		"MATCH (a:Person {name: 'Dave'}), (b:Person {name: 'Bob'}) MATCH (a)-[:KNOWS]->(b) RETURN b.name AS name", "name"); len(got) != 0 {
		t.Fatalf("missing edge matched: %v", got)
	}
}

func TestNamedRelationshipVariable(t *testing.T) {
	g := socialGraph(t)
	rows := runBoth(t, g,
		"MATCH (a:Person {name: 'Alice'})-[r:KNOWS]->(b) RETURN id(startNode(r)) AS s, b.name AS name ORDER BY name")
	var starts []int64
	for r := range rows.All() {
		v, _ := r.Get("s")
		i, _ := v.AsInt()
		starts = append(starts, i)
	}
	if len(starts) != 2 || starts[0] != 0 || starts[1] != 0 {
		t.Fatalf("startNode ids = %v", starts)
	}
}

func TestQuantifiedPaths(t *testing.T) {
	g := socialGraph(t)
	// {1,2}: Bob, Carol at 1 hop; Dave (and Bob/Carol again) at 2.
	wantStrs(t, strCol(t, g,
		"MATCH (p:Person {name: 'Alice'})-[:KNOWS]->{1,2}(f:Person) RETURN DISTINCT f.name AS name", "name"),
		"Bob", "Carol", "Dave")
	// {1,1} collapses to direct neighbors.
	wantStrs(t, strCol(t, g,
		"MATCH (p:Person {name: 'Alice'})-[:KNOWS]->{1,1}(f:Person) RETURN DISTINCT f.name AS name", "name"),
		"Bob", "Carol")
	// + is {1,}: unbounded reachable set dedups, so the KNOWS cycle
	// terminates and reaches everyone including back to Alice.
	wantStrs(t, strCol(t, g,
		"MATCH (a:Person {name: 'Alice'})-[:KNOWS]->+(b) RETURN DISTINCT b.name AS name", "name"),
		"Alice", "Bob", "Carol", "Dave")
}

func TestZeroLengthQuantifiers(t *testing.T) {
	g := replyForest(t)
	// * is {0,}: includes the node itself plus every ancestor to the root.
	wantStrs(t, strCol(t, g,
		"MATCH (c:Msg {name: 'c'})-[:replyOf]->*(x) RETURN x.name AS name", "name"),
		"a", "c", "root")
	// Reversed and unbounded: the whole thread, self included.
	wantStrs(t, strCol(t, g,
		"MATCH (root:Msg {name: 'root'})<-[:replyOf]-*(x) RETURN x.name AS name", "name"),
		"a", "b", "c", "root")
	// + excludes the zero-length hop in an acyclic forest.
	wantStrs(t, strCol(t, g,
		"MATCH (c:Msg {name: 'c'})-[:replyOf]->+(x) RETURN x.name AS name", "name"),
		"a", "root")
}

func TestRelVarOnReachableSetIsPlanError(t *testing.T) {
	g := replyForest(t)
	// A reachable set has no per-path relationship list, so a named rel
	// variable on {0,} / unbounded is rejected at plan time.
	for _, q := range []string{
		"MATCH (c:Msg {name: 'c'})-[e:replyOf]->*(x) RETURN x.name AS name",
		"MATCH (c:Msg {name: 'c'})-[e:replyOf]->+(x) RETURN x.name AS name",
	} {
		if _, err := Run(g, q); !errors.Is(err, ErrPlan) {
			t.Fatalf("expected a plan error for %s, got %v", q, err)
		}
	}
}

func TestNamedVarLengthRelList(t *testing.T) {
	g := socialGraph(t)
	// A bounded quantified hop with a named rel binds the trail's rel
	// list; one row per trail with size(e) = its hop count.
	rows := runBoth(t, g,
		"MATCH (a:Person {name: 'Alice'})-[e:KNOWS]->{1,2}(f:Person {name: 'Dave'}) RETURN size(e) AS hops")
	var hops []int64
	for r := range rows.All() {
		v, _ := r.Get("hops")
		h, _ := v.AsInt()
		hops = append(hops, h)
	}
	// Two 2-hop trails reach Dave (via Bob and via Carol).
	if len(hops) != 2 || hops[0] != 2 || hops[1] != 2 {
		t.Fatalf("trail hop counts = %v", hops)
	}
}

func TestOptionalMatch(t *testing.T) {
	g := socialGraph(t)
	rows := runBoth(t, g,
		"MATCH (p:Person) OPTIONAL MATCH (p)-[:WORKS_AT]->(c:Company) RETURN p.name AS name, c.name AS company ORDER BY name")
	want := map[string]string{"Alice": "Acme", "Bob": "Acme", "Carol": "Globex", "Dave": ""}
	n := 0
	for r := range rows.All() {
		n++
		nv, _ := r.Get("name")
		name, _ := nv.AsStr()
		cv, _ := r.Get("company")
		if want[name] == "" {
			if !cv.IsNull() {
				t.Fatalf("%s should have a null company, got %v", name, cv)
			}
			continue
		}
		if c, _ := cv.AsStr(); c != want[name] {
			t.Fatalf("%s company = %v, want %s", name, cv, want[name])
		}
	}
	if n != 4 {
		t.Fatalf("rows = %d", n)
	}
	// OPTIONAL over an impossible pattern still emits every input row.
	rows = runBoth(t, g,
		"MATCH (p:Person {name: 'Dave'}) OPTIONAL MATCH (p)-[:WORKS_AT]->(c) RETURN c AS c")
	r, ok := rows.Next()
	if !ok {
		t.Fatal("optional no-match must emit the input row")
	}
	if v, _ := r.Get("c"); !v.IsNull() {
		t.Fatalf("unmatched optional var = %v", v)
	}
}

func TestNamedPathFixedHop(t *testing.T) {
	g := socialGraph(t)
	rows := runBoth(t, g,
		"MATCH p = (a:Person {name: 'Alice'})-[:KNOWS]->(b:Person) RETURN size(nodes(p)) AS n, length(p) AS l")
	count := 0
	for r := range rows.All() {
		count++
		nv, _ := r.Get("n")
		lv, _ := r.Get("l")
		if n, _ := nv.AsInt(); n != 2 {
			t.Fatalf("nodes = %d", n)
		}
		if l, _ := lv.AsInt(); l != 1 {
			t.Fatalf("length = %d", l)
		}
	}
	if count != 2 {
		t.Fatalf("paths = %d", count)
	}
	// A WHERE over the bound path runs post-assembly.
	rows = runBoth(t, g,
		"MATCH p = (a:Person {name: 'Alice'})-[:KNOWS]->{1,2}(b:Person {name: 'Dave'}) WHERE length(p) = 2 RETURN length(p) AS l")
	if batch := rows.NextBatch(10); len(batch) != 2 {
		t.Fatalf("filtered paths = %d", len(batch))
	}
}

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

func TestVarLengthUndirectedTrailUniqueness(t *testing.T) {
	g := replyForest(t)
	// Undirected {1,2} from a: root and c at 1 hop; b (via root) and the
	// (a<-c is one edge, trail-unique) at 2. Distinct endpoints:
	wantStrs(t, strCol(t, g,
		"MATCH (a:Msg {name: 'a'})-[:replyOf]-{1,2}(x) RETURN DISTINCT x.name AS name", "name"),
		"b", "c", "root")
}

// pingPong is two nodes with one R edge each way -- a 2-hop trail
// a->b->a exists (two distinct rels) but revisits a, so ACYCLIC prunes
// it while TRAIL keeps it.
func pingPong(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(2, 2)
	for _, n := range []string{"a", "b"} {
		id, _ := b.AddNode("Msg")
		_ = b.SetProp(id, "name", n)
	}
	for _, e := range [][2]chickpeas.NodeID{{0, 1}, {1, 0}} {
		if _, err := b.AddRel(e[0], e[1], "R"); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize("name")
}

// TestExistsQuantifiedHop: a quantified rel inside EXISTS/COUNT resolves
// the reachable set (min 0 includes the anchor itself), not a single hop
// (IC12's shape: EXISTS { (tc)-[:IS_SUBCLASS_OF]->{0,10}(:X {..}) }).
func TestExistsQuantifiedHop(t *testing.T) {
	g := replyForest(t)
	// root at 0 hops, a and b at 1, c at 2 -- everything reaches root.
	wantStrs(t, strCol(t, g,
		"MATCH (x:Msg) WHERE EXISTS { (x)-[:replyOf]->{0,2}(:Msg {name: 'root'}) } RETURN x.name AS name", "name"),
		"a", "b", "c", "root")
	wantStrs(t, strCol(t, g,
		"MATCH (x:Msg) WHERE EXISTS { (x)-[:replyOf]->{2,2}(:Msg {name: 'root'}) } RETURN x.name AS name", "name"),
		"c")
}

// TestDurationISOString: duration('P100D') parses the ISO-8601 string
// form and adds calendar-correctly to a zoned datetime.
func TestDurationISOString(t *testing.T) {
	g := replyForest(t)
	got := intCol(t, g,
		"RETURN (zoned_datetime('2012-06-01') + duration('P100D')).epochMillis AS ms", "ms")
	if len(got) != 1 || got[0] != 1347148800000 {
		t.Fatalf("2012-06-01 + P100D = %v, want 1347148800000 (2012-09-09)", got)
	}
	got2 := intCol(t, g,
		"RETURN (zoned_datetime('2020-01-31') + duration('P1M'))"+
			".epochMillis AS ms NEXT LET d = zoned_datetime(ms) RETURN d.day AS day", "day")
	if len(got2) != 1 || got2[0] != 29 {
		t.Fatalf("2020-01-31 + P1M day = %v, want 29 (clamped leap February)", got2)
	}
	got3 := intCol(t, g,
		"RETURN (zoned_datetime('2012-06-01T00:00:00') + duration('PT12H30M')).epochMillis AS ms", "ms")
	if len(got3) != 1 || got3[0] != 1338508800000+12*3_600_000+30*60_000 {
		t.Fatalf("PT12H30M = %v", got3)
	}
}

func TestAcyclicPathMode(t *testing.T) {
	g := pingPong(t)
	// Trail semantics (bare and with the no-op TRAIL prefix): the 2-hop
	// round trip lands back on a.
	wantStrs(t, strCol(t, g,
		"MATCH (a:Msg {name: 'a'})-[:R]->{2,2}(x) RETURN x.name AS name", "name"), "a")
	wantStrs(t, strCol(t, g,
		"MATCH TRAIL (a:Msg {name: 'a'})-[:R]->{2,2}(x) RETURN x.name AS name", "name"), "a")
	// ACYCLIC rejects the revisit -- both bare and path-bind positions.
	wantStrs(t, strCol(t, g,
		"MATCH ACYCLIC (a:Msg {name: 'a'})-[:R]->{2,2}(x) RETURN x.name AS name", "name"))
	wantStrs(t, strCol(t, g,
		"MATCH ACYCLIC (a:Msg {name: 'a'})-[:R]->{1,2}(x) RETURN x.name AS name", "name"), "b")
	wantStrs(t, strCol(t, g,
		"MATCH p = ACYCLIC (a:Msg {name: 'a'})-[:R]->{1,2}(x) RETURN x.name AS name", "name"), "b")
	// ACYCLIC needs bounded, min >= 1 quantifiers (reach mode has no
	// per-path node stack).
	if _, err := Run(g, "MATCH ACYCLIC (a:Msg {name: 'a'})-[:R]->{0,}(x) RETURN x.name AS name"); !errors.Is(err, ErrPlan) {
		t.Fatalf("unbounded ACYCLIC: %v", err)
	}
}
