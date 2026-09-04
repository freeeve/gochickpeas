// Compiled-expression memo contract: pointer-identity reuse for
// shareable trees, declines for parameter literals and per-run-mutable
// node kinds, and slot-map identity in the key.
package compile

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
)

func memoFixture(t *testing.T) (*eval.Ctx, *chickpeas.Snapshot) {
	t.Helper()
	b := chickpeas.NewBuilder(4, 4)
	n0, _ := b.AddNode("A")
	_ = b.SetProp(n0, "age", int64(30))
	g := b.Finalize("memo")
	return &eval.Ctx{G: graph.New(g)}, g
}

func TestCompiledMemo(t *testing.T) {
	ctx, g := memoFixture(t)
	mm := &Memo{}
	slots := map[string]int{"n": 0}
	// n.age > 21: prop-vs-const, the dominant shareable shape.
	e := ast.Expr(&ast.Binary{Op: ast.OpGt,
		LHS: &ast.Prop{Var: "n", Key: "age"},
		RHS: &ast.Lit{Value: ast.Literal{Kind: ast.LitInt, I: 21}}})
	c1 := mm.NewFor(ctx, e, slots, g)
	c2 := mm.NewFor(ctx, e, slots, g)
	if c1 != c2 {
		t.Fatal("shareable tree did not memoize")
	}
	row := []value.Value{value.Node(0)}
	if !c1.Eval(ctx, row, slots).IsTruthy() {
		t.Fatal("memoized tree evaluated wrong")
	}
	// Same expr under a DIFFERENT slot map must not collide.
	slots2 := map[string]int{"n": 0, "m": 1}
	if c3 := mm.NewFor(ctx, e, slots2, g); c3 == c1 {
		t.Fatal("distinct slot maps shared a compile")
	}
	// A parameter literal declines (per-variant values bake in).
	ep := ast.Expr(&ast.Binary{Op: ast.OpGt,
		LHS: &ast.Prop{Var: "n", Key: "age"},
		RHS: &ast.Lit{Value: ast.Literal{Kind: ast.LitParam, P: 0}}})
	ctx.Params = []value.Value{value.Int(21)}
	p1 := mm.NewFor(ctx, ep, slots, g)
	p2 := mm.NewFor(ctx, ep, slots, g)
	if p1 == p2 {
		t.Fatal("parameter-bearing expression was memoized")
	}
	// A function call (cFunc's reused argv) is tree-unshareable.
	ef := ast.Expr(&ast.Func{Name: "abs", Args: []ast.Expr{&ast.Prop{Var: "n", Key: "age"}}})
	f1 := mm.NewFor(ctx, ef, slots, g)
	f2 := mm.NewFor(ctx, ef, slots, g)
	if f1 == f2 {
		t.Fatal("cFunc-bearing tree was shared")
	}
	// Nil memo compiles fresh.
	var nilMM *Memo
	if c := nilMM.NewFor(ctx, e, slots, g); c == nil {
		t.Fatal("nil memo returned nil")
	}
}
