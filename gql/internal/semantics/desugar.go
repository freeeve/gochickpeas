// Desugar: query normalization that runs once on the parsed AST before
// planning, shrinking the surface the planner and executor must handle
// (port of desugar.rs). Lowers non-literal inline pattern property values:
// a constant value ((t:Tag {name: 'Hot'}), {id: $p}) stays on the fast
// props seek path, but an expression value ((t:Tag {name: tagVar}),
// {since: a.created}) is rewritten to a WHERE var.key = value equality
// conjunct on the (possibly synthesized) node/relationship variable, so
// pattern matching never evaluates an expression inside a pattern.
//
// Runs at the top of planning, so every entry -- cached, prepared,
// EXPLAIN, and a recursively planned CALL { ... } body -- is normalized
// uniformly. Idempotent: a second pass finds the PropExprs already
// drained.
package semantics

import (
	"fmt"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// Desugar normalizes q in place. A synthetic-variable counter threads
// through so an anonymous node/relationship carrying a non-literal inline
// property gets a stable name to compare on.
func Desugar(q *ast.Query) error {
	ctr := 0
	return desugarParts(q.Parts, &ctr)
}

func desugarParts(parts []ast.QueryPart, ctr *int) error {
	for i := range parts {
		part := &parts[i]
		for _, clause := range part.Clauses {
			if err := desugarClause(clause, ctr); err != nil {
				return err
			}
		}
		if err := desugarProjection(&part.Ret, ctr); err != nil {
			return err
		}
	}
	return nil
}

func desugarClause(c ast.Clause, ctr *int) error {
	switch n := c.(type) {
	case *ast.Match:
		for i := range n.Patterns {
			if err := lowerPatternProps(&n.Patterns[i], &n.Where, ctr); err != nil {
				return err
			}
		}
		return desugarExpr(n.Where, ctr)
	case *ast.With:
		if err := desugarProjection(&n.Proj, ctr); err != nil {
			return err
		}
		return desugarExpr(n.Where, ctr)
	case *ast.ShortestPath:
		if err := lowerPatternProps(&n.Pattern, &n.Where, ctr); err != nil {
			return err
		}
		if n.Weight != nil && n.Weight.Kind == ast.CostExpr {
			if err := desugarExpr(n.Weight.Expr, ctr); err != nil {
				return err
			}
		}
		return desugarExpr(n.Where, ctr)
	case *ast.PathBind:
		if err := lowerPatternProps(&n.Pattern, &n.Where, ctr); err != nil {
			return err
		}
		return desugarExpr(n.Where, ctr)
	case *ast.CallProc:
		for _, a := range n.Args {
			if err := desugarExpr(a, ctr); err != nil {
				return err
			}
		}
		return nil
	case *ast.Unwind:
		return desugarExpr(n.Expr, ctr)
	case *ast.CallSubquery:
		return desugarParts(n.Query.Parts, ctr)
	}
	return nil
}

func desugarProjection(p *ast.Projection, ctr *int) error {
	for i := range p.Items {
		if err := desugarExpr(p.Items[i].Expr, ctr); err != nil {
			return err
		}
	}
	for i := range p.OrderBy {
		if err := desugarExpr(p.OrderBy[i].Expr, ctr); err != nil {
			return err
		}
	}
	return nil
}

// desugarExpr recurses an expression, lowering inline props inside
// EXISTS {} / COUNT {} / pattern-comprehension subpatterns into the
// subquery's own WHERE. Those are the only expression positions that hold
// a pattern; the shared walker reaches them at any depth. The walker
// visits a mutated node's children after the mutation, so a lowered WHERE
// (including its new conjuncts) is itself desugared, matching the Rust
// recursion order.
func desugarExpr(e ast.Expr, ctr *int) error {
	var err error
	ast.Walk(e, func(x ast.Expr) bool {
		switch n := x.(type) {
		case *ast.Exists:
			err = lowerPatternProps(n.Pattern, &n.Where, ctr)
		case *ast.CountSub:
			err = lowerPatternProps(n.Pattern, &n.Where, ctr)
		case *ast.PatternComp:
			err = lowerPatternProps(n.Pattern, &n.Where, ctr)
		}
		return err == nil
	})
	return err
}

// lowerPatternProps lowers every non-literal inline property in pattern to
// an equality conjunct ANDed into where.
func lowerPatternProps(p *ast.Pattern, where *ast.Expr, ctr *int) error {
	lowerNodeProps(&p.Start, where, ctr)
	for i := range p.Hops {
		if err := lowerRelProps(&p.Hops[i].Rel, where, ctr); err != nil {
			return err
		}
		lowerNodeProps(&p.Hops[i].Node, where, ctr)
	}
	return nil
}

func lowerNodeProps(n *ast.NodePat, where *ast.Expr, ctr *int) {
	// An inline element predicate ((v WHERE expr)) conjoins onto the
	// clause WHERE -- the predicate may reference any clause variable, so
	// the clause boundary is its ISO evaluation point anyway.
	if n.Where != nil {
		andInto(where, n.Where)
		n.Where = nil
	}
	if len(n.PropExprs) == 0 {
		return
	}
	v := ensureVar(&n.Var, ctr)
	for _, pe := range n.PropExprs {
		andInto(where, eqConj(v, pe.Key, pe.Val))
	}
	n.PropExprs = nil
}

func lowerRelProps(r *ast.RelPat, where *ast.Expr, ctr *int) error {
	if r.Where != nil {
		if r.Length != nil {
			return planErrf("an inline predicate on a variable-length relationship is not supported (Tier 1)")
		}
		andInto(where, r.Where)
		r.Where = nil
	}
	if len(r.PropExprs) == 0 {
		return nil
	}
	if r.Length != nil {
		return planErrf("a non-literal inline property on a variable-length relationship is not supported (Tier 1)")
	}
	v := ensureVar(&r.Var, ctr)
	for _, pe := range r.PropExprs {
		andInto(where, eqConj(v, pe.Key, pe.Val))
	}
	r.PropExprs = nil
	return nil
}

// ensureVar is the variable a lowered equality compares on: the pattern
// element's own name, or a fresh __ip{n} for an anonymous element
// (deterministic, so identical queries desugar identically).
func ensureVar(slot *string, ctr *int) string {
	if *slot != "" {
		return *slot
	}
	v := fmt.Sprintf("__ip%d", *ctr)
	*ctr++
	*slot = v
	return v
}

func eqConj(v, key string, val ast.Expr) ast.Expr {
	return &ast.Binary{
		Op:  ast.OpEq,
		LHS: &ast.Prop{Var: v, Key: key},
		RHS: val,
	}
}

func andInto(where *ast.Expr, conj ast.Expr) {
	if *where == nil {
		*where = conj
		return
	}
	*where = &ast.Binary{Op: ast.OpAnd, LHS: *where, RHS: conj}
}
