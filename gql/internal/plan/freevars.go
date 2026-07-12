// The one exhaustive expression-variable descent, shared by the reorder
// correlation guard (collectAllVars: every mentioned name, binder locals
// included) and the scope-subset validators (freeVarsOutside: free
// references only) -- so a new AST kind is added in exactly one walker.
package plan

import (
	"slices"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// MentionsVar reports whether e references name anywhere (binder locals
// included -- over-reporting only declines an optimization, never
// unsound).
func MentionsVar(e ast.Expr, name string) bool {
	out := map[string]bool{}
	collectAllVars(e, out)
	return out[name]
}

// collectAllVars gathers every variable name e mentions, descending into
// EXISTS/COUNT subquery patterns and inner WHEREs. Unlike CheckRefs it
// adds binder-introduced locals (a subquery's pattern variables, a
// comprehension's iteration variable) too -- the caller filters against
// the query's real variables to tell a correlation from a local name;
// over-collecting only makes the reorder more conservative, never unsound.
func collectAllVars(e ast.Expr, out map[string]bool) {
	collectVars(e, out, nil)
}

// freeVarsOutside returns e's free variable references not in allowed,
// sorted for deterministic reporting. Binder-introduced locals
// (quantifier/comprehension/reduce variables, subquery pattern variables)
// are in scope for their own sub-expressions and never reported.
func freeVarsOutside(e ast.Expr, allowed []string) []string {
	locals := make(map[string]bool, len(allowed)+4)
	for _, a := range allowed {
		locals[a] = true
	}
	out := map[string]bool{}
	collectVars(e, out, locals)
	bad := make([]string, 0, len(out))
	for v := range out {
		bad = append(bad, v)
	}
	slices.Sort(bad)
	return bad
}

// collectVars walks e adding variable references to out. With locals nil,
// binder-introduced names are added to out as well (collectAllVars'
// over-collecting mode); with a locals scope, binders extend it for their
// sub-scope only and out receives just the free references.
func collectVars(e ast.Expr, out, locals map[string]bool) {
	ref := func(name string) {
		if locals == nil || !locals[name] {
			out[name] = true
		}
	}
	// bound runs fn with names in scope, restoring the scope after; in the
	// over-collecting mode the names land in out instead.
	bound := func(fn func(), names ...string) {
		if locals == nil {
			for _, n := range names {
				out[n] = true
			}
			fn()
			return
		}
		var added []string
		for _, n := range names {
			if !locals[n] {
				locals[n] = true
				added = append(added, n)
			}
		}
		fn()
		for _, n := range added {
			delete(locals, n)
		}
	}
	switch n := e.(type) {
	case nil:
		return
	case *ast.Var:
		ref(n.Name)
	case *ast.Prop:
		ref(n.Var)
	case *ast.MapProj:
		ref(n.Var)
		for _, en := range n.Entries {
			if en.Expr != nil {
				collectVars(en.Expr, out, locals)
			}
		}
	case *ast.HasLabelExpr:
		ref(n.Var)
	case *ast.Cost:
		ref(n.From)
		ref(n.To)
	case *ast.Unary:
		collectVars(n.Expr, out, locals)
	case *ast.IsNull:
		collectVars(n.Expr, out, locals)
	case *ast.Binary:
		collectVars(n.LHS, out, locals)
		collectVars(n.RHS, out, locals)
	case *ast.Func:
		for _, a := range n.Args {
			collectVars(a, out, locals)
		}
	case *ast.ListExpr:
		for _, a := range n.Elems {
			collectVars(a, out, locals)
		}
	case *ast.In:
		collectVars(n.Expr, out, locals)
		collectVars(n.List, out, locals)
	case *ast.Case:
		if n.Operand != nil {
			collectVars(n.Operand, out, locals)
		}
		for _, w := range n.Whens {
			collectVars(w.Cond, out, locals)
			collectVars(w.Result, out, locals)
		}
		if n.Else != nil {
			collectVars(n.Else, out, locals)
		}
	case *ast.Exists:
		bound(func() { collectVars(n.Where, out, locals) }, patternVarNames(n.Pattern)...)
	case *ast.CountSub:
		bound(func() { collectVars(n.Where, out, locals) }, patternVarNames(n.Pattern)...)
	case *ast.PatternComp:
		bound(func() {
			collectVars(n.Where, out, locals)
			collectVars(n.Proj, out, locals)
		}, patternVarNames(n.Pattern)...)
	case *ast.ListPred:
		collectVars(n.List, out, locals)
		bound(func() { collectVars(n.Pred, out, locals) }, n.Var)
	case *ast.Reduce:
		collectVars(n.Init, out, locals)
		collectVars(n.List, out, locals)
		bound(func() { collectVars(n.Body, out, locals) }, n.Acc, n.Var)
	case *ast.ListComp:
		collectVars(n.List, out, locals)
		bound(func() {
			if n.Filter != nil {
				collectVars(n.Filter, out, locals)
			}
			if n.Map != nil {
				collectVars(n.Map, out, locals)
			}
		}, n.Var)
	case *ast.Index:
		collectVars(n.Base, out, locals)
		collectVars(n.Idx, out, locals)
	case *ast.Slice:
		collectVars(n.Base, out, locals)
		if n.From != nil {
			collectVars(n.From, out, locals)
		}
		if n.To != nil {
			collectVars(n.To, out, locals)
		}
	case *ast.PropOf:
		collectVars(n.Base, out, locals)
	case *ast.MapLit:
		for _, f := range n.Fields {
			collectVars(f.Val, out, locals)
		}
	}
}
