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

// numericInFixture: 24 Post nodes carrying views (float-stored) and size
// (int-stored), both cycling 0..3, plus two nodes at non-integral views
// 2.5. A key keeps one dtype graph-wide (the snapshot stores one column
// per key), so the coercion trap is literal-vs-stored dtype mismatch:
// an IN seek without twin probes finds nothing when int literals query
// the float column or float literals the int column.
func numericInFixture(t *testing.T) graph.Graph {
	t.Helper()
	bld := chickpeas.NewBuilder(32, 0)
	for i := range 24 {
		id, err := bld.AddNode("Post")
		if err != nil {
			t.Fatal(err)
		}
		if err := bld.SetProp(id, "views", float64(i%4)); err != nil {
			t.Fatal(err)
		}
		if err := bld.SetProp(id, "size", int64(i%4)); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		id, err := bld.AddNode("Post")
		if err != nil {
			t.Fatal(err)
		}
		if err := bld.SetProp(id, "views", 2.5); err != nil {
			t.Fatal(err)
		}
	}
	return graph.New(bld.Finalize())
}

// TestPropInSeekNumericTwins pins the numeric IN seek (task 310): each
// listed value probes its numeric twin alongside itself, so int literals
// find float-stored rows and vice versa, matching the coercing IN the
// label-scan path evaluates -- same rows, same order. Twin-equal literals
// (2 and 2.0) must not duplicate rows, and a non-integral literal only
// finds its exact stored value.
func TestPropInSeekNumericTwins(t *testing.T) {
	g := numericInFixture(t)
	q := "MATCH (p:Post) WHERE p.views IN [1, 2] RETURN p"

	// Engagement: the numeric list anchors on the seek.
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

	// Int literals over the float-stored views column: without twin
	// probes this seek returns 0 rows instead of 12. The disabled leg
	// evaluates the coercing IN over a label scan -- same rows, same
	// order. Then the mirror direction: float literals over the
	// int-stored size column.
	for _, iq := range []string{q, "MATCH (p:Post) WHERE p.size IN [1.0, 2.0] RETURN p"} {
		seek := runIDs(t, g, iq)
		if len(seek) != 12 {
			t.Fatalf("%q seek rows = %d, want 12", iq, len(seek))
		}
		plan.DisablePropInSeek = true
		scan := runIDs(t, g, iq)
		plan.DisablePropInSeek = false
		if len(scan) != len(seek) {
			t.Fatalf("%q scan rows = %d, seek rows = %d", iq, len(scan), len(seek))
		}
		for i := range scan {
			if scan[i] != seek[i] {
				t.Fatalf("%q row %d: seek id %d, scan id %d -- the union must restore ascending scan order", iq, i, seek[i], scan[i])
			}
		}
	}

	// Twin-equal literals dedup like duplicate literals do.
	dup := runIDs(t, g, "MATCH (p:Post) WHERE p.views IN [2, 2.0] RETURN p")
	single := runIDs(t, g, "MATCH (p:Post) WHERE p.views IN [2] RETURN p")
	if len(dup) != len(single) || len(single) != 6 {
		t.Fatalf("twin-equal list yielded %d rows vs %d (want 6) -- the union must dedup twin hits", len(dup), len(single))
	}

	// A non-integral literal has no twin: exactly the 2.5-stored rows,
	// and integral seeks never pick them up.
	if got := runIDs(t, g, "MATCH (p:Post) WHERE p.views IN [2.5] RETURN p"); len(got) != 2 {
		t.Fatalf("non-integral seek rows = %d, want 2", len(got))
	}
}
