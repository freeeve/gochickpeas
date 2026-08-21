package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// propInFixture: 30 Post nodes with language cycling ar/hu/en, ids
// interleaved so the union of two seeks is NOT naturally sorted.
func propInFixture(t *testing.T) graph.Graph {
	t.Helper()
	bld := chickpeas.NewBuilder(64, 0)
	langs := []string{"ar", "hu", "en"}
	for i := range 30 {
		id, err := bld.AddNode("Post")
		if err != nil {
			t.Fatal(err)
		}
		if err := bld.SetProp(id, "language", langs[i%3]); err != nil {
			t.Fatal(err)
		}
	}
	return graph.New(bld.Finalize())
}

// runIDs plans and executes q, returning the matched node ids in row
// order.
func runIDs(t *testing.T, g graph.Graph, q string) []uint32 {
	t.Helper()
	qq, err := parser.Parse(q)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(qq, g)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Execute(&eval.Ctx{G: g}, p)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]uint32, len(rows))
	for i, r := range rows {
		id, _ := r[0].AsNode()
		out[i] = uint32(id)
	}
	return out
}

// TestPropInSeekIdentityAndOrder pins the IN seek (task 308) against the
// label-scan evaluation: same rows IN THE SAME ORDER (the union sorts to
// restore ascending scan order), engagement proven by the plan source,
// and duplicate literals in the list must not duplicate rows (the union
// dedups ids).
func TestPropInSeekIdentityAndOrder(t *testing.T) {
	g := propInFixture(t)
	q := "MATCH (p:Post) WHERE p.language IN ['ar', 'hu'] RETURN p"

	// Engagement: the built plan anchors on the seek.
	qq, err := parser.Parse(q)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := plan.Build(qq, g)
	if err != nil {
		t.Fatal(err)
	}
	ms := pl.Branches[0][0].Stages[0].(*plan.MatchStage)
	if ms.Ops[0].Source.Kind != plan.ScanPropertyIn {
		t.Fatalf("plan anchors on %v, want ScanPropertyIn", ms.Ops[0].Source.Kind)
	}

	seek := runIDs(t, g, q)
	if len(seek) != 20 {
		t.Fatalf("seek rows = %d, want 20", len(seek))
	}
	plan.DisablePropInSeek = true
	scan := runIDs(t, g, q)
	plan.DisablePropInSeek = false
	if len(scan) != len(seek) {
		t.Fatalf("scan rows = %d, seek rows = %d", len(scan), len(seek))
	}
	for i := range scan {
		if scan[i] != seek[i] {
			t.Fatalf("row %d: seek id %d, scan id %d -- the union must restore ascending scan order", i, seek[i], scan[i])
		}
	}

	// Duplicate literals must not duplicate rows.
	dup := runIDs(t, g, "MATCH (p:Post) WHERE p.language IN ['ar', 'ar'] RETURN p")
	single := runIDs(t, g, "MATCH (p:Post) WHERE p.language IN ['ar'] RETURN p")
	if len(dup) != len(single) {
		t.Fatalf("duplicate-literal list yielded %d rows vs %d -- the union must dedup ids", len(dup), len(single))
	}
}
