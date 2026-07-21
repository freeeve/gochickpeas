// Correlated-subquery decorrelation setup: recognizing a cheap bound-pair
// EXISTS probe, computing a subquery's outer-slot memo key, and marking a
// COUNT{} decorrelatable when it is a fixed linear chain between two
// outer-bound endpoints with no other outer dependency (evaluated once per
// anchor instead of a DFS per row). Split from compile.go, which holds the
// compiled-node types and the expression compiler.
package compile

import "github.com/freeeve/gochickpeas/gql/internal/ast"

// cheapExistsProbe reports the subquery shape whose evaluation is a
// near-constant-time bound-pair probe: one fixed-length hop, both
// endpoint variables bound in the outer scope, no inner WHERE.
func cheapExistsProbe(p *ast.Pattern, where ast.Expr, slots map[string]int) bool {
	if where != nil || len(p.Hops) != 1 || p.Hops[0].Rel.Length != nil {
		return false
	}
	startBound := p.Start.Var != "" && hasSlot(slots, p.Start.Var)
	endBound := p.Hops[0].Node.Var != "" && hasSlot(slots, p.Hops[0].Node.Var)
	return startBound && endBound
}

func hasSlot(m map[string]int, k string) bool {
	_, ok := m[k]
	return ok
}

// correlatedSlots is the outer slots a correlated subquery reads -- its
// memo key. ok=false when the dependency set can't be fully determined (a
// nested subquery), which disables memoization (still correct, evaluated
// per row).
func correlatedSlots(p *ast.Pattern, where ast.Expr, slots map[string]int) ([]int, bool) {
	seen := map[int]struct{}{}
	add := func(v string) {
		if v == "" {
			return
		}
		if s, ok := slots[v]; ok {
			seen[s] = struct{}{}
		}
	}
	add(p.Start.Var)
	for i := range p.Hops {
		add(p.Hops[i].Rel.Var)
		add(p.Hops[i].Node.Var)
	}
	ok := true
	if where != nil {
		ok = collectOuterSlots(where, slots, add)
	}
	out := make([]int, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	// Deterministic key order.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if !ok {
		return nil, false
	}
	return out, true
}

// setupDecor marks cs decorrelatable and records its endpoints when the
// COUNT{} is a linear chain whose two endpoints are BOTH bound to outer
// variables and whose only outer dependencies are those endpoints. Such a
// subquery is a per-endpoint aggregate over an anchored set: it can be
// evaluated once per distinct anchor value (grouped by the other endpoint)
// instead of a DFS per outer row (task 084). Conservative -- any structure the
// single-scan rewrite would not reproduce exactly (an inner node that is
// itself an outer variable, a bound relationship variable, a variable-length
// hop, or a WHERE that reads an outer variable other than the two endpoints)
// leaves cs on the existing per-row path.
func setupDecor(cs *cSubquery, p *ast.Pattern, where ast.Expr, slots map[string]int) {
	if len(p.Hops) < 1 {
		return
	}
	startVar := p.Start.Var
	endVar := p.EndNode().Var
	if startVar == "" || endVar == "" || startVar == endVar {
		return
	}
	ss, ok1 := slots[startVar]
	es, ok2 := slots[endVar]
	if !ok1 || !ok2 {
		return
	}
	// Inner nodes must be subquery-local, and no relationship variable may be
	// outer-bound: the single grouped scan enumerates every inner element, so
	// an outer-anchored inner would change which matches it produces.
	for i := range p.Hops {
		if p.Hops[i].Rel.Length != nil {
			return // variable-length hop: v1 handles fixed chains only
		}
		if v := p.Hops[i].Rel.Var; v != "" {
			if _, outer := slots[v]; outer {
				return
			}
		}
		if i < len(p.Hops)-1 { // inner node (not the end endpoint)
			if v := p.Hops[i].Node.Var; v != "" {
				if _, outer := slots[v]; outer {
					return
				}
			}
		}
	}
	// The WHERE may read only the two endpoints (and inner, subquery-local
	// variables). Any other outer variable would make the table depend on a
	// value not captured by the anchor key, so it could not be shared across
	// rows. collectOuterSlots reports false for an unenumerable dependency.
	if where != nil {
		bad := false
		ok := collectOuterSlots(where, slots, func(v string) {
			if s, isOuter := slots[v]; isOuter && s != ss && s != es {
				bad = true
			}
		})
		if !ok || bad {
			return
		}
	}
	cs.decorOK = true
	cs.decorStartVar, cs.decorEndVar = startVar, endVar
	cs.decorStartSlot, cs.decorEndSlot = ss, es
}

// collectOuterSlots adds the outer slots e references; returns false when
// e contains a construct whose outer dependencies can't be enumerated (a
// nested EXISTS/COUNT, or a pattern comprehension's correlation).
func collectOuterSlots(e ast.Expr, slots map[string]int, add func(string)) bool {
	switch n := e.(type) {
	case *ast.Lit:
		return true
	case *ast.Var:
		add(n.Name)
		return true
	case *ast.Prop:
		add(n.Var)
		return true
	case *ast.Cost:
		add(n.From)
		add(n.To)
		return true
	case *ast.Unary:
		return collectOuterSlots(n.Expr, slots, add)
	case *ast.IsNull:
		return collectOuterSlots(n.Expr, slots, add)
	case *ast.Binary:
		return collectOuterSlots(n.LHS, slots, add) && collectOuterSlots(n.RHS, slots, add)
	case *ast.In:
		return collectOuterSlots(n.Expr, slots, add) && collectOuterSlots(n.List, slots, add)
	case *ast.ListPred:
		return collectOuterSlots(n.List, slots, add) && collectOuterSlots(n.Pred, slots, add)
	case *ast.Reduce:
		return collectOuterSlots(n.Init, slots, add) &&
			collectOuterSlots(n.List, slots, add) &&
			collectOuterSlots(n.Body, slots, add)
	case *ast.ListComp:
		ok := collectOuterSlots(n.List, slots, add)
		if n.Filter != nil {
			ok = collectOuterSlots(n.Filter, slots, add) && ok
		}
		if n.Map != nil {
			ok = collectOuterSlots(n.Map, slots, add) && ok
		}
		return ok
	case *ast.ListExpr:
		for _, el := range n.Elems {
			if !collectOuterSlots(el, slots, add) {
				return false
			}
		}
		return true
	case *ast.PatternComp:
		// The comprehension's correlation structure isn't modeled here;
		// collect what the filter/proj reference, then report incomplete.
		if n.Where != nil {
			collectOuterSlots(n.Where, slots, add)
		}
		collectOuterSlots(n.Proj, slots, add)
		return false
	case *ast.Func:
		for _, a := range n.Args {
			if !collectOuterSlots(a, slots, add) {
				return false
			}
		}
		return true
	case *ast.Case:
		ok := true
		if n.Operand != nil {
			ok = collectOuterSlots(n.Operand, slots, add)
		}
		for _, w := range n.Whens {
			ok = collectOuterSlots(w.Cond, slots, add) && ok
			ok = collectOuterSlots(w.Result, slots, add) && ok
		}
		if n.Else != nil {
			ok = collectOuterSlots(n.Else, slots, add) && ok
		}
		return ok
	case *ast.Index:
		return collectOuterSlots(n.Base, slots, add) && collectOuterSlots(n.Idx, slots, add)
	case *ast.Slice:
		ok := collectOuterSlots(n.Base, slots, add)
		if n.From != nil {
			ok = collectOuterSlots(n.From, slots, add) && ok
		}
		if n.To != nil {
			ok = collectOuterSlots(n.To, slots, add) && ok
		}
		return ok
	case *ast.PropOf:
		return collectOuterSlots(n.Base, slots, add)
	case *ast.MapProj:
		add(n.Var)
		for _, en := range n.Entries {
			if en.Kind == ast.MapProjField && !collectOuterSlots(en.Expr, slots, add) {
				return false
			}
		}
		return true
	case *ast.MapLit:
		for _, f := range n.Fields {
			if !collectOuterSlots(f.Val, slots, add) {
				return false
			}
		}
		return true
	case *ast.HasLabelExpr:
		add(n.Var)
		return true
	default:
		// A nested subquery's correlated reads aren't enumerated -> no memo.
		return false
	}
}
