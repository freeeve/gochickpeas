// Parity test for the columnar batch specialization: the buffer sweep
// CandidateBatch returns must prune exactly the candidates the scalar
// Compiled.Eval rejects, across operators and operand order, and must
// respect the pre-cleared keep flags and out-of-window ids.
package compile

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
)

func TestCandidateBatchMatchesScalar(t *testing.T) {
	// Every node carries i, so the i64 column is a contiguous-presence
	// window (SliceRange ok) and the batch form specializes.
	b := chickpeas.NewBuilder(16, 0)
	var ids []graph.NodeID
	for i := range 12 {
		n, _ := b.AddNode("N")
		ids = append(ids, graph.NodeID(n))
		if err := b.SetProp(n, "i", int64(i*10-30)); err != nil {
			t.Fatal(err)
		}
	}
	g := b.Finalize("candbatch")
	ctx := &eval.Ctx{G: graph.New(g)}
	slots := map[string]int{"n": 0}
	row := make([]value.Value, 1)

	fresh := func(n int) []bool {
		keep := make([]bool, n)
		for i := range keep {
			keep[i] = true
		}
		return keep
	}

	for _, src := range []string{
		"n.i > 10", "n.i >= 0", "n.i < 50", "n.i <= 20", "n.i = 30", "n.i <> 30",
		"20 < n.i", "0 >= n.i", "30 = n.i", // reversed operand order (rev)
	} {
		c := New(ctx, exprOf(t, src), slots, g)
		batch, ok := CandidateBatch(c, 0)
		if !ok {
			t.Fatalf("%q did not specialize to a batch (compiled %T)", src, c.c)
		}
		keep := fresh(len(ids))
		batch(ctx, row, ids, keep)
		for i, id := range ids {
			row[0] = value.Node(id)
			want := c.Eval(ctx, row, slots).IsTruthy()
			if keep[i] != want {
				t.Fatalf("%q node %d: batch kept %v, eval %v", src, id, keep[i], want)
			}
		}
	}

	// A pre-cleared keep entry is never revived, and the sweep only ever
	// clears (never sets) still-kept entries.
	c := New(ctx, exprOf(t, "n.i > -1000"), slots, g) // every node passes
	batch, ok := CandidateBatch(c, 0)
	if !ok {
		t.Fatal("all-pass predicate did not specialize")
	}
	keep := []bool{false, true, false}
	batch(ctx, row, ids[:3], keep)
	if keep[0] || !keep[1] || keep[2] {
		t.Fatalf("batch disturbed pre-set keep flags: %v", keep)
	}

	// An id past the column window is treated as absent and pruned.
	b2, ok := CandidateBatch(New(ctx, exprOf(t, "n.i >= -1000000"), slots, g), 0)
	if !ok {
		t.Fatal("did not specialize")
	}
	outKeep := []bool{true}
	b2(ctx, row, []graph.NodeID{graph.NodeID(9999)}, outKeep)
	if outKeep[0] {
		t.Fatal("out-of-window id must prune")
	}

	// Shapes that are not a single-slot i64 prop-vs-int-const comparison
	// keep the scalar path.
	for _, src := range []string{"n.i + 1 > 2", "n.i = n.i", "n.i > 1.5"} {
		if _, ok := CandidateBatch(New(ctx, exprOf(t, src), slots, g), 0); ok {
			t.Fatalf("%q must not specialize to a batch", src)
		}
	}
	if _, ok := CandidateBatch(New(ctx, exprOf(t, "n.i = 30"), slots, g), 3); ok {
		t.Fatal("wrong slot must not specialize")
	}
}
