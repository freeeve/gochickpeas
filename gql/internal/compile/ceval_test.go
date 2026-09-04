// Differential coverage of the compiled evaluator's operator arms: every
// expression evaluates through BOTH the compiled tree and the
// interpreter, and the two must agree by canonical value key -- the
// package-local form of the engine's dual-path harness.
package compile

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
)

func cvLit(i int64) ast.Expr                     { return &ast.Lit{Value: ast.Literal{Kind: ast.LitInt, I: i}} }
func cvSlit(s string) ast.Expr                   { return &ast.Lit{Value: ast.Literal{Kind: ast.LitStr, S: s}} }
func cvBlit(b bool) ast.Expr                     { return &ast.Lit{Value: ast.Literal{Kind: ast.LitBool, B: b}} }
func cvNul() ast.Expr                            { return &ast.Lit{Value: ast.Literal{Kind: ast.LitNull}} }
func cvBin(op ast.BinOp, l, r ast.Expr) ast.Expr { return &ast.Binary{Op: op, LHS: l, RHS: r} }

func TestCevalOperatorArmsMatchInterpreter(t *testing.T) {
	b := chickpeas.NewBuilder(4, 4)
	n0, _ := b.AddNode("A")
	_ = b.SetProp(n0, "age", int64(30))
	_ = b.SetProp(n0, "name", "ada")
	g := b.Finalize("ceval")
	ctx := &eval.Ctx{G: graph.New(g)}
	slots := map[string]int{"n": 0}
	row := []value.Value{value.Node(0)}
	age := ast.Expr(&ast.Prop{Var: "n", Key: "age"})
	name := ast.Expr(&ast.Prop{Var: "n", Key: "name"})

	exprs := []ast.Expr{
		// Three-valued logic, short-circuits and null arms included.
		cvBin(ast.OpAnd, cvBlit(true), cvBlit(false)),
		cvBin(ast.OpAnd, cvBlit(false), cvNul()),
		cvBin(ast.OpAnd, cvNul(), cvBlit(true)),
		cvBin(ast.OpOr, cvBlit(true), cvNul()),
		cvBin(ast.OpOr, cvNul(), cvBlit(false)),
		cvBin(ast.OpXor, cvBlit(true), cvBlit(false)),
		cvBin(ast.OpXor, cvNul(), cvBlit(true)),
		// Comparisons over a property read (cCmpPropConst fusion) and
		// over general operands.
		cvBin(ast.OpEq, age, cvLit(30)),
		cvBin(ast.OpNeq, age, cvLit(31)),
		cvBin(ast.OpLt, age, cvLit(31)),
		cvBin(ast.OpLte, age, cvLit(30)),
		cvBin(ast.OpGt, cvLit(31), age),
		cvBin(ast.OpGte, age, age),
		cvBin(ast.OpEq, age, cvNul()),
		// String predicates and concat.
		cvBin(ast.OpStartsWith, name, cvSlit("a")),
		cvBin(ast.OpEndsWith, name, cvSlit("da")),
		cvBin(ast.OpContains, name, cvSlit("d")),
		cvBin(ast.OpContains, name, cvSlit("zz")),
		cvBin(ast.OpConcat, name, cvSlit("!")),
		// Arithmetic through the default arm.
		cvBin(ast.OpAdd, age, cvLit(12)),
		cvBin(ast.OpSub, age, cvLit(5)),
		cvBin(ast.OpMul, age, cvLit(2)),
		cvBin(ast.OpDiv, age, cvLit(3)),
		cvBin(ast.OpAdd, age, cvNul()),
		// Unary, IS NULL, CASE, lists and IN.
		&ast.Unary{Op: ast.Not, Expr: cvBin(ast.OpGt, age, cvLit(10))},
		&ast.Unary{Op: ast.Neg, Expr: age},
		&ast.IsNull{Expr: cvNul()},
		&ast.IsNull{Expr: age, Negated: true},
		&ast.Case{Operand: age, Whens: []ast.CaseWhen{{Cond: cvLit(30), Result: cvSlit("thirty")}}, Else: cvSlit("other")},
		&ast.Case{Whens: []ast.CaseWhen{{Cond: cvBin(ast.OpGt, age, cvLit(40)), Result: cvSlit("old")}}, Else: cvSlit("young")},
		&ast.ListExpr{Elems: []ast.Expr{cvLit(1), age, cvNul()}},
		&ast.In{Expr: age, List: &ast.ListExpr{Elems: []ast.Expr{cvLit(29), cvLit(30)}}},
		&ast.In{Expr: age, List: &ast.ListExpr{Elems: []ast.Expr{cvLit(1), cvNul()}}},
		&ast.In{Expr: cvLit(1), List: cvNul()},
		// Index reads through the compiled path.
		&ast.Index{Base: &ast.ListExpr{Elems: []ast.Expr{cvLit(7), cvLit(8)}}, Idx: cvLit(1)},
		// Var reads: bound and unbound.
		&ast.Var{Name: "n"},
		&ast.Var{Name: "missing"},
	}
	for i, e := range exprs {
		c := New(ctx, e, slots, g)
		got := c.Eval(ctx, row, slots)
		want := eval.Eval(ctx, e, row, slots)
		if value.Key(got) != value.Key(want) {
			t.Errorf("expr %d: compiled %v, interpreted %v", i, got, want)
		}
	}
}
