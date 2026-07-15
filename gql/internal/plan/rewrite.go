// Clause rewrite pass (port of the Rust plan/rewrite.rs, keeping only
// fuse_projection_before_aggregate -- the window-count fusion belonged to
// the skipped recognizer kernels): a pure-projection boundary immediately
// followed by an aggregating boundary is inlined into the aggregate, so
// the preceding MATCH's scan and the aggregation land in one segment.
package plan

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

// pureProjectionSubst is the alias -> definition substitution a pure
// projection contributes when fused into the next clause: an entry per
// computed or renamed column, keyed by alias. Identity pass-throughs
// contribute nothing. ok=false unless the projection is a plain 1:1 map
// (no * / DISTINCT / ORDER BY / OFFSET / LIMIT, no aggregate, every item a
// bare variable or explicitly aliased, no duplicate output name).
func pureProjectionSubst(proj *ast.Projection) (map[string]ast.Expr, bool) {
	if proj.Star || proj.Distinct || len(proj.OrderBy) > 0 || proj.Skip != nil || proj.Limit != nil {
		return nil, false
	}
	subst := map[string]ast.Expr{}
	names := map[string]bool{}
	for _, item := range proj.Items {
		if semantics.ExprHasAgg(item.Expr) {
			return nil, false
		}
		var name string
		v, isVar := item.Expr.(*ast.Var)
		switch {
		case item.Alias != "":
			name = item.Alias
		case isVar:
			name = v.Name
		default:
			// An unaliased non-variable projection has no identifier a
			// later clause can reference.
			return nil, false
		}
		if names[name] {
			return nil, false
		}
		names[name] = true
		identity := isVar && v.Name == name
		if !identity {
			subst[name] = item.Expr
		}
	}
	return subst, true
}

// projectionIsAggregated reports whether a projection aggregates.
func projectionIsAggregated(proj *ast.Projection) bool {
	if proj.Star {
		return false
	}
	for _, it := range proj.Items {
		if semantics.ExprHasAgg(it.Expr) {
			return true
		}
	}
	return false
}

// substExpr substitutes inlined aliases into e, returning the rewritten
// expression; ok=false when e contains a construct this pass does not
// rewrite (a subquery / scoped expression), abandoning the fusion. A
// property/label test on an inlined alias is only rewritable when the
// alias maps to a bare variable.
func substExpr(e ast.Expr, subst map[string]ast.Expr) (ast.Expr, bool) {
	switch n := e.(type) {
	case *ast.Lit:
		return e, true
	case *ast.Var:
		if rep, ok := subst[n.Name]; ok {
			return rep, true
		}
		return e, true
	case *ast.Prop:
		rep, ok := subst[n.Var]
		if !ok {
			return e, true
		}
		if v, isVar := rep.(*ast.Var); isVar {
			return &ast.Prop{Var: v.Name, Key: n.Key}, true
		}
		return nil, false
	case *ast.HasLabelExpr:
		rep, ok := subst[n.Var]
		if !ok {
			return e, true
		}
		if v, isVar := rep.(*ast.Var); isVar {
			return &ast.HasLabelExpr{Var: v.Name, Expr: n.Expr}, true
		}
		return nil, false
	case *ast.Unary:
		x, ok := substExpr(n.Expr, subst)
		if !ok {
			return nil, false
		}
		return &ast.Unary{Op: n.Op, Expr: x}, true
	case *ast.IsNull:
		x, ok := substExpr(n.Expr, subst)
		if !ok {
			return nil, false
		}
		return &ast.IsNull{Expr: x, Negated: n.Negated}, true
	case *ast.IsTruth:
		x, ok := substExpr(n.Expr, subst)
		if !ok {
			return nil, false
		}
		return &ast.IsNull{Expr: x, Negated: n.Negated}, true
	case *ast.IsTyped:
		x, ok := substExpr(n.Expr, subst)
		if !ok {
			return nil, false
		}
		return &ast.IsNull{Expr: x, Negated: n.Negated}, true
	case *ast.Binary:
		l, ok := substExpr(n.LHS, subst)
		if !ok {
			return nil, false
		}
		r, ok := substExpr(n.RHS, subst)
		if !ok {
			return nil, false
		}
		return &ast.Binary{Op: n.Op, LHS: l, RHS: r}, true
	case *ast.In:
		x, ok := substExpr(n.Expr, subst)
		if !ok {
			return nil, false
		}
		l, ok := substExpr(n.List, subst)
		if !ok {
			return nil, false
		}
		return &ast.In{Expr: x, List: l}, true
	case *ast.Func:
		if n.Star {
			return e, true
		}
		mapped := make([]ast.Expr, len(n.Args))
		for i, a := range n.Args {
			m, ok := substExpr(a, subst)
			if !ok {
				return nil, false
			}
			mapped[i] = m
		}
		return &ast.Func{Name: n.Name, Distinct: n.Distinct, Args: mapped}, true
	case *ast.ListExpr:
		mapped := make([]ast.Expr, len(n.Elems))
		for i, a := range n.Elems {
			m, ok := substExpr(a, subst)
			if !ok {
				return nil, false
			}
			mapped[i] = m
		}
		return &ast.ListExpr{Elems: mapped}, true
	case *ast.Case:
		out := &ast.Case{}
		if n.Operand != nil {
			o, ok := substExpr(n.Operand, subst)
			if !ok {
				return nil, false
			}
			out.Operand = o
		}
		out.Whens = make([]ast.CaseWhen, len(n.Whens))
		for i, w := range n.Whens {
			c, ok := substExpr(w.Cond, subst)
			if !ok {
				return nil, false
			}
			r, ok := substExpr(w.Result, subst)
			if !ok {
				return nil, false
			}
			out.Whens[i] = ast.CaseWhen{Cond: c, Result: r}
		}
		if n.Else != nil {
			x, ok := substExpr(n.Else, subst)
			if !ok {
				return nil, false
			}
			out.Else = x
		}
		return out, true
	}
	// Subqueries and scoped expressions bind their own variables:
	// conservatively refuse to fuse.
	return nil, false
}

// inlineProjection rewrites the aggregating boundary by inlining subst.
// Each unaliased item is pinned to its original derived name first, so
// substitution never changes an output column name. ok=false abandons the
// fusion.
func inlineProjection(proj *ast.Projection, where ast.Expr, subst map[string]ast.Expr) (*ast.With, bool) {
	out := ast.Projection{
		Star:     proj.Star,
		Distinct: proj.Distinct,
		Items:    make([]ast.ReturnItem, len(proj.Items)),
		OrderBy:  make([]ast.SortItem, len(proj.OrderBy)),
		Skip:     proj.Skip,
		Limit:    proj.Limit,
	}
	for i, item := range proj.Items {
		alias := item.Alias
		if alias == "" {
			alias = semantics.DerivedName(item.Expr)
		}
		x, ok := substExpr(item.Expr, subst)
		if !ok {
			return nil, false
		}
		out.Items[i] = ast.ReturnItem{Expr: x, Alias: alias}
	}
	for i, s := range proj.OrderBy {
		x, ok := substExpr(s.Expr, subst)
		if !ok {
			return nil, false
		}
		out.OrderBy[i] = ast.SortItem{Expr: x, Desc: s.Desc}
	}
	var outWhere ast.Expr
	if where != nil {
		x, ok := substExpr(where, subst)
		if !ok {
			return nil, false
		}
		outWhere = x
	}
	return &ast.With{Proj: out, Where: outWhere}, true
}

// fuseProjectionBeforeAggregate fuses a pure-projection boundary followed
// by an aggregating boundary into the aggregate, so the streaming
// aggregator folds matched rows without materializing the projected set.
// Conservative: fires only when the projection is a plain 1:1 map and
// every inlined reference is safely substitutable.
func fuseProjectionBeforeAggregate(clauses []ast.Clause) []ast.Clause {
	out := make([]ast.Clause, 0, len(clauses))
	for i := 0; i < len(clauses); i++ {
		w, ok := clauses[i].(*ast.With)
		if ok && w.Where == nil {
			if subst, pure := pureProjectionSubst(&w.Proj); pure && i+1 < len(clauses) {
				if next, isWith := clauses[i+1].(*ast.With); isWith && projectionIsAggregated(&next.Proj) {
					if fused, okf := inlineProjection(&next.Proj, next.Where, subst); okf {
						out = append(out, fused)
						i++ // consume the aggregating boundary we fused into
						continue
					}
				}
			}
		}
		out = append(out, clauses[i])
	}
	return out
}
