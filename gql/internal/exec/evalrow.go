// Package exec runs a compiled Plan against the graph seam. This file is
// the RowEval seam: every filter/projection evaluates through it, so the
// executor never cares whether the interpreted or the columnar compiled
// form is behind a given expression.
package exec

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/compile"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
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

// compileEval binds an expression for per-row evaluation: the columnar
// compiled form when the graph asserts the native capability (and the
// differential test hook doesn't pin the interpreter), else interpreted.
func compileEval(ctx *eval.Ctx, e ast.Expr, slots map[string]int) RowEval {
	if !ctx.ForceInterp {
		if n, ok := ctx.G.(graph.Native); ok {
			return compile.New(ctx, e, slots, n.Snapshot())
		}
	}
	return interpExpr{e}
}

// hoistEval applies the backend's IN-list hoisting to a bound expression:
// batch-constant lists bake a membership index, carried loop-invariant
// lists cache per match-call. The interpreted form re-evaluates lists per
// row (correct, just not pre-hashed), mirroring the Rust portable default.
func hoistEval(ctx *eval.Ctx, re RowEval, isConst, isCarried func(int) bool, sample []value.Value, slots map[string]int) RowEval {
	c, ok := re.(*compile.Compiled)
	if !ok {
		return re
	}
	return compile.HoistCarriedIn(compile.HoistConstIn(ctx, c, isConst, sample, slots), isCarried)
}

// evalPushdown reports the row slots a bound expression reads (through
// slots) and whether it contains a node that pins it to the last op level.
// The compiled form introspects its resolved nodes (a memoized subquery
// pushes to where its correlated bindings bind); the interpreted form
// walks the AST conservatively.
func evalPushdown(re RowEval, e ast.Expr, slots map[string]int, refs *[]int, hasSlow *bool) {
	if c, ok := re.(*compile.Compiled); ok {
		r, slow := compile.Slots(c)
		*refs = append(*refs, r...)
		*hasSlow = *hasSlow || slow
		return
	}
	astPushdown(e, slots, refs, hasSlow)
}

// astPushdown is the interpreted path's conservative slot analysis.
func astPushdown(e ast.Expr, slots map[string]int, refs *[]int, hasSlow *bool) {
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
		astPushdown(n.Expr, slots, refs, hasSlow)
	case *ast.Binary:
		astPushdown(n.LHS, slots, refs, hasSlow)
		astPushdown(n.RHS, slots, refs, hasSlow)
	case *ast.ListExpr:
		for _, el := range n.Elems {
			astPushdown(el, slots, refs, hasSlow)
		}
	case *ast.In:
		astPushdown(n.Expr, slots, refs, hasSlow)
		astPushdown(n.List, slots, refs, hasSlow)
	case *ast.IsNull:
		astPushdown(n.Expr, slots, refs, hasSlow)
	case *ast.Case:
		if n.Operand != nil {
			astPushdown(n.Operand, slots, refs, hasSlow)
		}
		for _, w := range n.Whens {
			astPushdown(w.Cond, slots, refs, hasSlow)
			astPushdown(w.Result, slots, refs, hasSlow)
		}
		if n.Else != nil {
			astPushdown(n.Else, slots, refs, hasSlow)
		}
	default:
		// Subqueries, function calls, comprehensions, cost, map forms:
		// conservative last-level placement.
		*hasSlow = true
	}
}
