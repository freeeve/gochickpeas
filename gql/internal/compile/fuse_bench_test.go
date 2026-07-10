// Micro-benchmark for the fused prop-vs-const comparison: same process,
// alternating sub-benchmarks, so machine load cancels out of the
// comparison. The fused node must never be slower than the equivalent
// cBin tree it replaces.
package compile

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/value"
)

// benchFixture builds n nodes with an i64 "v" property and returns the
// snapshot plus one row per node.
func benchFixture(b *testing.B, n int) (*chickpeas.Snapshot, [][]value.Value) {
	b.Helper()
	bld := chickpeas.NewBuilder(n, 1)
	for i := 0; i < n; i++ {
		id, _ := bld.AddNode("N")
		_ = bld.SetProp(id, "v", int64(i))
	}
	g := bld.Finalize()
	rows := make([][]value.Value, n)
	for i := 0; i < n; i++ {
		rows[i] = []value.Value{value.Node(chickpeas.NodeID(i))}
	}
	return g, rows
}

func BenchmarkCmpPropConst(b *testing.B) {
	g, rows := benchFixture(b, 4096)
	slots := map[string]int{"n": 0}
	ctx := &eval.Ctx{}
	prop := &cProp{slot: 0, reader: newPropReader(g, "v")}
	lit := &cLit{v: value.Int(2048)}
	fused := &Compiled{c: &cCmpPropConst{prop: prop, c: lit.v, op: ast.OpLt}, g: g}
	unfused := &Compiled{c: &cBin{op: ast.OpLt, l: prop, r: lit}, g: g}
	for _, bc := range []struct {
		name string
		c    *Compiled
	}{{"fused", fused}, {"unfused", unfused}, {"fused2", fused}, {"unfused2", unfused}} {
		b.Run(bc.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if v := bc.c.Eval(ctx, rows[i%len(rows)], slots); v.IsNull() {
					b.Fatal("unexpected null")
				}
			}
		})
	}
}
