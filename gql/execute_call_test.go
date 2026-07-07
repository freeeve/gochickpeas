// M19 tests: CALL procedures dispatching to the engine kernels -- per-node
// analytics cross-checked against the kernels directly, index-backed
// searches, and YIELD column binding -- under the dual-path harness.
package gql

import (
	"math"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

func TestCallWccComponents(t *testing.T) {
	g := socialGraph(t)
	// The KNOWS graph connects all four people into one undirected
	// component; the two companies are singletons.
	rows := runBoth(t, g,
		"CALL wcc('KNOWS') YIELD node, component RETURN node, component")
	comp := map[uint32]int64{}
	n := 0
	for r := range rows.All() {
		n++
		nv, _ := r.Get("node")
		id, _ := nv.AsNode()
		cv, _ := r.Get("component")
		c, _ := cv.AsInt()
		comp[uint32(id)] = c
	}
	if n != 6 {
		t.Fatalf("wcc rows = %d, want one per node", n)
	}
	if comp[0] != comp[1] || comp[1] != comp[2] || comp[2] != comp[3] {
		t.Fatalf("people should share one component: %v", comp)
	}
	if comp[4] == comp[0] || comp[5] == comp[0] || comp[4] == comp[5] {
		t.Fatalf("companies are singleton components: %v", comp)
	}
	// Aggregating over the yield: 3 distinct components.
	rows = runBoth(t, g,
		"CALL wcc('KNOWS') YIELD node, component RETURN count(DISTINCT component) AS n")
	r, _ := rows.Next()
	if v, _ := r.Get("n"); !value.Equal(v, value.Int(3)) {
		t.Fatalf("distinct components = %v", v)
	}
}

func TestCallPageRankMatchesKernel(t *testing.T) {
	g := socialGraph(t)
	want := g.PageRank(true, 0.85, 20)
	rows := runBoth(t, g,
		"CALL algo.pagerank(true, 0.85, 20) YIELD node, value RETURN node, value")
	n := 0
	for r := range rows.All() {
		nv, _ := r.Get("node")
		id, _ := nv.AsNode()
		vv, _ := r.Get("value")
		score, _ := vv.AsFloat()
		if math.Abs(score-want[id]) > 1e-12 {
			t.Fatalf("node %d score = %v, kernel says %v", id, score, want[id])
		}
		n++
	}
	if n != len(want) {
		t.Fatalf("pagerank rows = %d, want %d", n, len(want))
	}
}

func TestCallBfsDistances(t *testing.T) {
	g := socialGraph(t)
	// Directed BFS from Alice(0): 0 -> {1,2} -> {3}; companies via
	// WORKS_AT (any type matches): Acme at 1, Globex at 2.
	rows := runBoth(t, g,
		"CALL algo.bfs(0, true) YIELD node, value RETURN node, value")
	dist := map[uint32]int64{}
	for r := range rows.All() {
		nv, _ := r.Get("node")
		id, _ := nv.AsNode()
		vv, _ := r.Get("value")
		d, _ := vv.AsInt()
		dist[uint32(id)] = d
	}
	want := map[uint32]int64{0: 0, 1: 1, 2: 1, 3: 2, 4: 1, 5: 2}
	for id, w := range want {
		if dist[id] != w {
			t.Fatalf("dist[%d] = %d, want %d (all: %v)", id, dist[id], w, dist)
		}
	}
	// An unreachable node reads MaxInt64: BFS from Dave(3) never reaches
	// Globex(5) directly... it does via 3->0->2->5; use a company source
	// instead (companies have no outgoing rels).
	rows = runBoth(t, g,
		"CALL algo.bfs(4, true) YIELD node, value RETURN node, value")
	for r := range rows.All() {
		nv, _ := r.Get("node")
		id, _ := nv.AsNode()
		vv, _ := r.Get("value")
		d, _ := vv.AsInt()
		if id == 4 && d != 0 {
			t.Fatalf("self distance = %d", d)
		}
		if id != 4 && d != math.MaxInt64 {
			t.Fatalf("node %d should be unreachable from a sink, got %d", id, d)
		}
	}
}

func TestCallFtsSearch(t *testing.T) {
	g := socialGraph(t)
	wantStrs(t, strCol(t, g,
		"CALL fts.search('Person', 'name', 'alice') YIELD node MATCH (node) RETURN node.name AS name", "name"),
		"Alice")
	// A miss yields no rows.
	rows := runBoth(t, g,
		"CALL fts.search('Person', 'name', 'zebra') YIELD node RETURN node")
	if _, ok := rows.Next(); ok {
		t.Fatal("fts miss should yield nothing")
	}
}

// geoGraph is three labeled places: Paris, Versailles (~17km away), and
// Lyon (~390km away).
func geoGraph(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(4, 0)
	places := []struct {
		name     string
		lat, lon float64
	}{
		{"Paris", 48.8566, 2.3522},
		{"Versailles", 48.8049, 2.1204},
		{"Lyon", 45.7640, 4.8357},
	}
	for _, p := range places {
		id, _ := b.AddNode("Place")
		_ = b.SetProp(id, "name", p.name)
		_ = b.SetProp(id, "lat", p.lat)
		_ = b.SetProp(id, "lon", p.lon)
	}
	return b.Finalize("name")
}

func TestCallGeoProcedures(t *testing.T) {
	g := geoGraph(t)
	wantStrs(t, strCol(t, g,
		"CALL geo.withinRadius('Place', 'lat', 'lon', 48.8566, 2.3522, 30.0) YIELD node MATCH (node) RETURN node.name AS name", "name"),
		"Paris", "Versailles")
	wantStrs(t, strCol(t, g,
		"CALL geo.withinBBox('Place', 'lat', 'lon', 45.0, 4.0, 46.5, 5.5) YIELD node MATCH (node) RETURN node.name AS name", "name"),
		"Lyon")
}

// flowGraph is the little money-flow fixture: s pays a (5) and b (3);
// a pays c (7), b pays c (2). Node ids follow insertion: s=0 a=1 b=2 c=3.
func flowGraph(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(8, 8)
	names := []string{"s", "a", "b", "c"}
	ids := make([]chickpeas.NodeID, len(names))
	for i, n := range names {
		id, err := b.AddNode("Acct")
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
		_ = b.SetProp(id, "name", n)
	}
	rel := func(u, v int, amt float64) {
		t.Helper()
		pos, err := b.AddRel(ids[u], ids[v], "flow")
		if err != nil {
			t.Fatal(err)
		}
		_ = b.SetRelPropAt(pos, "amt", amt)
	}
	rel(0, 1, 5)
	rel(0, 2, 3)
	rel(1, 3, 7)
	rel(2, 3, 2)
	return b.Finalize("name")
}

// propagateRows drains node-name/value/depth triples keyed by name.
func propagateRows(t *testing.T, rows *Rows) map[string][2]int64 {
	t.Helper()
	names := []string{"s", "a", "b", "c"}
	out := map[string][2]int64{}
	for r := range rows.All() {
		nv, _ := r.Get("n")
		id, _ := nv.AsNode()
		vv, _ := r.Get("v")
		f, _ := vv.AsFloat()
		dv, _ := r.Get("dp")
		d, _ := dv.AsInt()
		out[names[id]] = [2]int64{int64(f), d}
	}
	return out
}

func TestCallPropagateStatic(t *testing.T) {
	g := flowGraph(t)
	// Seeds a(5) and b(3) at depth 1; each seed's run claims c through its
	// own edge (7 and 2), so c accumulates 9 at depth 2.
	rows := runBoth(t, g,
		"CALL algo.propagate([1, 2], [5.0, 3.0], 'flow', 'out', 2, 'amt', 'asc', 0) YIELD node AS n, value AS v, depth AS dp RETURN n, v, dp")
	got := propagateRows(t, rows)
	want := map[string][2]int64{"a": {5, 1}, "b": {3, 1}, "c": {9, 2}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Fatalf("%s: got %v want %v", k, got[k], w)
		}
	}
}

func TestCallPropagateCorrelated(t *testing.T) {
	g := flowGraph(t)
	// The CR8 shape: bind the seed rels upstream, feed the collected nodes
	// and amounts through the correlated call, and keep an incoming column
	// (k) alive across the cross-join.
	rows := runBoth(t, g, `MATCH (root:Acct {name: 's'})-[d:flow]->(x)
RETURN collect(x) AS seeds, collect(d.amt) AS vals, 100 AS k
NEXT
CALL algo.propagate(seeds, vals, 'flow', 'out', 2, 'amt', 'asc', 0) YIELD node AS n, value AS v, depth AS dp
RETURN n, v, dp, k`)
	got := map[string][3]int64{}
	names := []string{"s", "a", "b", "c"}
	for r := range rows.All() {
		nv, _ := r.Get("n")
		id, _ := nv.AsNode()
		vv, _ := r.Get("v")
		f, _ := vv.AsFloat()
		dv, _ := r.Get("dp")
		d, _ := dv.AsInt()
		kv, _ := r.Get("k")
		k, _ := kv.AsInt()
		got[names[id]] = [3]int64{int64(f), d, k}
	}
	want := map[string][3]int64{"a": {5, 1, 100}, "b": {3, 1, 100}, "c": {9, 2, 100}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Fatalf("%s: got %v want %v", k, got[k], w)
		}
	}
}

func TestCallCorrelatedBfsAndRuntimeMismatch(t *testing.T) {
	g := flowGraph(t)
	// A bound node as a procedure argument (previously only literal ids).
	rows := runBoth(t, g,
		"MATCH (p:Acct {name: 's'}) CALL algo.bfs(p, true) YIELD node, value RETURN node, value")
	dist := map[uint32]int64{}
	for r := range rows.All() {
		nv, _ := r.Get("node")
		id, _ := nv.AsNode()
		vv, _ := r.Get("value")
		d, _ := vv.AsInt()
		dist[uint32(id)] = d
	}
	want := map[uint32]int64{0: 0, 1: 1, 2: 1, 3: 2}
	for id, w := range want {
		if dist[id] != w {
			t.Fatalf("dist[%d] = %d, want %d (all: %v)", id, dist[id], w, dist)
		}
	}
	// A row whose evaluated arguments fail validation yields no rows
	// (total-eval semantics), not an error.
	rows = runBoth(t, g, `MATCH (p:Acct {name: 's'}) LET bad = p.name
CALL algo.propagate(bad, 1.0, 'flow', 'out', 2, 'amt', 'asc', 0) YIELD node RETURN node`)
	if _, ok := rows.Next(); ok {
		t.Fatal("invalid runtime args should yield no rows")
	}
}

func TestCallPropagateYieldAndArgErrors(t *testing.T) {
	g := flowGraph(t)
	for _, q := range []string{
		"CALL wcc('flow') YIELD node, depth RETURN node",
		"CALL algo.propagate([1], [1.0], 'flow', 'out', 2, 'amt', 'sideways', 0) YIELD node RETURN node",
		"CALL algo.propagate([1], [1.0, 2.0], 'flow', 'out', 2, 'amt', 'asc', 0) YIELD node RETURN node",
		"MATCH (p:Acct) CALL algo.propagate(p, count(p), 'flow', 'out', 2, 'amt', 'asc', 0) YIELD node RETURN node",
	} {
		if _, err := Run(g, q); err == nil {
			t.Fatalf("expected plan error: %s", q)
		}
	}
}

func TestCallCdlpAndLcc(t *testing.T) {
	g := socialGraph(t)
	// CDLP labels agree with the kernel seeded by dense ids.
	init := make([]uint32, g.CSRIDSpace())
	for i := range init {
		init[i] = uint32(i)
	}
	want := g.CDLPSeeded(false, 10, init)
	rows := runBoth(t, g, "CALL algo.cdlp(false, 10) YIELD node, value RETURN node, value")
	for r := range rows.All() {
		nv, _ := r.Get("node")
		id, _ := nv.AsNode()
		vv, _ := r.Get("value")
		c, _ := vv.AsInt()
		if c != int64(want[id]) {
			t.Fatalf("cdlp[%d] = %d, kernel says %d", id, c, want[id])
		}
	}
	// LCC agrees with the kernel.
	wantL := g.LCC(false)
	rows = runBoth(t, g, "CALL algo.lcc(false) YIELD node, value RETURN node, value")
	for r := range rows.All() {
		nv, _ := r.Get("node")
		id, _ := nv.AsNode()
		vv, _ := r.Get("value")
		f, _ := vv.AsFloat()
		if math.Abs(f-wantL[id]) > 1e-12 {
			t.Fatalf("lcc[%d] = %v, kernel says %v", id, f, wantL[id])
		}
	}
}
