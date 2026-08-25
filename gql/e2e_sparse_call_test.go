// Sparse-graph family guard for the CALL procedures: real nodes at high
// ids must never drop from per-node results (the Rust sibling's mode-5
// family -- a kernel sized by node_count but indexed by raw id loses
// every real node above the count -- reappeared in the very function
// their family sweep was named after; the durable defense is a
// regression that fails on the FAMILY, not per-site fixes). Phantom-row
// assertions are deliberately absent: gap-id filtering is the posed
// existence-oracle decision, and this guard must keep passing before
// and after it lands.
package gql_test

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql"
)

func TestSparseCallRealNodesPresent(t *testing.T) {
	b := chickpeas.NewBuilder(8, 8)
	for _, id := range []uint32{0, 7, 1000, 5000} {
		if _, err := b.AddNodeWithID(id, "Thing"); err != nil {
			t.Fatal(err)
		}
	}
	// 0 -> 7 -> 5000 keeps the high-id node reachable so distance-style
	// procedures must produce a real value for it, not an unreached
	// sentinel that could mask an off-the-end read.
	if _, err := b.AddRel(0, 7, "R"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddRel(7, 5000, "R"); err != nil {
		t.Fatal(err)
	}
	if err := b.SetRelPropAt(0, "weight", 1.5); err != nil {
		t.Fatal(err)
	}
	if err := b.SetRelPropAt(1, "weight", 2.5); err != nil {
		t.Fatal(err)
	}
	g := b.Finalize()

	// Labeled control: the sparse fixture itself is sound.
	rows, err := gql.Run(g, "MATCH (n:Thing) RETURN count(n) AS c")
	if err != nil {
		t.Fatal(err)
	}
	for r := range rows.All() {
		v, _ := r.GetAt(0)
		if c, _ := v.AsInt(); c != 4 {
			t.Fatalf("labeled control count = %d, want 4", c)
		}
	}

	// Every per-node procedure must give the high-id real node a row.
	// Value spot-checks where the fixture pins one (sssp hops = 2,
	// weighted = 1.5+2.5, bfs hops = 2).
	procs := []struct {
		call string
		want float64 // spot value; -1 = presence only
	}{
		{"CALL wcc('R') YIELD node, component FILTER id(node) = 5000 RETURN component", -1},
		{"CALL algo.wcc() YIELD node, value FILTER id(node) = 5000 RETURN value", -1},
		{"CALL algo.bfs(0) YIELD node, value FILTER id(node) = 5000 RETURN value", 2},
		{"CALL algo.sssp(0) YIELD node, value FILTER id(node) = 5000 RETURN value", 2},
		{"CALL algo.sssp(0, true, true) YIELD node, value FILTER id(node) = 5000 RETURN value", 4},
		{"CALL algo.pagerank() YIELD node, value FILTER id(node) = 5000 RETURN value", -1},
		{"CALL algo.cdlp() YIELD node, value FILTER id(node) = 5000 RETURN value", -1},
		{"CALL algo.lcc() YIELD node, value FILTER id(node) = 5000 RETURN value", -1},
		// propagate is the traversal-row family (one row per REACHED
		// node), so its mode-5 exposure is kernel-internal sizing; the
		// reachable high-id node must appear at depth 3 (depths are seed-1-based).
		{"CALL algo.propagate([0], [9.0], 'R', 'out', 10, 'weight', 'asc', 0) YIELD node, depth FILTER id(node) = 5000 RETURN depth", 3},
	}
	for _, p := range procs {
		rows, err := gql.Run(g, p.call)
		if err != nil {
			t.Fatalf("%q: %v", p.call, err)
		}
		n := 0
		var got float64 = -1
		for r := range rows.All() {
			n++
			v, _ := r.GetAt(0)
			if f, ok := v.AsFloat(); ok {
				got = f
			} else if i, ok := v.AsInt(); ok {
				got = float64(i)
			}
		}
		if n != 1 {
			t.Fatalf("%q: rows = %d, want exactly 1 (a real high-id node dropped from a per-node procedure)", p.call, n)
		}
		if p.want >= 0 && got != p.want {
			t.Fatalf("%q: value = %v, want %v", p.call, got, p.want)
		}
	}
}

// TestSparsePhantomsExcluded pins the existence oracle across the two
// row paths (task 328, decision: retain + expose): a sparse builder
// graph's gap ids must never surface as rows -- the unlabeled scan and
// the CALL emission walk both consult NodeExists. Before the oracle,
// every one of these counted 5001 on this 4-node fixture.
func TestSparsePhantomsExcluded(t *testing.T) {
	b := chickpeas.NewBuilder(8, 0)
	for _, id := range []uint32{0, 7, 1000, 5000} {
		if _, err := b.AddNodeWithID(id, "Thing"); err != nil {
			t.Fatal(err)
		}
	}
	g := b.Finalize()

	counts := []struct {
		q    string
		want int64
	}{
		{"MATCH (n) RETURN count(n) AS c", 4},
		{"RETURN COUNT { (n) } AS c", 4},
		{"MATCH (n) WHERE id(n) >= 0 RETURN count(n) AS c", 4},
		{"MATCH (n:Thing) RETURN count(n) AS c", 4},
		{"CALL wcc('R') YIELD node, component RETURN count(node) AS c", 4},
		{"CALL wcc('R') YIELD node, component RETURN count(DISTINCT component) AS c", 4},
	}
	for _, tc := range counts {
		rows, err := gql.Run(g, tc.q)
		if err != nil {
			t.Fatalf("%q: %v", tc.q, err)
		}
		for r := range rows.All() {
			v, _ := r.GetAt(0)
			if c, _ := v.AsInt(); c != tc.want {
				t.Fatalf("%q = %d, want %d", tc.q, c, tc.want)
			}
		}
	}
	// Row emission, not just counts: exactly the four real nodes.
	rows, err := gql.Run(g, "MATCH (n) RETURN n")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range rows.All() {
		n++
	}
	if n != 4 {
		t.Fatalf("MATCH (n) rows = %d, want 4", n)
	}
}

// TestSparsePageRankMatchesDense is the value oracle for the N-dependent
// kernel repair: the same 4-node topology built dense (ids 0..3) and
// sparse (ids 0/7/1000/5000) must produce IDENTICAL ranks for
// corresponding real nodes -- id spacing alone must not move a rank.
func TestSparsePageRankMatchesDense(t *testing.T) {
	build := func(ids []uint32) *chickpeas.Snapshot {
		b := chickpeas.NewBuilder(8, 8)
		for _, id := range ids {
			if _, err := b.AddNodeWithID(id, "Thing"); err != nil {
				t.Fatal(err)
			}
		}
		// 0 -> 1 -> 2 -> 0 cycle plus a dangling 3 (by fixture position).
		for _, e := range [][2]int{{0, 1}, {1, 2}, {2, 0}, {0, 3}} {
			if _, err := b.AddRel(chickpeas.NodeID(ids[e[0]]), chickpeas.NodeID(ids[e[1]]), "R"); err != nil {
				t.Fatal(err)
			}
		}
		return b.Finalize()
	}
	dense := build([]uint32{0, 1, 2, 3})
	sparse := build([]uint32{0, 7, 1000, 5000})
	dr := dense.PageRank(true, 0.85, 20)
	sr := sparse.PageRank(true, 0.85, 20)
	sparseIDs := []uint32{0, 7, 1000, 5000}
	for i := range 4 {
		if dr[i] != sr[sparseIDs[i]] {
			t.Fatalf("rank[%d]: dense %v, sparse %v -- id spacing moved a rank", i, dr[i], sr[sparseIDs[i]])
		}
	}
	// Gap entries carry no rank.
	if sr[1] != 0 || sr[4999] != 0 {
		t.Fatalf("gap ids carry rank: sr[1]=%v sr[4999]=%v", sr[1], sr[4999])
	}
}
