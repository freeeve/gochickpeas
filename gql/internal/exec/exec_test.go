package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// urow builds a single-column row of int values.
func urow(xs ...int64) []value.Value {
	r := make([]value.Value, len(xs))
	for i, x := range xs {
		r[i] = value.Int(x)
	}
	return r
}

func rowsEqual(got, want [][]value.Value) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if len(got[i]) != len(want[i]) {
			return false
		}
		for j := range got[i] {
			if !value.Equal(got[i][j], want[i][j]) {
				return false
			}
		}
	}
	return true
}

// TestCombineUnion covers the four set-combination semantics: UNION ALL
// concatenates, UNION dedups the whole set in first-occurrence order, EXCEPT
// keeps deduped-left rows absent from the right, and INTERSECT keeps
// deduped-left rows present in the right.
func TestCombineUnion(t *testing.T) {
	// UNION ALL: concatenate, duplicates kept.
	acc := [][]value.Value{urow(1), urow(2)}
	combineUnion(&acc, [][]value.Value{urow(2), urow(3)}, ast.UnionAll)
	if !rowsEqual(acc, [][]value.Value{urow(1), urow(2), urow(2), urow(3)}) {
		t.Fatalf("UNION ALL = %v", acc)
	}

	// UNION (distinct): dedup the accumulated set, first-occurrence order.
	acc = [][]value.Value{urow(1), urow(2)}
	combineUnion(&acc, [][]value.Value{urow(2), urow(3)}, ast.UnionDistinct)
	if !rowsEqual(acc, [][]value.Value{urow(1), urow(2), urow(3)}) {
		t.Fatalf("UNION = %v", acc)
	}

	// EXCEPT: dedup the left, drop rows present in the right.
	acc = [][]value.Value{urow(1), urow(2), urow(2), urow(3)}
	combineUnion(&acc, [][]value.Value{urow(2)}, ast.UnionExcept)
	if !rowsEqual(acc, [][]value.Value{urow(1), urow(3)}) {
		t.Fatalf("EXCEPT = %v", acc)
	}

	// INTERSECT: dedup the left, keep rows present in the right.
	acc = [][]value.Value{urow(1), urow(2), urow(3), urow(3)}
	combineUnion(&acc, [][]value.Value{urow(2), urow(3), urow(4)}, ast.UnionIntersect)
	if !rowsEqual(acc, [][]value.Value{urow(2), urow(3)}) {
		t.Fatalf("INTERSECT = %v", acc)
	}
}

// TestExecuteProfiled covers the PROFILE execution entry: it walks each
// branch's segments recording per-segment projected-row cardinalities.
func TestExecuteProfiled(t *testing.T) {
	bld := chickpeas.NewBuilder(4, 0)
	for range 3 {
		if _, err := bld.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	g := graph.New(bld.Finalize())
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

	// A single projection: one segment, one projected row.
	if prof := ExecuteProfiled(ctx, build("RETURN 1 AS x")); len(prof.Segs) != 1 || prof.Segs[0].ProjRows != 1 {
		t.Fatalf("RETURN 1: segs=%d projRows=%d, want 1/1", len(prof.Segs), prof.Segs[0].ProjRows)
	}
	// A NEXT chain profiles two segments.
	if prof := ExecuteProfiled(ctx, build("RETURN 1 AS x NEXT RETURN x AS y")); len(prof.Segs) != 2 {
		t.Fatalf("NEXT chain segs = %d, want 2", len(prof.Segs))
	}
	// A scan over the three N nodes projects three rows.
	if prof := ExecuteProfiled(ctx, build("MATCH (n:N) RETURN n")); len(prof.Segs) != 1 || prof.Segs[0].ProjRows != 3 {
		t.Fatalf("MATCH scan: segs=%d projRows=%d, want 1/3", len(prof.Segs), prof.Segs[0].ProjRows)
	}
}
