// Binder helpers shared by the planner: aggregate detection, column
// naming, and reference validation against a slot map (port of binder.rs).
// The per-segment compilation that uses these lives in the plan package.
package semantics

import (
	"maps"
	"strings"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// aggNames are the aggregate functions the engine recognises. Execution
// support is a subset (see plan/exec); collect binds but may be rejected
// at planning.
var aggNames = []string{"count", "sum", "avg", "min", "max", "collect"}

// scalarFuncs are the scalar functions the evaluator resolves (the eval
// FuncOp table plus the graph-resolved startNode/endNode), lowercased.
// M14's eval must keep its resolution table in sync with this set.
var scalarFuncs = map[string]struct{}{
	"date": {}, "datetime": {}, "localdatetime": {}, "duration": {},
	"length": {}, "nodes": {}, "rels": {}, "relationships": {},
	"size": {}, "range": {}, "left": {}, "right": {}, "substring": {},
	"id": {}, "abs": {}, "ceil": {}, "floor": {}, "round": {},
	"sign": {}, "sqrt": {}, "tofloat": {}, "tointeger": {},
	"tostring": {}, "toboolean": {}, "coalesce": {},
	"startnode": {}, "endnode": {},
}

// IsAggName reports whether name is an aggregate function
// (case-insensitive).
func IsAggName(name string) bool {
	for _, k := range aggNames {
		if strings.EqualFold(name, k) {
			return true
		}
	}
	return false
}

// IsKnownScalarFunc reports whether name is a scalar function the engine
// can evaluate (case-insensitive).
func IsKnownScalarFunc(name string) bool {
	_, ok := scalarFuncs[strings.ToLower(name)]
	return ok
}

// IsKnownFunction reports whether name is any function the engine
// recognizes -- an aggregate or a scalar. Unknown names are rejected at
// plan time by CheckRefs; without this they would evaluate to null per
// row, silently nulling columns and filtering rows.
func IsKnownFunction(name string) bool {
	return IsAggName(name) || IsKnownScalarFunc(name)
}

// DerivedName derives a column name for an unaliased projection expression
// (the standard uses the source text; this is a close-enough
// reconstruction, observable in result column names).
func DerivedName(e ast.Expr) string {
	switch n := e.(type) {
	case *ast.Var:
		return n.Name
	case *ast.Prop:
		return n.Var + "." + n.Key
	case *ast.Func:
		if n.Star {
			return n.Name + "(*)"
		}
		inner := make([]string, len(n.Args))
		for i, a := range n.Args {
			inner[i] = DerivedName(a)
		}
		return n.Name + "(" + strings.Join(inner, ", ") + ")"
	default:
		return "expr"
	}
}

// ExprHasAgg reports whether e contains an aggregate call. Deliberately
// does NOT descend into EXISTS/COUNT subqueries (their expressions are a
// different scope), mirroring the Rust binder exactly -- so the shared
// pre-order walker is not used here.
func ExprHasAgg(e ast.Expr) bool {
	switch n := e.(type) {
	case *ast.Func:
		if IsAggName(n.Name) {
			return true
		}
		for _, a := range n.Args {
			if ExprHasAgg(a) {
				return true
			}
		}
		return false
	case *ast.Unary:
		return ExprHasAgg(n.Expr)
	case *ast.Binary:
		return ExprHasAgg(n.LHS) || ExprHasAgg(n.RHS)
	case *ast.In:
		return ExprHasAgg(n.Expr) || ExprHasAgg(n.List)
	case *ast.IsNull:
		return ExprHasAgg(n.Expr)
	case *ast.ListExpr:
		for _, el := range n.Elems {
			if ExprHasAgg(el) {
				return true
			}
		}
		return false
	case *ast.ListPred:
		return ExprHasAgg(n.List) || ExprHasAgg(n.Pred)
	case *ast.Reduce:
		return ExprHasAgg(n.Init) || ExprHasAgg(n.List) || ExprHasAgg(n.Body)
	case *ast.ListComp:
		return ExprHasAgg(n.List) ||
			(n.Filter != nil && ExprHasAgg(n.Filter)) ||
			(n.Map != nil && ExprHasAgg(n.Map))
	case *ast.Index:
		return ExprHasAgg(n.Base) || ExprHasAgg(n.Idx)
	case *ast.Slice:
		return ExprHasAgg(n.Base) ||
			(n.From != nil && ExprHasAgg(n.From)) ||
			(n.To != nil && ExprHasAgg(n.To))
	case *ast.PropOf:
		return ExprHasAgg(n.Base)
	case *ast.Case:
		if n.Operand != nil && ExprHasAgg(n.Operand) {
			return true
		}
		for _, w := range n.Whens {
			if ExprHasAgg(w.Cond) || ExprHasAgg(w.Result) {
				return true
			}
		}
		return n.Else != nil && ExprHasAgg(n.Else)
	// A map literal / projection can hold an aggregate in a computed
	// field, e.g. RETURN person{.name, cnt: count(m)}. Recurse so it is
	// planned as an aggregate instead of silently reaching the scalar
	// evaluator.
	case *ast.MapLit:
		for _, f := range n.Fields {
			if ExprHasAgg(f.Val) {
				return true
			}
		}
		return false
	case *ast.MapProj:
		for _, en := range n.Entries {
			if en.Kind == ast.MapProjField && ExprHasAgg(en.Expr) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// CheckRefs validates that every variable referenced in e is present in
// scope (the planner's name -> slot map) and every called function is
// known.
func CheckRefs(e ast.Expr, scope map[string]int) error {
	switch n := e.(type) {
	case *ast.Var:
		return req(n.Name, scope)
	case *ast.Prop:
		return req(n.Var, scope)
	case *ast.Unary:
		return CheckRefs(n.Expr, scope)
	case *ast.Binary:
		if err := CheckRefs(n.LHS, scope); err != nil {
			return err
		}
		return CheckRefs(n.RHS, scope)
	case *ast.Func:
		if !IsKnownFunction(n.Name) {
			return bindErrf("unknown function `%s`", n.Name)
		}
		for _, a := range n.Args {
			if err := CheckRefs(a, scope); err != nil {
				return err
			}
		}
		return nil
	case *ast.ListExpr:
		for _, el := range n.Elems {
			if err := CheckRefs(el, scope); err != nil {
				return err
			}
		}
		return nil
	case *ast.In:
		if err := CheckRefs(n.Expr, scope); err != nil {
			return err
		}
		return CheckRefs(n.List, scope)
	case *ast.IsNull:
		return CheckRefs(n.Expr, scope)
	case *ast.Case:
		if n.Operand != nil {
			if err := CheckRefs(n.Operand, scope); err != nil {
				return err
			}
		}
		for _, w := range n.Whens {
			if err := CheckRefs(w.Cond, scope); err != nil {
				return err
			}
			if err := CheckRefs(w.Result, scope); err != nil {
				return err
			}
		}
		if n.Else != nil {
			return CheckRefs(n.Else, scope)
		}
		return nil
	case *ast.Exists:
		return checkSubqueryRefs(n.Pattern, n.Where, scope)
	case *ast.CountSub:
		return checkSubqueryRefs(n.Pattern, n.Where, scope)
	case *ast.PatternComp:
		// The pattern's node/rel variables are bound for both the filter
		// and projection; outer variables remain in scope.
		inner := scopeWithPatternVars(scope, n.Pattern)
		if n.Where != nil {
			if err := CheckRefs(n.Where, inner); err != nil {
				return err
			}
		}
		return CheckRefs(n.Proj, inner)
	case *ast.Cost:
		if err := req(n.From, scope); err != nil {
			return err
		}
		return req(n.To, scope)
	case *ast.ListPred:
		// The iteration variable is in scope only for the inner predicate.
		if err := CheckRefs(n.List, scope); err != nil {
			return err
		}
		inner := cloneScope(scope)
		addVar(inner, n.Var)
		return CheckRefs(n.Pred, inner)
	case *ast.Reduce:
		if err := CheckRefs(n.Init, scope); err != nil {
			return err
		}
		if err := CheckRefs(n.List, scope); err != nil {
			return err
		}
		inner := cloneScope(scope)
		addVar(inner, n.Acc)
		addVar(inner, n.Var)
		return CheckRefs(n.Body, inner)
	case *ast.ListComp:
		// The iteration variable is in scope only for the filter and map;
		// the source list is validated in the outer scope.
		if err := CheckRefs(n.List, scope); err != nil {
			return err
		}
		inner := cloneScope(scope)
		addVar(inner, n.Var)
		if n.Filter != nil {
			if err := CheckRefs(n.Filter, inner); err != nil {
				return err
			}
		}
		if n.Map != nil {
			return CheckRefs(n.Map, inner)
		}
		return nil
	case *ast.Index:
		if err := CheckRefs(n.Base, scope); err != nil {
			return err
		}
		return CheckRefs(n.Idx, scope)
	case *ast.Slice:
		if err := CheckRefs(n.Base, scope); err != nil {
			return err
		}
		if n.From != nil {
			if err := CheckRefs(n.From, scope); err != nil {
				return err
			}
		}
		if n.To != nil {
			return CheckRefs(n.To, scope)
		}
		return nil
	case *ast.PropOf:
		return CheckRefs(n.Base, scope)
	case *ast.MapProj:
		// Reads the base var and each computed field; .key entries only
		// read the base var's properties.
		if err := req(n.Var, scope); err != nil {
			return err
		}
		for _, en := range n.Entries {
			if en.Kind == ast.MapProjField {
				if err := CheckRefs(en.Expr, scope); err != nil {
					return err
				}
			}
		}
		return nil
	case *ast.MapLit:
		for _, f := range n.Fields {
			if err := CheckRefs(f.Val, scope); err != nil {
				return err
			}
		}
		return nil
	case *ast.HasLabelExpr:
		return req(n.Var, scope)
	}
	// Literals bind no references.
	return nil
}

// CheckRefsSkippingAgg is CheckRefs, except a top-level aggregate call's
// arguments are validated directly (an aggregate's own name needs no
// scalar-function check, and count(*) binds nothing).
func CheckRefsSkippingAgg(e ast.Expr, scope map[string]int) error {
	if f, ok := e.(*ast.Func); ok && IsAggName(f.Name) {
		for _, a := range f.Args {
			if err := CheckRefs(a, scope); err != nil {
				return err
			}
		}
		return nil
	}
	return CheckRefs(e, scope)
}

// checkSubqueryRefs validates an EXISTS/COUNT subquery's WHERE: it may
// reference outer variables plus the pattern's own.
func checkSubqueryRefs(p *ast.Pattern, where ast.Expr, scope map[string]int) error {
	if where == nil {
		return nil
	}
	return CheckRefs(where, scopeWithPatternVars(scope, p))
}

// scopeWithPatternVars clones scope and adds the pattern's node and rel
// variables (slot 0; subquery variables are matched, not slotted).
func scopeWithPatternVars(scope map[string]int, p *ast.Pattern) map[string]int {
	inner := cloneScope(scope)
	addVar(inner, p.Start.Var)
	for i := range p.Hops {
		addVar(inner, p.Hops[i].Rel.Var)
		addVar(inner, p.Hops[i].Node.Var)
	}
	return inner
}

func cloneScope(scope map[string]int) map[string]int {
	inner := make(map[string]int, len(scope)+4)
	maps.Copy(inner, scope)
	return inner
}

// addVar adds a pattern-introduced variable to a scope, keeping an
// existing (outer) slot.
func addVar(scope map[string]int, v string) {
	if v == "" {
		return
	}
	if _, ok := scope[v]; !ok {
		scope[v] = 0
	}
}

func req(v string, scope map[string]int) error {
	if _, ok := scope[v]; ok {
		return nil
	}
	return bindErrf("unknown variable `%s`", v)
}
