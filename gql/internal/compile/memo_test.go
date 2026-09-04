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
	// A small-arity function call evaluates through the stack buffer,
	// carries no reused argv, and shares.
	ef := ast.Expr(&ast.Func{Name: "abs", Args: []ast.Expr{&ast.Prop{Var: "n", Key: "age"}}})
	f1 := mm.NewFor(ctx, ef, slots, g)
	f2 := mm.NewFor(ctx, ef, slots, g)
	if f1 != f2 {
		t.Fatal("stack-arity cFunc tree did not share")
	}
	// Past the stack threshold the per-node argv returns and the tree
	// declines.
	var wide []ast.Expr
	for i := 0; i < cFuncStackArgs+1; i++ {
		wide = append(wide, &ast.Prop{Var: "n", Key: "age"})
	}
	ew := ast.Expr(&ast.Func{Name: "coalesce", Args: wide})
	w1 := mm.NewFor(ctx, ew, slots, g)
	w2 := mm.NewFor(ctx, ew, slots, g)
	if w1 == w2 {
		t.Fatal("over-threshold cFunc tree was shared")
	}
	// Nil memo compiles fresh.
	var nilMM *Memo
	if c := nilMM.NewFor(ctx, e, slots, g); c == nil {
		t.Fatal("nil memo returned nil")
	}
}
