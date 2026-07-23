package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// TestNodeIDSeekValue covers the id-seek value resolution: a non-negative
// integer within the CSR id space resolves (including a sparse high id with
// no node present -- the id space, not the node count, is the bound), while
// a negative id, an out-of-space id, and a non-integer all decline.
func TestNodeIDSeekValue(t *testing.T) {
	// One node at id 10 -> the id space spans 0..10 with a single node, so
	// the space exceeds the node count.
	b := chickpeas.NewBuilder(16, 0)
	if _, err := b.AddNodeWithID(10, "N"); err != nil {
		t.Fatal(err)
	}
	sg := graph.New(b.Finalize("idseek"))
	ctx := &eval.Ctx{G: sg}
	space := int64(sg.IDSpace())
	if space <= 10 {
		t.Fatalf("id space = %d, want > 10", space)
	}

	// In-space ids resolve, including a sparse id with no node behind it
	// (existence is the caller's job; the seek only bounds-checks).
	for _, id := range []int64{0, 5, 10, space - 1} {
		got, ok := nodeIDSeekValue(ctx, value.Int(id))
		if !ok || got != graph.NodeID(id) {
			t.Fatalf("id %d = %d,%v, want %d,true", id, got, ok, id)
		}
	}

	// A negative id, an id at/beyond the space, and a non-integer decline.
	for _, v := range []value.Value{
		value.Int(-1),
		value.Int(space),
		value.Int(space + 1000),
		value.Str("7"),
		value.Float(3.0),
		value.Null(),
	} {
		if _, ok := nodeIDSeekValue(ctx, v); ok {
			t.Fatalf("%+v should not resolve to an id seek", v)
		}
	}
}

// TestExistsSeedCandidates covers the EXISTS-seed backward candidate walk:
// from the bound anchor it enumerates the nodes reachable over each seed
// chain's hops, filtered by the per-level and final matchers, and reports
// success. Fixture: anchor 0 with outgoing R edges to 1, 2, 3.
func TestExistsSeedCandidates(t *testing.T) {
	bld := chickpeas.NewBuilder(8, 8)
	for range 4 {
		if _, err := bld.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []int{1, 2, 3} {
		if _, err := bld.AddRel(graph.NodeID(0), graph.NodeID(d), "R"); err != nil {
			t.Fatal(err)
		}
	}
	sg := graph.New(bld.Finalize())
	ctx := &eval.Ctx{G: sg}
	rmR := sg.CompileRelMatcher([]string{"R"})
	mAll := sg.CompileNodeMatcher(nil, nil)

	op := &plan.BindOp{Source: plan.ScanSource{
		Kind:  plan.ScanExistsSeed,
		Seeds: []plan.SeedChain{{AnchorSlot: 0, Hops: []plan.SeedHop{{Dir: graph.Outgoing}}}},
	}}
	seedRel := [][]*graph.RelMatcher{{rmR}}
	seedNode := [][]*graph.NodeMatcher{{mAll}}
	row := []value.Value{value.Node(graph.NodeID(0))}

	var cand []graph.NodeID
	var scr genScratch
	if ok := existsSeedCandidates(ctx, op, mAll, seedRel, seedNode, row, &cand, &scr); !ok {
		t.Fatal("existsSeedCandidates should succeed under the fan-out cap")
	}
	got := map[graph.NodeID]bool{}
	for _, n := range cand {
		got[n] = true
	}
	if len(got) != 3 || !got[1] || !got[2] || !got[3] {
		t.Fatalf("seed candidates = %v, want {1,2,3}", cand)
	}
}

// TestFreshScanKinds drives the scan-source variants end-to-end: an inline
// property anchors on the value index (ScanProperty), an unlabeled pattern
// scans every node (ScanAll), id(n) = k seeks a single node (ScanNodeID), and
// a substring predicate anchors via the text path (ScanTextMatch).
func TestFreshScanKinds(t *testing.T) {
	bld := chickpeas.NewBuilder(8, 0)
	a0, _ := bld.AddNode("A")
	_ = bld.SetProp(a0, "v", int64(10))
	_ = bld.SetProp(a0, "name", "alice")
	a1, _ := bld.AddNode("A")
	_ = bld.SetProp(a1, "v", int64(20))
	_ = bld.SetProp(a1, "name", "bob")
	g := graph.New(bld.Finalize("v", "name"))
	ctx := &eval.Ctx{G: g}

	run := func(src string) [][]value.Value {
		t.Helper()
		q, err := parser.Parse(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		p, err := plan.Build(q, g)
		if err != nil {
			t.Fatalf("plan %q: %v", src, err)
		}
		rows, err := Execute(ctx, p)
		if err != nil {
			t.Fatalf("exec %q: %v", src, err)
		}
		return rows
	}

	if rows := run("MATCH (a:A {v: 20}) RETURN a"); len(rows) != 1 {
		t.Fatalf("property scan rows = %d, want 1", len(rows))
	}
	if rows := run("MATCH (n) RETURN n"); len(rows) != 2 {
		t.Fatalf("all scan rows = %d, want 2", len(rows))
	}
	if rows := run("MATCH (n) WHERE id(n) = 0 RETURN n"); len(rows) != 1 {
		t.Fatalf("id-seek rows = %d, want 1", len(rows))
	}
	if rows := run("MATCH (a:A) WHERE a.name CONTAINS 'li' RETURN a"); len(rows) != 1 {
		t.Fatalf("text-match rows = %d, want 1 (alice)", len(rows))
	}
}
