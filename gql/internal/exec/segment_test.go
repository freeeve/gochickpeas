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

// TestFilterBoundarySegment drives a standalone FILTER over a computed alias:
// RETURN a.v + 1 AS w NEXT FILTER w > 15 runs the FILTER as its own segment
// through the passthrough sink, keeping only the rows whose computed value
// passes the guard.
func TestFilterBoundarySegment(t *testing.T) {
	bld := chickpeas.NewBuilder(8, 0)
	for _, v := range []int64{10, 20, 30} {
		n, err := bld.AddNode("A")
		if err != nil {
			t.Fatal(err)
		}
		_ = bld.SetProp(n, "v", v)
	}
	g := graph.New(bld.Finalize("v"))
	ctx := &eval.Ctx{G: g}

	q, err := parser.Parse("MATCH (a:A) RETURN a.v + 1 AS w NEXT FILTER w > 15 RETURN w")
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

	// v+1 gives {11, 21, 31}; FILTER w > 15 keeps {21, 31}.
	got := map[int64]bool{}
	for _, r := range rows {
		w, _ := r[0].AsInt()
		got[w] = true
	}
	if len(got) != 2 || !got[21] || !got[31] {
		t.Fatalf("post-WHERE results = %v, want {21, 31}", got)
	}
}

// TestAggregateSegmentPostWhere drives applyPostWhere: a FILTER fused onto
// an aggregating boundary filters the grouped output rows in place,
// including the collect-broadcast IN probe over a constant output column.
func TestAggregateSegmentPostWhere(t *testing.T) {
	bld := chickpeas.NewBuilder(8, 0)
	for _, v := range []int64{1, 1, 2, 3} {
		n, err := bld.AddNode("A")
		if err != nil {
			t.Fatal(err)
		}
		_ = bld.SetProp(n, "v", v)
	}
	g := graph.New(bld.Finalize("v"))
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

	// Groups: v=1 (n=2), v=2 (n=1), v=3 (n=1); the boundary FILTER keeps
	// only the doubled group.
	rows := run("MATCH (a:A) RETURN a.v AS v, count(*) AS n NEXT FILTER n > 1 RETURN v, n")
	if len(rows) != 1 {
		t.Fatalf("filtered groups = %d, want 1", len(rows))
	}
	if v, _ := rows[0][0].AsInt(); v != 1 {
		t.Fatalf("kept group v = %d, want 1", v)
	}

	// IN over a collected broadcast column: every row carries the same
	// list, the membership probe keeps matching groups.
	rows = run("MATCH (a:A) RETURN a.v AS v, collect(a.v) AS all NEXT FILTER v IN all RETURN v")
	if len(rows) != 3 {
		t.Fatalf("membership-filtered groups = %d, want 3", len(rows))
	}
}
