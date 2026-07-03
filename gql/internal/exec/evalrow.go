// Package exec runs a compiled Plan against the graph seam. This file is
// the RowEval seam: every filter/projection evaluates through it, so the
// executor never cares whether the interpreted or the columnar compiled
// (M16) form is behind a given expression.
package exec

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/value"
)

// RowEval is a bound expression in per-row-evaluable form.
type RowEval interface {
	Eval(ctx *eval.Ctx, row []value.Value, slots map[string]int) value.Value
}

// interpExpr is the portable per-row evaluable: the AST run through the
// interpreter.
type interpExpr struct{ e ast.Expr }

func (i interpExpr) Eval(ctx *eval.Ctx, row []value.Value, slots map[string]int) value.Value {
	return eval.Eval(ctx, i.e, row, slots)
}

// compileEval binds an expression for per-row evaluation. M15 always
// interprets; M16 selects the columnar compiled form when the graph
// asserts the native capability.
func compileEval(e ast.Expr) RowEval { return interpExpr{e} }

// evalPushdown reports the row slots a bound expression reads (through
// slots) and whether it contains a node that pins it to the last op level
// (graph-touching or scope-extending -- subqueries, function calls, list
// machinery). The executor buckets each WHERE conjunct to the deepest op
// binding a slot it reads; slow conjuncts keep last-level timing. Mirrors
// the Rust cexpr_slots analysis over the AST.
func evalPushdown(e ast.Expr, slots map[string]int, refs *[]int, hasSlow *bool) {
	switch n := e.(type) {
	case *ast.Lit:
	case *ast.Var:
		if s, ok := slots[n.Name]; ok {
			*refs = append(*refs, s)
		}
	case *ast.Prop:
		if s, ok := slots[n.Var]; ok {
			*refs = append(*refs, s)
		}
	case *ast.HasLabelExpr:
		if s, ok := slots[n.Var]; ok {
			*refs = append(*refs, s)
		}
	case *ast.Unary:
		evalPushdown(n.Expr, slots, refs, hasSlow)
	case *ast.Binary:
		evalPushdown(n.LHS, slots, refs, hasSlow)
		evalPushdown(n.RHS, slots, refs, hasSlow)
	case *ast.ListExpr:
		for _, el := range n.Elems {
			evalPushdown(el, slots, refs, hasSlow)
		}
	case *ast.In:
		evalPushdown(n.Expr, slots, refs, hasSlow)
		evalPushdown(n.List, slots, refs, hasSlow)
	case *ast.IsNull:
		evalPushdown(n.Expr, slots, refs, hasSlow)
	case *ast.Case:
		if n.Operand != nil {
			evalPushdown(n.Operand, slots, refs, hasSlow)
		}
		for _, w := range n.Whens {
			evalPushdown(w.Cond, slots, refs, hasSlow)
			evalPushdown(w.Result, slots, refs, hasSlow)
		}
		if n.Else != nil {
			evalPushdown(n.Else, slots, refs, hasSlow)
		}
	default:
		// Subqueries, function calls, comprehensions, cost, map forms:
		// conservative last-level placement.
		*hasSlow = true
	}
}
