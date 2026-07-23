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

// TestRunSubplan covers the CALL-subquery runner (and runBranchSeeded): a
// single-branch projection over a seed yields its row, and a UNION combines
// both branches' rows.
func TestRunSubplan(t *testing.T) {
	g := graph.New(chickpeas.NewBuilder(1, 0).Finalize())
	ctx := &eval.Ctx{G: g}
	build := func(src string) *plan.Plan {
		t.Helper()
		q, err := parser.Parse(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		p, err := plan.Build(q, g)
		if err != nil {
			t.Fatalf("plan %q: %v", src, err)
		}
		return p
	}

	// One branch: the projection produces a single row over the empty seed.
	rows := runSubplan(ctx, build("RETURN 1 AS x"), []value.Value{})
	if len(rows) != 1 {
		t.Fatalf("single-branch rows = %d, want 1", len(rows))
	}
	if v, _ := rows[0][0].AsInt(); v != 1 {
		t.Fatalf("single-branch value = %v, want 1", rows[0][0])
	}

	// UNION combines both branches (distinct).
	u := runSubplan(ctx, build("RETURN 1 AS x UNION RETURN 2 AS x"), []value.Value{})
	if len(u) != 2 {
		t.Fatalf("union rows = %d, want 2", len(u))
	}
	got := map[int64]bool{}
	for _, r := range u {
		v, _ := r[0].AsInt()
		got[v] = true
	}
	if !got[1] || !got[2] {
		t.Fatalf("union values = %v, want {1,2}", got)
	}
}
