// Auto-parameterization (port of autoparam.rs): lift constant inline
// node/relationship property values out of a query into numbered parameter
// slots, so two queries differing only in those constants -- {id: 669} vs
// {id: 648}, {name: 'India'} vs {name: 'China'} -- become one template
// that shares a cached plan. The lifted values are returned in slot order
// and supplied per execution through the eval context.
//
// Lifted: inline pattern property values ({id: 669}) and constant
// comparison bounds in WHERE / subquery predicates (m.day < D) -- the
// positions that vary most across parameter sets. Left baked: CASE bin
// thresholds, scalar-function arguments, projection constants, UNWIND/FOR
// lists, CALL procedure arguments, and var-length bounds -- these can
// shape the plan, so lifting them would change the plan, not just a
// looked-up value.
package semantics

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/value"
)

// AutoParameterize lifts inline property constants in q to Literal param
// slots in a fixed left-to-right order, mutating q into the cacheable
// template and returning the lifted values in slot order. Two queries with
// identical structure lift to identical templates with values in matching
// slots.
func AutoParameterize(q *ast.Query) []value.Value {
	var vals []value.Value
	liftParts(q.Parts, &vals)
	return vals
}

// LitValue converts a parsed constant to its runtime value. Param slots
// resolve at execution time, not here, and read as Null.
func LitValue(l ast.Literal) value.Value {
	switch l.Kind {
	case ast.LitInt:
		return value.Int(l.I)
	case ast.LitFloat:
		return value.Float(l.F)
	case ast.LitStr:
		return value.Str(l.S)
	case ast.LitBool:
		return value.Bool(l.B)
	default:
		return value.Null()
	}
}

// liftParts lifts constants across every branch of a (sub)query's parts,
// in order, so the slot layout is stable for the whole query. Shared by
// the top-level pass and a CALL { ... } subquery body.
func liftParts(parts []ast.QueryPart, vals *[]value.Value) {
	for i := range parts {
		part := &parts[i]
		for _, clause := range part.Clauses {
			liftClause(clause, vals)
		}
		liftProjection(&part.Ret, vals)
	}
}

// liftLit replaces a single constant with the next parameter slot,
// recording its value. A null match ({x: null}) is structural and an
// already-lifted slot is left as is.
func liftLit(lit *ast.Literal, vals *[]value.Value) {
	switch lit.Kind {
	case ast.LitNull, ast.LitParam, ast.LitNamedParam:
		return
	}
	slot := uint32(len(*vals))
	*vals = append(*vals, LitValue(*lit))
	*lit = ast.ParamLit(slot)
}

func liftProps(props []ast.PropEntry, vals *[]value.Value) {
	for i := range props {
		liftLit(&props[i].Val, vals)
	}
}

func isComparison(op ast.BinOp) bool {
	switch op {
	case ast.OpLt, ast.OpLte, ast.OpGt, ast.OpGte, ast.OpEq, ast.OpNeq,
		ast.OpStartsWith, ast.OpEndsWith, ast.OpContains:
		return true
	}
	return false
}

func liftPattern(p *ast.Pattern, vals *[]value.Value) {
	liftProps(p.Start.Props, vals)
	for i := range p.Hops {
		liftProps(p.Hops[i].Rel.Props, vals)
		liftProps(p.Hops[i].Node.Props, vals)
	}
}

func liftClause(c ast.Clause, vals *[]value.Value) {
	switch n := c.(type) {
	case *ast.Match:
		for i := range n.Patterns {
			liftPattern(&n.Patterns[i], vals)
		}
		if n.Where != nil {
			liftWhereExpr(n.Where, vals)
		}
	case *ast.With:
		liftProjection(&n.Proj, vals)
		if n.Where != nil {
			liftWhereExpr(n.Where, vals)
		}
	// A path-search/path-bind clause lifts only its pattern's inline
	// props (its WHERE is a post-path filter, left baked like other
	// plan-shaping predicates).
	case *ast.ShortestPath:
		liftPattern(&n.Pattern, vals)
	case *ast.PathBind:
		liftPattern(&n.Pattern, vals)
	// Proc args are part of the analytic's identity (e.g. the rel type
	// for WCC), not a looked-up value -- leave them baked.
	case *ast.CallProc:
	// The FOR list is a projection-like constant (lifting it would change
	// the row set, not just a looked-up value), so leave it baked;
	// recurse only for nested subpatterns.
	case *ast.Unwind:
		liftExpr(n.Expr, vals)
	// A CALL { subquery } body lifts in place too, with its slots in the
	// same global sequence, so the cached template matches and the values
	// resolve at execution.
	case *ast.CallSubquery:
		liftParts(n.Query.Parts, vals)
	}
}

func liftProjection(p *ast.Projection, vals *[]value.Value) {
	for i := range p.Items {
		liftExpr(p.Items[i].Expr, vals)
	}
	for i := range p.OrderBy {
		liftExpr(p.OrderBy[i].Expr, vals)
	}
}

// liftWhereExpr lifts constant comparison bounds in a WHERE/predicate
// position: a `prop <cmp> lit` (either operand order) lifts the literal --
// the date/threshold positions that vary across parameter sets. Recurses
// through AND/OR/NOT and into EXISTS/COUNT subqueries; anything else (a
// CASE, a function call) defers to liftExpr, which lifts only nested
// subpatterns. So a CASE bin threshold or a scalar function argument stays
// baked (it can shape the plan).
func liftWhereExpr(e ast.Expr, vals *[]value.Value) {
	switch n := e.(type) {
	case *ast.Binary:
		switch {
		case n.Op == ast.OpAnd || n.Op == ast.OpOr:
			liftWhereExpr(n.LHS, vals)
			liftWhereExpr(n.RHS, vals)
		case isComparison(n.Op):
			if lit := propCmpLit(n.LHS, n.RHS); lit != nil {
				liftLit(lit, vals)
			} else {
				liftWhereExpr(n.LHS, vals)
				liftWhereExpr(n.RHS, vals)
			}
		default:
			liftExpr(n, vals)
		}
	case *ast.Unary:
		if n.Op == ast.Not {
			liftWhereExpr(n.Expr, vals)
		} else {
			liftExpr(n, vals)
		}
	case *ast.Exists:
		liftPattern(n.Pattern, vals)
		if n.Where != nil {
			liftWhereExpr(n.Where, vals)
		}
	case *ast.CountSub:
		liftPattern(n.Pattern, vals)
		if n.Where != nil {
			liftWhereExpr(n.Where, vals)
		}
	default:
		liftExpr(e, vals)
	}
}

// propCmpLit returns the literal of a `prop <cmp> lit` / `lit <cmp> prop`
// comparison, nil for any other operand shape.
func propCmpLit(lhs, rhs ast.Expr) *ast.Literal {
	if _, ok := lhs.(*ast.Prop); ok {
		if lit, ok := rhs.(*ast.Lit); ok {
			return &lit.Value
		}
	}
	if _, ok := rhs.(*ast.Prop); ok {
		if lit, ok := lhs.(*ast.Lit); ok {
			return &lit.Value
		}
	}
	return nil
}

// liftExpr recurses an expression to lift inline props inside EXISTS {} /
// COUNT {} / pattern-comprehension subpatterns (and the predicate
// constants of those subqueries). The expression's own literals
// (CASE/projection constants) are intentionally not lifted.
func liftExpr(e ast.Expr, vals *[]value.Value) {
	switch n := e.(type) {
	case *ast.Unary:
		liftExpr(n.Expr, vals)
	case *ast.Binary:
		liftExpr(n.LHS, vals)
		liftExpr(n.RHS, vals)
	case *ast.Func:
		for _, a := range n.Args {
			liftExpr(a, vals)
		}
	case *ast.ListExpr:
		for _, el := range n.Elems {
			liftExpr(el, vals)
		}
	case *ast.In:
		liftExpr(n.Expr, vals)
		liftExpr(n.List, vals)
	case *ast.IsNull:
		liftExpr(n.Expr, vals)
	case *ast.IsTruth:
		liftExpr(n.Expr, vals)
	case *ast.IsTyped:
		liftExpr(n.Expr, vals)
	case *ast.Case:
		if n.Operand != nil {
			liftExpr(n.Operand, vals)
		}
		for _, w := range n.Whens {
			liftExpr(w.Cond, vals)
			liftExpr(w.Result, vals)
		}
		if n.Else != nil {
			liftExpr(n.Else, vals)
		}
	case *ast.Exists:
		liftPattern(n.Pattern, vals)
		if n.Where != nil {
			liftWhereExpr(n.Where, vals)
		}
	case *ast.CountSub:
		liftPattern(n.Pattern, vals)
		if n.Where != nil {
			liftWhereExpr(n.Where, vals)
		}
	case *ast.PatternComp:
		liftPattern(n.Pattern, vals)
		if n.Where != nil {
			liftWhereExpr(n.Where, vals)
		}
		liftExpr(n.Proj, vals)
	// The list-predicate window bounds stay baked (a var-length per-hop
	// predicate is small and rarely templated); recurse for nested
	// subpatterns.
	case *ast.ListPred:
		liftExpr(n.List, vals)
		liftExpr(n.Pred, vals)
	case *ast.Reduce:
		liftExpr(n.Init, vals)
		liftExpr(n.List, vals)
		liftExpr(n.Body, vals)
	case *ast.ListComp:
		liftExpr(n.List, vals)
		if n.Filter != nil {
			liftExpr(n.Filter, vals)
		}
		if n.Map != nil {
			liftExpr(n.Map, vals)
		}
	case *ast.Index:
		liftExpr(n.Base, vals)
		liftExpr(n.Idx, vals)
	case *ast.Slice:
		liftExpr(n.Base, vals)
		if n.From != nil {
			liftExpr(n.From, vals)
		}
		if n.To != nil {
			liftExpr(n.To, vals)
		}
	case *ast.PropOf:
		liftExpr(n.Base, vals)
	// A map projection's computed fields can hold nested subpatterns; the
	// .key entries and the base var carry no liftable constants.
	case *ast.MapProj:
		for _, en := range n.Entries {
			if en.Kind == ast.MapProjField {
				liftExpr(en.Expr, vals)
			}
		}
	// A map literal's values are projection-like constants (left baked);
	// recurse only for nested subpatterns within them.
	case *ast.MapLit:
		for _, f := range n.Fields {
			liftExpr(f.Val, vals)
		}
	}
	// Leaves (Lit, Var, Prop, Cost, HasLabelExpr) hold nothing liftable.
}
