package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
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
