// Differential test for the record-form typed sort: for every key-width,
// kind, and direction mix -- with and without LIMIT bounding -- the
// record path must reproduce a reference stable sort under value.OrderCmp
// exactly (the index tiebreak makes both total, so equality is row-for-
// row). Mixed-kind columns fall to the generic path, exercised alongside.
package exec

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
	"sort"
)

func TestSortRecordsMatchReference(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	kinds := [][]string{
		{"int"},
		{"float"},
		{"node"},
		{"int", "node"},
		{"float", "int"},
		{"node", "int", "float"},
		{"int", "int", "int"},
		{"int", "mixed"}, // generic fallback path
	}
	mk := func(kind string, r *rand.Rand) value.Value {
		switch kind {
		case "int":
			return value.Int(int64(r.Intn(7) - 3))
		case "float":
			return value.Float(float64(r.Intn(5)) / 2.0)
		case "node":
			return value.Node(uint32(r.Intn(6)))
		default:
			if r.Intn(2) == 0 {
				return value.Int(int64(r.Intn(4)))
			}
			return value.Str(fmt.Sprintf("s%d", r.Intn(4)))
		}
	}
	for ki, ks := range kinds {
		for _, desc := range []bool{false, true} {
			for _, limit := range []*uint64{nil, ptrU64(5)} {
				n := 200
				cols := make([]string, len(ks)+1)
				returns := make([]plan.BoundReturn, len(ks)+1)
				order := make([]ast.SortItem, len(ks))
				for k := range ks {
					cols[k] = fmt.Sprintf("k%d", k)
					order[k] = ast.SortItem{Expr: &ast.Var{Name: cols[k]}, Desc: desc != (k%2 == 1)}
				}
				cols[len(ks)] = "payload"
				proj := &plan.ProjPlan{Columns: cols, Returns: returns, OrderBy: order, Limit: limit}
				outs := make([][]value.Value, n)
				for i := range outs {
					row := make([]value.Value, len(cols))
					for k, kind := range ks {
						row[k] = mk(kind, rng)
					}
					row[len(ks)] = value.Int(int64(i))
					outs[i] = row
				}
				// Reference: stable sort under value.OrderCmp.
				ref := make([][]value.Value, n)
				copy(ref, outs)
				sort.SliceStable(ref, func(a, b int) bool {
					for k := range order {
						c := value.OrderCmp(ref[a][k], ref[b][k])
						if order[k].Desc {
							c = -c
						}
						if c != 0 {
							return c < 0
						}
					}
					return false
				})
				if limit != nil && int(*limit) < len(ref) {
					ref = ref[:*limit]
				}
				ctx := &eval.Ctx{}
				got := sortRowsByOrder(ctx, proj, map[string]int{}, func(int) []value.Value { return nil }, 0, outs)
				got = paginate(got, nil, limit)
				if len(got) != len(ref) {
					t.Fatalf("case %d desc=%v limit=%v: len %d want %d", ki, desc, limit != nil, len(got), len(ref))
				}
				for i := range ref {
					if fmt.Sprint(got[i]) != fmt.Sprint(ref[i]) {
						t.Fatalf("case %d desc=%v limit=%v row %d: got %v want %v",
							ki, desc, limit != nil, i, got[i], ref[i])
					}
				}
			}
		}
	}
}

func ptrU64(v uint64) *uint64 { return &v }
