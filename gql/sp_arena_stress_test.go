// Shortest-path slab-arena stress (task 203): the per-stage-run scratch
// hands result node/rel chains out of append-only slabs, and emitted
// rows RETAIN those sub-slices while the slabs keep advancing. The
// hazards under test: content corruption across slab boundaries (a path
// straddling the 4096-entry rung), the oversize branch (one path longer
// than a whole slab), aliasing between neighboring hand-outs, and
// retention validity after the stage ends and later work runs on the
// same snapshot.
package gql

import (
	"fmt"
	"strings"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// lineGraph is nodes 0..n-1 chained by R edges, ids as the `id` prop.
func lineGraph(t *testing.T, n int) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(n+2, n+2)
	prev := chickpeas.NodeID(0)
	for i := 0; i < n; i++ {
		nd, err := b.AddNode("N")
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SetProp(nd, "id", int64(i)); err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			if _, err := b.AddRel(prev, nd, "R"); err != nil {
				t.Fatal(err)
			}
		}
		prev = nd
	}
	return b.Finalize("line")
}

// TestSPPathSlabBoundaries drives many paths through one stage run so
// the node slab crosses its 4096 rung repeatedly, plus one path longer
// than a whole slab (the oversize branch). Every path's node list must
// read back exactly 0..k -- a slab handing out overlapping sub-slices
// would corrupt a neighbor.
func TestSPPathSlabBoundaries(t *testing.T) {
	const n = 5100
	g := lineGraph(t, n)
	// Rows k = 100, 200, ..., 5000: total path nodes ~130k, crossing the
	// 4096 slab dozens of times; the k=5000 row exceeds one slab alone.
	var ks []string
	for k := 100; k <= 5000; k += 100 {
		ks = append(ks, fmt.Sprintf("%d", k))
	}
	q := "FOR k IN [" + strings.Join(ks, ", ") + "] " +
		"MATCH (a:N {id: 0}) MATCH (b:N) WHERE b.id = k " +
		"MATCH p = ANY SHORTEST (a)-[:R]-{1,5100}(b) " +
		"RETURN k, length(p) AS len, nodes(p) AS ns"
	rows, err := RunUncached(g, q)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for r := range rows.All() {
		vals := r.Values()
		k, _ := vals[0].AsInt()
		l, _ := vals[1].AsInt()
		if l != k {
			t.Fatalf("k=%d: length %d", k, l)
		}
		ns, ok := vals[2].AsList()
		if !ok || int64(len(ns)) != k+1 {
			t.Fatalf("k=%d: nodes len %d, want %d", k, len(ns), k+1)
		}
		// The chain is built in id order from a fresh builder, so node id
		// == position; verify every element, not just the ends.
		for i, nv := range ns {
			id, ok := nv.AsNode()
			if !ok || int(id) != i {
				t.Fatalf("k=%d: nodes[%d] = %v, want node %d", k, i, nv, i)
			}
		}
		seen++
	}
	if seen != len(ks) {
		t.Fatalf("rows = %d, want %d", seen, len(ks))
	}
}

// TestSPPathRetentionAcrossQueries pins retention: paths captured from
// one query must stay intact while LATER queries run against the same
// snapshot (each stage owns fresh slabs; nothing may recycle retained
// memory).
func TestSPPathRetentionAcrossQueries(t *testing.T) {
	g := lineGraph(t, 600)
	capture := func(k int) []value.Value {
		q := fmt.Sprintf("MATCH (a:N {id: 0}) MATCH (b:N {id: %d}) MATCH p = ANY SHORTEST (a)-[:R]-{1,600}(b) RETURN nodes(p) AS ns", k)
		rows, err := RunUncached(g, q)
		if err != nil {
			t.Fatal(err)
		}
		for r := range rows.All() {
			ns, _ := r.Values()[0].AsList()
			out := make([]value.Value, len(ns))
			copy(out, ns)
			return out
		}
		t.Fatal("no row")
		return nil
	}
	held := capture(500)
	// Churn: more shortest-path work on the same snapshot, filling fresh
	// slabs. If any slab memory were recycled, held would corrupt.
	for k := 50; k <= 550; k += 50 {
		_ = capture(k)
	}
	for i, nv := range held {
		id, ok := nv.AsNode()
		if !ok || int(id) != i {
			t.Fatalf("retained path corrupted at [%d]: %v", i, nv)
		}
	}
}

// TestAllShortestSlabFlood pins the enumeration path's slab use: a
// ladder graph has 2^rungs equal-length corner-to-corner paths (capped
// by the engine at 1024); each enumerated path copies its suffix into
// the slab. Count and per-path validity (alternating rails, strictly
// advancing rungs) must survive the flood.
func TestAllShortestSlabFlood(t *testing.T) {
	const rungs = 12 // 2^12 = 4096 candidate paths, > the 1024 cap
	b := chickpeas.NewBuilder(64, 256)
	mk := func(id int64) chickpeas.NodeID {
		nd, err := b.AddNode("L")
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SetProp(nd, "id", id); err != nil {
			t.Fatal(err)
		}
		return nd
	}
	// Two rails a/b per level; every level-i node connects to both
	// level-i+1 nodes: 2^rungs shortest paths of equal length.
	type lvl struct{ a, b chickpeas.NodeID }
	levels := make([]lvl, rungs+1)
	for i := 0; i <= rungs; i++ {
		levels[i] = lvl{mk(int64(i * 2)), mk(int64(i*2 + 1))}
	}
	src := mk(9000)
	dst := mk(9001)
	rel := func(x, y chickpeas.NodeID) {
		if _, err := b.AddRel(x, y, "R"); err != nil {
			t.Fatal(err)
		}
	}
	rel(src, levels[0].a)
	rel(src, levels[0].b)
	for i := 0; i < rungs; i++ {
		rel(levels[i].a, levels[i+1].a)
		rel(levels[i].a, levels[i+1].b)
		rel(levels[i].b, levels[i+1].a)
		rel(levels[i].b, levels[i+1].b)
	}
	rel(levels[rungs].a, dst)
	rel(levels[rungs].b, dst)
	g := b.Finalize("ladder")

	q := "MATCH (s:L {id: 9000}) MATCH (d:L {id: 9001}) MATCH p = ALL SHORTEST (s)-[:R]-{1,20}(d) RETURN nodes(p) AS ns"
	rows, err := RunUncached(g, q)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for r := range rows.All() {
		ns, _ := r.Values()[0].AsList()
		if len(ns) != rungs+3 {
			t.Fatalf("path len %d, want %d", len(ns), rungs+3)
		}
		// Interior nodes must descend the levels in order: node id/2 ==
		// level index (rails interleave freely).
		for i := 1; i < len(ns)-1; i++ {
			id, _ := ns[i].AsNode()
			// Level nodes were created first: ids 0..2*rungs+1 in level order.
			if int(id)/2 != i-1 {
				t.Fatalf("path[%d] = node %d, not on level %d", i, id, i-1)
			}
		}
		count++
	}
	if count != 1024 {
		t.Fatalf("paths = %d, want the 1024 cap", count)
	}
}
