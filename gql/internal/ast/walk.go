// The shared expression walker: desugar, the binder, and autoparam all
// traverse expressions with it (including the pattern-carrying subquery
// nodes' inner WHERE/projection expressions).
package ast

// Walk visits e and every sub-expression in pre-order. Returning false
// from f skips the node's children (the node itself was already visited).
// Pattern-carrying nodes (Exists/CountSub/PatternComp) descend into their
// inner WHERE / projection expressions and the patterns' inline property
// expressions, so a pass that rewrites literals or checks references sees
// every expression a query evaluates.
func Walk(e Expr, f func(Expr) bool) {
	if e == nil || !f(e) {
		return
	}
	switch n := e.(type) {
	case *Unary:
		Walk(n.Expr, f)
	case *Binary:
		Walk(n.LHS, f)
		Walk(n.RHS, f)
	case *Func:
		for _, a := range n.Args {
			Walk(a, f)
		}
	case *ListExpr:
		for _, el := range n.Elems {
			Walk(el, f)
		}
	case *In:
		Walk(n.Expr, f)
		Walk(n.List, f)
	case *IsNull:
		Walk(n.Expr, f)
	case *Case:
		Walk(n.Operand, f)
		for _, w := range n.Whens {
			Walk(w.Cond, f)
			Walk(w.Result, f)
		}
		Walk(n.Else, f)
	case *Cost:
		if n.Weight.Kind == CostExpr {
			Walk(n.Weight.Expr, f)
		}
	case *Exists:
		walkPattern(n.Pattern, f)
		Walk(n.Where, f)
	case *CountSub:
		walkPattern(n.Pattern, f)
		Walk(n.Where, f)
	case *ListPred:
		Walk(n.List, f)
		Walk(n.Pred, f)
	case *Reduce:
		Walk(n.Init, f)
		Walk(n.List, f)
		Walk(n.Body, f)
	case *ListComp:
		Walk(n.List, f)
		Walk(n.Filter, f)
		Walk(n.Map, f)
	case *PatternComp:
		walkPattern(n.Pattern, f)
		Walk(n.Where, f)
		Walk(n.Proj, f)
	case *Index:
		Walk(n.Base, f)
		Walk(n.Idx, f)
	case *Slice:
		Walk(n.Base, f)
		Walk(n.From, f)
		Walk(n.To, f)
	case *PropOf:
		Walk(n.Base, f)
	case *MapProj:
		for _, en := range n.Entries {
			if en.Kind == MapProjField {
				Walk(en.Expr, f)
			}
		}
	case *MapLit:
		for _, fl := range n.Fields {
			Walk(fl.Val, f)
		}
	}
}

// walkPattern visits the inline property expressions of a pattern's nodes
// and rels.
func walkPattern(p *Pattern, f func(Expr) bool) {
	if p == nil {
		return
	}
	for _, pe := range p.Start.PropExprs {
		Walk(pe.Val, f)
	}
	for i := range p.Hops {
		for _, pe := range p.Hops[i].Rel.PropExprs {
			Walk(pe.Val, f)
		}
		for _, pe := range p.Hops[i].Node.PropExprs {
			Walk(pe.Val, f)
		}
	}
}
