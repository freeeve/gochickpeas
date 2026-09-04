// Cross-execution memoization of compiled expression trees: the dual
// shareability gates (AST-side parameter check, lowered-side mutable-kind
// check) and the (snapshot, expression, slot-map)-keyed store the
// PlanCache threads through eval.Ctx. Split from compile.go, which holds
// the node kinds and the lowering.
package compile

import (
	"reflect"
	"sync"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
)

// exprShareable reports whether e's compiled form depends only on
// (expression, slots, graph): any parameter literal bakes a per-variant
// value at compile time and declines; unknown node kinds decline
// conservatively. This is the AST-side half of the memo gate -- the
// compiled-side half (treeShareable) rejects per-run-mutable node kinds.
func exprShareable(e ast.Expr) bool {
	switch n := e.(type) {
	case nil:
		return true
	case *ast.Lit:
		return n.Value.Kind != ast.LitParam && n.Value.Kind != ast.LitNamedParam
	case *ast.Var, *ast.HasLabelExpr:
		return true
	case *ast.Prop:
		return true
	case *ast.Unary:
		return exprShareable(n.Expr)
	case *ast.Binary:
		return exprShareable(n.LHS) && exprShareable(n.RHS)
	case *ast.IsNull:
		return exprShareable(n.Expr)
	case *ast.In:
		return exprShareable(n.Expr) && exprShareable(n.List)
	case *ast.Index:
		return exprShareable(n.Base) && exprShareable(n.Idx)
	case *ast.Func:
		for _, a := range n.Args {
			if !exprShareable(a) {
				return false
			}
		}
		return true
	case *ast.ListExpr:
		for _, x := range n.Elems {
			if !exprShareable(x) {
				return false
			}
		}
		return true
	case *ast.Case:
		if !exprShareable(n.Operand) || !exprShareable(n.Else) {
			return false
		}
		for _, w := range n.Whens {
			if !exprShareable(w.Cond) || !exprShareable(w.Result) {
				return false
			}
		}
		return true
	}
	return false
}

// treeShareable reports whether the lowered tree is immutable: the
// per-run-mutable kinds (subquery memos, carried-IN epochs, cFunc's
// reused argv) decline; everything else in the lowering is
// write-once-at-compile.
func treeShareable(c cnode) bool {
	switch n := c.(type) {
	case *cLit, *cSlot, *cProp, *cInConst, *cCmpPropConst, *cSlow:
		return true
	case *cNot:
		return treeShareable(n.e)
	case *cNeg:
		return treeShareable(n.e)
	case *cBin:
		return treeShareable(n.l) && treeShareable(n.r)
	case *cList:
		for _, x := range n.xs {
			if !treeShareable(x) {
				return false
			}
		}
		return true
	case *cIn:
		return treeShareable(n.e) && treeShareable(n.list)
	case *cIsNull:
		return treeShareable(n.e)
	case *cCase:
		if !treeShareable(n.operand) || !treeShareable(n.els) {
			return false
		}
		for _, w := range n.whens {
			if !treeShareable(w[0]) || !treeShareable(w[1]) {
				return false
			}
		}
		return true
	}
	return false // cSubquery, cInCarried, cFunc, unknown kinds
}

// memoCap bounds the compiled-expression memo (a runaway backstop; the
// population is the cached plans' non-param simple predicates).
const memoCap = 8192

// memoKey is (snapshot, expression node, slot map), all by identity: an
// expression node is owned by its cached plan, and the plan gate can
// evaluate one conjunct pointer under a second slot map, so the map's
// identity is part of what the compile consumed.
type memoKey struct {
	g     *chickpeas.Snapshot
	e     ast.Expr
	slots uintptr
}

// Memo carries compiled expression trees across executions of cached
// plans. Only trees passing BOTH shareability gates store; hoisting
// rewrites rebuild fresh spines (hoist.go), so a memoized pre-hoist tree
// handed to a hoisting site is never mutated -- copies share only
// immutable leaves. Hits return the artifact by pointer; the rowFast
// closure and every stored node kind are write-once-at-compile.
type Memo struct {
	mu sync.Mutex
	m  map[memoKey]*Compiled
}

// NewFor is the memoized compile entry: a nil memo, a parameter-bearing
// expression, or an over-cap memo compiles fresh; a non-shareable result
// is returned unstored.
func (mm *Memo) NewFor(ctx *eval.Ctx, e ast.Expr, slots map[string]int, g *chickpeas.Snapshot) *Compiled {
	if mm == nil || !exprShareable(e) {
		return New(ctx, e, slots, g)
	}
	key := memoKey{g: g, e: e, slots: reflect.ValueOf(slots).Pointer()}
	mm.mu.Lock()
	c := mm.m[key]
	mm.mu.Unlock()
	if c != nil {
		return c
	}
	c = New(ctx, e, slots, g)
	if treeShareable(c.c) {
		mm.mu.Lock()
		if mm.m == nil {
			mm.m = map[memoKey]*Compiled{}
		}
		if len(mm.m) < memoCap {
			mm.m[key] = c
		}
		mm.mu.Unlock()
	}
	return c
}
