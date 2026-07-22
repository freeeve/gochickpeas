package exec

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
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
