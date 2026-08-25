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
