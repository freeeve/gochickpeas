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

// TestFuncConjunctPrunesMidPattern is the work-observing half of the
// predicate-placement fix (task 298, ported from rustychickpeas 00813de):
// a scalar-function conjunct on a mid-pattern node must prune at that
// node's binding level, not after the whole pattern enumerates. Observed
// via PROFILE operator row counts (box-independent), summed across all
// ops rather than asserting on a named operator, since the planner is
// free to anchor the chain from either end. Fixture: one anchor, 8 mids,
// 8 leaves per mid; exactly one mid passes abs(m.v) = 0, so a pruning
// placement walks ~1 mid's leaves while a last-level pin walks all 64.
func TestFuncConjunctPrunesMidPattern(t *testing.T) {
	bld := chickpeas.NewBuilder(128, 256)
	a, _ := bld.AddNode("A")
	for i := range 8 {
		m, _ := bld.AddNode("M")
		if err := bld.SetProp(m, "v", int64(i)); err != nil {
			t.Fatal(err)
		}
		if _, err := bld.AddRel(a, m, "R"); err != nil {
			t.Fatal(err)
		}
		for range 8 {
			b, _ := bld.AddNode("B")
			if _, err := bld.AddRel(m, b, "S"); err != nil {
				t.Fatal(err)
			}
		}
	}
	g := graph.New(bld.Finalize())
	ctx := &eval.Ctx{G: g}
	q, err := parser.Parse("MATCH (a:A)-[:R]->(m:M)-[:S]->(b:B) WHERE abs(m.v) = 0 RETURN b")
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(q, g)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Execute(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 8 {
		t.Fatalf("result rows = %d, want 8 (the one passing mid's leaves)", len(rows))
	}
	var total uint64
	for _, sp := range ExecuteProfiled(ctx, p).Segs {
		for _, st := range sp.Stages {
			for _, n := range st.Match {
				total += n
			}
			if st.Single != nil {
				total += *st.Single
			}
		}
	}
	// Pruning placement: 1 anchor + 8 mids + the passing mid's 8 leaves
	// (+ the WHERE tally) stays well under the ~80 rows a last-level pin
	// pays walking every mid's leaves before filtering.
	if total >= 40 {
		t.Fatalf("summed PROFILE op rows = %d: the function conjunct did not prune before the fan-out", total)
	}
}

// TestVarLenRelConjunctPlacement pins the coupling the port warned about
// (task 298): a conjunct reading a named variable-length relationship
// LIST slot is only placeable once that slot is tracked in the level map
// -- with placement relaxed but the slot untracked, the conjunct lands
// where the slot is still null, size(e) evaluates to null, and every row
// is silently dropped. Chain x0->x1->x2 over R: size(e) = 2 must keep
// exactly the two-hop trails, not zero rows.
func TestVarLenRelConjunctPlacement(t *testing.T) {
	bld := chickpeas.NewBuilder(4, 4)
	for range 3 {
		if _, err := bld.AddNode("X"); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range [][2]int{{0, 1}, {1, 2}} {
		if _, err := bld.AddRel(graph.NodeID(e[0]), graph.NodeID(e[1]), "R"); err != nil {
			t.Fatal(err)
		}
	}
	g := graph.New(bld.Finalize())
	ctx := &eval.Ctx{G: g}
	q, err := parser.Parse("MATCH (a:X)-[e:R]->{1,2}(b:X) WHERE size(e) = 2 RETURN a, b")
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(q, g)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Execute(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("size(e) = 2 kept %d rows, want 1 (the x0->x1->x2 trail)", len(rows))
	}
	aid, _ := rows[0][0].AsNode()
	bid, _ := rows[0][1].AsNode()
	if aid != 0 || bid != 2 {
		t.Fatalf("kept trail = (%d, %d), want (0, 2)", aid, bid)
	}
}

// TestSlotAgrees covers the batch-constant predicate: an empty or single-row
// batch is vacuously constant, equal values agree, a differing value breaks
// it, and out-of-range reads disqualify unless padNull treats them as Null.
func TestSlotAgrees(t *testing.T) {
	row := func(vs ...value.Value) []value.Value { return vs }

	if !slotAgrees(0, nil, false) || !slotAgrees(0, [][]value.Value{row(value.Int(5))}, false) {
		t.Fatal("empty and single-row batches are vacuously constant")
	}
	if !slotAgrees(0, [][]value.Value{row(value.Int(5)), row(value.Int(5))}, false) {
		t.Fatal("equal values agree")
	}
	if slotAgrees(0, [][]value.Value{row(value.Int(5)), row(value.Int(6))}, false) {
		t.Fatal("a differing value breaks constancy")
	}
	// A constant slot alongside a differing one.
	if !slotAgrees(1, [][]value.Value{row(value.Int(5), value.Int(9)), row(value.Int(6), value.Int(9))}, false) {
		t.Fatal("slot 1 is constant while slot 0 differs")
	}
	// Out-of-range with padNull=false disqualifies (multiple rows).
	if slotAgrees(2, [][]value.Value{row(value.Int(5)), row(value.Int(6))}, false) {
		t.Fatal("out-of-range slot must disqualify when padNull is false")
	}
	// Out-of-range with padNull=true reads Null everywhere -> agrees.
	if !slotAgrees(2, [][]value.Value{row(value.Int(5)), row(value.Int(6))}, true) {
		t.Fatal("out-of-range slot reads Null under padNull, so it agrees")
	}
	// Ragged rows: present-then-absent disqualifies without padNull, agrees
	// with padNull when the present value is Null.
	if slotAgrees(1, [][]value.Value{row(value.Int(5), value.Int(9)), row(value.Int(6))}, false) {
		t.Fatal("a row missing the slot disqualifies without padNull")
	}
	if !slotAgrees(1, [][]value.Value{row(value.Int(5), value.Null()), row(value.Int(6))}, true) {
		t.Fatal("a Null present value matches a padded-Null absent one")
	}
}
