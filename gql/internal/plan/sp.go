// Shortest-path stage lowering (port of the Rust plan/sp.rs): turn an
// ANY/ALL SHORTEST path-search clause into an SpStage, validating the
// optional weight, extracting the per-hop predicate, and resolving the
// bound endpoint slots.
package plan

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

func buildSpStage(spec *stageSpec, slots map[string]int, bound map[int]bool, nextSlot *int) (*SpStage, error) {
	pattern := spec.pattern
	if spec.all && spec.weight != nil {
		return nil, planErrf("ALL SHORTEST does not support weighted path selection")
	}
	if len(pattern.Hops) != 1 {
		kw := "ANY SHORTEST"
		if spec.all {
			kw = "ALL SHORTEST"
		}
		return nil, planErrf("%s requires exactly one relationship (a)-[:T]->{..}(b)", kw)
	}
	rel := &pattern.Hops[0].Rel
	end := &pattern.Hops[0].Node
	from, err := endpointSlot(&pattern.Start, slots)
	if err != nil {
		return nil, err
	}
	to, err := endpointSlot(end, slots)
	if err != nil {
		return nil, err
	}
	var maxHops *uint64
	if rel.Length != nil {
		maxHops = rel.Length.Max
	}
	relPred, err := extractSpHopPred(spec.where, rel.Var)
	if err != nil {
		return nil, err
	}
	// A per-edge weight expression evaluates in the edge scope: it may
	// reference only the path's relationship variable (bound per hop).
	weightVar := ""
	if spec.weight != nil && spec.weight.Kind == ast.CostExpr {
		if rel.Var == "" {
			return nil, planErrf("a weighted path-search weight expression requires a named relationship variable")
		}
		if err := validateWeightExpr(spec.weight.Expr, []string{rel.Var}); err != nil {
			return nil, err
		}
		weightVar = rel.Var
	}
	pathSlot := *nextSlot
	*nextSlot++
	slots[spec.pathVar] = pathSlot
	bound[pathSlot] = true
	return &SpStage{
		PathSlot:  pathSlot,
		From:      from,
		To:        to,
		Dir:       mapDir(rel.Dir),
		Types:     rel.Types,
		Max:       maxHops,
		Optional:  spec.optional,
		All:       spec.all,
		Weight:    spec.weight,
		WeightVar: weightVar,
		RelPred:   relPred,
	}, nil
}

// validateWeightExpr checks that a per-edge weight references only the
// allowed variables (the rel variable, plus subquery-pattern-bound ones in
// scope). A correlated EXISTS/COUNT subquery is permitted: its pattern
// variables extend the allowed set for its WHERE.
func validateWeightExpr(e ast.Expr, allowed []string) error {
	bad := weightExprCheck(e, allowed)
	if bad != "" {
		return planErrf("a weighted path-search weight expression may reference only the relationship variable `%s` (per-edge), optionally via correlated EXISTS/COUNT subqueries: %s", allowed[0], bad)
	}
	return nil
}

// weightExprCheck returns a description of the first disallowed reference
// or construct ("" when clean).
func weightExprCheck(e ast.Expr, allowed []string) string {
	checkVar := func(v string) string {
		for _, a := range allowed {
			if a == v {
				return ""
			}
		}
		return "references `" + v + "`"
	}
	switch n := e.(type) {
	case *ast.Lit:
		return ""
	case *ast.Var:
		return checkVar(n.Name)
	case *ast.Prop:
		return checkVar(n.Var)
	case *ast.Unary:
		return weightExprCheck(n.Expr, allowed)
	case *ast.IsNull:
		return weightExprCheck(n.Expr, allowed)
	case *ast.Binary:
		if bad := weightExprCheck(n.LHS, allowed); bad != "" {
			return bad
		}
		return weightExprCheck(n.RHS, allowed)
	case *ast.Func:
		for _, a := range n.Args {
			if bad := weightExprCheck(a, allowed); bad != "" {
				return bad
			}
		}
		return ""
	case *ast.ListExpr:
		for _, a := range n.Elems {
			if bad := weightExprCheck(a, allowed); bad != "" {
				return bad
			}
		}
		return ""
	case *ast.In:
		if bad := weightExprCheck(n.Expr, allowed); bad != "" {
			return bad
		}
		return weightExprCheck(n.List, allowed)
	case *ast.Case:
		if n.Operand != nil {
			if bad := weightExprCheck(n.Operand, allowed); bad != "" {
				return bad
			}
		}
		for _, w := range n.Whens {
			if bad := weightExprCheck(w.Cond, allowed); bad != "" {
				return bad
			}
			if bad := weightExprCheck(w.Result, allowed); bad != "" {
				return bad
			}
		}
		if n.Else != nil {
			return weightExprCheck(n.Else, allowed)
		}
		return ""
	case *ast.Index:
		if bad := weightExprCheck(n.Base, allowed); bad != "" {
			return bad
		}
		return weightExprCheck(n.Idx, allowed)
	case *ast.Slice:
		if bad := weightExprCheck(n.Base, allowed); bad != "" {
			return bad
		}
		if n.From != nil {
			if bad := weightExprCheck(n.From, allowed); bad != "" {
				return bad
			}
		}
		if n.To != nil {
			return weightExprCheck(n.To, allowed)
		}
		return ""
	case *ast.PropOf:
		return weightExprCheck(n.Base, allowed)
	case *ast.Exists:
		return weightSubqueryCheck(n.Pattern, n.Where, allowed)
	case *ast.CountSub:
		return weightSubqueryCheck(n.Pattern, n.Where, allowed)
	}
	return "unsupported weight form"
}

// weightSubqueryCheck validates a correlated subquery weight: its pattern
// variables are local, so its WHERE checks against the extended set.
func weightSubqueryCheck(p *ast.Pattern, where ast.Expr, allowed []string) string {
	if where == nil {
		return ""
	}
	ext := append(append([]string{}, allowed...), patternVarNames(p)...)
	return weightExprCheck(where, ext)
}

// extractSpHopPred lifts the all(r IN rels(e) WHERE ...) per-hop predicate
// from a path-search WHERE; the whole WHERE must reduce to that one form.
func extractSpHopPred(where ast.Expr, relVar string) (*RelHopPred, error) {
	if where == nil {
		return nil, nil
	}
	var conjs []ast.Expr
	splitAndRef(where, &conjs)
	var found *RelHopPred
	for _, c := range conjs {
		lp, ok := c.(*ast.ListPred)
		if ok && relVar != "" && relsArg(lp.List) == relVar {
			if lp.Quant != ast.QuantAll {
				return nil, planErrf("only `all(r IN rels(e) WHERE ...)` is supported over a path-search relationship")
			}
			if err := predRefsOnly(lp.Pred, lp.Var); err != nil {
				return nil, err
			}
			if found != nil {
				return nil, planErrf("multiple per-hop predicates on one path search are not supported")
			}
			found = &RelHopPred{Var: lp.Var, Pred: lp.Pred}
			continue
		}
		return nil, planErrf("a WHERE on a path search is only supported as `all(r IN rels(e) WHERE ...)` over its relationship variable")
	}
	return found, nil
}

// endpointSlot resolves a path-search endpoint to its bound slot.
func endpointSlot(node *ast.NodePat, slots map[string]int) (int, error) {
	if node.Var == "" {
		return 0, planErrf("path-search endpoints must be named, bound variables (Tier 1)")
	}
	s, ok := slots[node.Var]
	if !ok {
		return 0, planErrf("path-search endpoint `%s` must be a bound variable (Tier 1)", node.Var)
	}
	return s, nil
}
