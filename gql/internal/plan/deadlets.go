// Two boundary-expression reductions that compose to delete dead work:
//
//  1. size-of-unfiltered-comprehension: size(ts) where ts's defining
//     boundary item is [x IN S | expr] with no filter rewrites to
//     size(S) -- an unfiltered comprehension maps every element (nulls
//     included), so length is preserved whatever the map expression
//     does. Applied when S is a bare variable still in scope at the
//     reading segment (definitions chase through bare passthrough
//     items).
//  2. dead-LET nulling: a non-aggregated, non-DISTINCT boundary item
//     whose alias no later expression reads -- and which is not a final
//     output -- has its expression replaced by a null literal. The
//     column keeps its position (no slot renumbering, plan shape
//     unchanged); the work is what disappears.
//
// On the trail idiom (FinBench CR1/CR2) the monotonic pushdown has
// already absorbed the boundary filter, so the timestamp comprehension's
// only consumer is min(size(ts)): rule 1 redirects the size to the rel
// list and rule 2 deletes the comprehension -- the corpus's largest
// per-query allocation count (174k/op, 20x the next query) collapses to
// the fill floor.
package plan

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// DisableDeadLets pins differential tests to the unreduced boundaries;
// the counters make engagement falsifiable per rule.
var (
	DisableDeadLets bool
	compLenRewrites int
	deadLetNullings int
)

// reduceDeadLets runs both reductions over one branch's segments.
func reduceDeadLets(segs []*Segment) {
	if DisableDeadLets {
		return
	}
	rewriteComprehensionLengths(segs)
	nullDeadLets(segs)
}

// rewriteComprehensionLengths applies rule 1 across the branch.
func rewriteComprehensionLengths(segs []*Segment) {
	// alias -> source var, per boundary, propagated through bare
	// passthrough items while the source stays carried.
	compSource := map[string]string{}
	for si, seg := range segs {
		if si > 0 {
			// Rewrite reads in this segment's stages and boundary.
			inScope := func(name string) bool {
				_, ok := seg.Slots[name]
				return ok
			}
			rw := func(e ast.Expr) ast.Expr {
				if e == nil {
					return nil
				}
				out, changed := rewriteSizeOf(e, compSource, inScope)
				if changed {
					compLenRewrites++
				}
				return out
			}
			for _, st := range seg.Stages {
				if ms, ok := st.(*MatchStage); ok {
					ms.Where = rw(ms.Where)
				}
			}
			proj := &seg.Proj
			for i := range proj.Returns {
				proj.Returns[i].Expr = rw(proj.Returns[i].Expr)
			}
			for i := range proj.Aggs {
				proj.Aggs[i].Arg = rw(proj.Aggs[i].Arg)
				proj.Aggs[i].Arg2 = rw(proj.Aggs[i].Arg2)
			}
			for i := range proj.Post {
				proj.Post[i].Expr = rw(proj.Post[i].Expr)
			}
			for i := range proj.OrderBy {
				proj.OrderBy[i].Expr = rw(proj.OrderBy[i].Expr)
			}
			seg.PostWhere = rw(seg.PostWhere)
		}
		// Fold this boundary's definitions for downstream segments. A
		// mapping's SOURCE must survive unchanged too: an item that
		// rebinds the source name to anything but a bare passthrough of
		// itself invalidates every mapping through it (size(ts) must
		// measure the list ts was built from, not a later value that
		// happens to share the source's name).
		next := map[string]string{}
		for _, it := range seg.Proj.Returns {
			switch e := it.Expr.(type) {
			case *ast.ListComp:
				if e.Filter == nil {
					if src, ok := e.List.(*ast.Var); ok {
						next[it.Name] = src.Name
					}
				}
			case *ast.Var:
				if src, ok := compSource[e.Name]; ok && it.Name != "" {
					next[it.Name] = src
				}
			}
		}
		for _, it := range seg.Proj.Returns {
			if v, isVar := it.Expr.(*ast.Var); isVar && v.Name == it.Name {
				continue // bare passthrough keeps the source intact
			}
			for alias, src := range next {
				if src == it.Name {
					delete(next, alias)
				}
			}
		}
		compSource = next
	}
}

// rewriteSizeOf substitutes size(alias) -> size(source) wherever the
// alias maps and the source is in scope, rebuilding only touched spines.
func rewriteSizeOf(e ast.Expr, compSource map[string]string, inScope func(string) bool) (ast.Expr, bool) {
	if f, ok := e.(*ast.Func); ok && !f.Star && len(f.Args) == 1 && eqFold(f.Name, "size") {
		if v, isVar := f.Args[0].(*ast.Var); isVar {
			if src, has := compSource[v.Name]; has && inScope(src) {
				return &ast.Func{Name: f.Name, Args: []ast.Expr{&ast.Var{Name: src}}}, true
			}
		}
	}
	// Explicit containers only: a nested occurrence this walk misses is
	// a missed optimization, never an unsoundness -- the dead-nulling
	// rule re-checks reads independently via MentionsVar.
	switch n := e.(type) {
	case *ast.Func:
		args := n.Args
		changed := false
		for i, c := range n.Args {
			r, ch := rewriteSizeOf(c, compSource, inScope)
			if ch {
				if !changed {
					args = append([]ast.Expr(nil), n.Args...)
					changed = true
				}
				args[i] = r
			}
		}
		if changed {
			return &ast.Func{Name: n.Name, Distinct: n.Distinct, Star: n.Star, Args: args}, true
		}
	case *ast.Binary:
		l, lch := rewriteSizeOf(n.LHS, compSource, inScope)
		r, rch := rewriteSizeOf(n.RHS, compSource, inScope)
		if lch || rch {
			return &ast.Binary{Op: n.Op, LHS: l, RHS: r}, true
		}
	case *ast.Unary:
		x, ch := rewriteSizeOf(n.Expr, compSource, inScope)
		if ch {
			return &ast.Unary{Op: n.Op, Expr: x}, true
		}
	case *ast.IsNull:
		x, ch := rewriteSizeOf(n.Expr, compSource, inScope)
		if ch {
			return &ast.IsNull{Expr: x, Negated: n.Negated}, true
		}
	case *ast.Index:
		b, bch := rewriteSizeOf(n.Base, compSource, inScope)
		i, ich := rewriteSizeOf(n.Idx, compSource, inScope)
		if bch || ich {
			return &ast.Index{Base: b, Idx: i}, true
		}
	case *ast.ListPred:
		l, lch := rewriteSizeOf(n.List, compSource, inScope)
		pr, pch := rewriteSizeOf(n.Pred, compSource, inScope)
		if lch || pch {
			return &ast.ListPred{Quant: n.Quant, Var: n.Var, List: l, Pred: pr}, true
		}
	case *ast.ListComp:
		l, lch := rewriteSizeOf(n.List, compSource, inScope)
		f, fch := rewriteSizeOf(n.Filter, compSource, inScope)
		m, mch := rewriteSizeOf(n.Map, compSource, inScope)
		if lch || fch || mch {
			return &ast.ListComp{Var: n.Var, List: l, Filter: f, Map: m}, true
		}
	}
	return e, false
}

// nullDeadLets applies rule 2: unread non-final boundary items on
// non-aggregated, non-DISTINCT projections stop computing.
func nullDeadLets(segs []*Segment) {
	for si := 0; si < len(segs)-1; si++ {
		seg := segs[si]
		if seg.Proj.Aggregated || seg.Proj.Distinct {
			continue
		}
		for i := range seg.Proj.Returns {
			it := &seg.Proj.Returns[i]
			if it.Name == "" || isTrivialExpr(it.Expr) {
				continue
			}
			if aliasReadDownstream(segs, si, it.Name) {
				continue
			}
			it.Expr = &ast.Lit{Value: ast.NullLit()}
			deadLetNullings++
		}
	}
}

// aliasReadDownstream reports whether name is read by the defining
// boundary's own ORDER BY/post-where or anything in a later segment
// (unknown stage kinds count as reads).
func aliasReadDownstream(segs []*Segment, si int, name string) bool {
	seg := segs[si]
	for i := range seg.Proj.OrderBy {
		if MentionsVar(seg.Proj.OrderBy[i].Expr, name) {
			return true
		}
	}
	if MentionsVar(seg.PostWhere, name) {
		return true
	}
	for li, later := range segs[si+1:] {
		final := si+1+li == len(segs)-1
		for _, st := range later.Stages {
			if ms, ok := st.(*MatchStage); ok {
				if MentionsVar(ms.Where, name) {
					return true
				}
				continue
			}
			if stageMentionsVar(st, name) {
				return true
			}
		}
		p := &later.Proj
		for i := range p.Returns {
			// A bare self-passthrough at a NON-final boundary propagates
			// the column without consuming it; the scan keeps walking to
			// find a real read. Final-segment items are outputs and
			// always count.
			if !final {
				if v, isVar := p.Returns[i].Expr.(*ast.Var); isVar && v.Name == name && p.Returns[i].Name == name {
					continue
				}
			}
			if MentionsVar(p.Returns[i].Expr, name) {
				return true
			}
		}
		for i := range p.Aggs {
			if MentionsVar(p.Aggs[i].Arg, name) || MentionsVar(p.Aggs[i].Arg2, name) {
				return true
			}
		}
		for i := range p.Post {
			if MentionsVar(p.Post[i].Expr, name) {
				return true
			}
		}
		for i := range p.OrderBy {
			if MentionsVar(p.OrderBy[i].Expr, name) {
				return true
			}
		}
		if MentionsVar(later.PostWhere, name) {
			return true
		}
		if later.CSEResidual != nil && MentionsVar(later.CSEResidual, name) {
			return true
		}
	}
	return false
}

// isTrivialExpr reports expressions not worth nulling (already free).
func isTrivialExpr(e ast.Expr) bool {
	switch e.(type) {
	case *ast.Var, *ast.Lit, *ast.Prop:
		return true
	}
	return false
}
