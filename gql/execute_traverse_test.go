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

func TestVarLengthUndirectedTrailUniqueness(t *testing.T) {
	g := replyForest(t)
	// Undirected {1,2} from a: root and c at 1 hop; b (via root) and the
	// (a<-c is one edge, trail-unique) at 2. Distinct endpoints:
	wantStrs(t, strCol(t, g,
		"MATCH (a:Msg {name: 'a'})-[:replyOf]-{1,2}(x) RETURN DISTINCT x.name AS name", "name"),
		"b", "c", "root")
}
