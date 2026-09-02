// Named-path elision: a path bound over ONE quantified hop (the only
// form the planner accepts) whose every use is rels(p) or length(p) is
// redundant -- the hop's relationship-list value already sits in the row
// at the path's RelsSlot, in path order. The uses rewrite to a synthetic
// variable aliasing that slot and the assembly drops entirely: no path
// value, no node reconstruction, no per-row endpoint walk -- and the
// stage WHERE, no longer a post-path filter, gets ordinary pushdown.
// Any other use of the path (nodes(p), a bare projection, a comparison)
// declines the whole elision.
package plan

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// DisablePathElide pins differential tests to the assembled-path
// execution; pathElides counts rewrites so tests cannot pass vacuously.
var (
	DisablePathElide bool
	pathElides       int
)

// elidePathBinds rewrites each qualifying segment in place (fresh
// expression trees on every rewritten spine; unchanged subtrees are
// shared, never mutated).
func elidePathBinds(segs []*Segment) {
	if DisablePathElide {
		return
	}
	for i := range segs {
		elideSegmentPathBinds(segs, i)
	}
}

func elideSegmentPathBinds(segs []*Segment, segIdx int) {
	seg := segs[segIdx]
	for si, st := range seg.Stages {
		ms, ok := st.(*MatchStage)
		if !ok || ms.PathBind == nil {
			continue
		}
		pb := ms.PathBind
		// The rels slot must hold a LIST (a quantified hop); a fixed
		// hop binds a bare relationship value there.
		listValued := false
		for i := range ms.Ops {
			if ms.Ops[i].RelSlot == pb.RelsSlot && ms.Ops[i].Kind == OpVarExpand {
				listValued = true
			}
		}
		if !listValued {
			continue
		}
		pName := ""
		for nm, s := range seg.Slots {
			if s == pb.PathSlot {
				pName = nm
			}
		}
		if pName == "" {
			continue
		}
		synth := "__prels_" + pName
		if _, taken := seg.Slots[synth]; taken {
			continue
		}

		// Two-phase: trial-rewrite every site; apply only when all
		// succeed. Sites cover everywhere the path variable is visible:
		// this and later stages of the segment, the boundary projection
		// (items, aggregate arguments, post wrappers, order keys), and
		// the post-where. Later segments never see it -- a projection
		// item mentioning p in any unrewritable way declines here.
		var sites []elideSite
		ok = true
		add := func(e ast.Expr, apply func(ast.Expr)) {
			if e == nil || !ok {
				return
			}
			if r, rok := rewritePathUses(e, pName, synth); rok {
				if r != e {
					sites = append(sites, elideSite{r, apply})
				}
			} else {
				ok = false
			}
		}
		for sj := si; sj < len(seg.Stages) && ok; sj++ {
			switch s2 := seg.Stages[sj].(type) {
			case *MatchStage:
				s2c := s2
				add(s2.Where, func(e ast.Expr) { s2c.Where = e })
			default:
				// A non-MATCH stage kind's expressions are not modeled
				// here; decline if any could read the path.
				ok = !stageMentionsVar(seg.Stages[sj], pName)
			}
		}
		// The boundary and any carry chain: a LET/FILTER boundary star-
		// projects p forward as a bare Var item, which downstream
		// segments then read -- follow the chain, rewriting each
		// carrying item to the synthetic list column and the downstream
		// uses under the same rules. The FINAL segment's items are the
		// query's outputs, so a carry reaching it declines naturally
		// (a bare output use of p is unrewritable).
		if !elideBoundaryChain(segs, segIdx, pName, synth, add, &sites, &ok) {
			ok = false
		}
		if !ok {
			continue
		}
		for _, s := range sites {
			s.apply(s.expr)
		}
		seg.Slots[synth] = pb.RelsSlot
		ms.PathBind = nil
		pathElides++
	}
}

// elideBoundaryChain scans segment k's boundary plus every downstream
// segment reached by a bare carry of the path variable, appending trial
// rewrites via add. Reports false when any use cannot rewrite.
// elideSite is one pending rewrite: the fresh expression and where to
// install it if the whole elision succeeds.
type elideSite struct {
	expr  ast.Expr
	apply func(ast.Expr)
}

func elideBoundaryChain(segs []*Segment, k int, pName, synth string,
	add func(ast.Expr, func(ast.Expr)), sites *[]elideSite, okp *bool) bool {
	seg := segs[k]
	proj := &seg.Proj
	carried := false
	for i := range proj.Returns {
		it := &proj.Returns[i]
		if v, isVar := it.Expr.(*ast.Var); isVar && v.Name == pName && it.Name == pName && k+1 < len(segs) {
			// A bare carry: rewrite the item to carry the rel list under
			// the synthetic name instead, and rename the downstream
			// column. Applied only if the whole chain succeeds.
			itc := it
			ci := i
			*sites = append(*sites, elideSite{&ast.Var{Name: synth}, func(e ast.Expr) {
				itc.Expr = e
				itc.Name = synth
				if ci < len(proj.Columns) {
					proj.Columns[ci] = synth
				}
				next := segs[k+1]
				if s, okk := next.Slots[pName]; okk {
					delete(next.Slots, pName)
					next.Slots[synth] = s
				}
			}})
			carried = true
			continue
		}
		ic := i
		add(it.Expr, func(e ast.Expr) { proj.Returns[ic].Expr = e })
	}
	for i := range proj.Aggs {
		ic := i
		add(proj.Aggs[i].Arg, func(e ast.Expr) { proj.Aggs[ic].Arg = e })
		add(proj.Aggs[i].Arg2, func(e ast.Expr) { proj.Aggs[ic].Arg2 = e })
	}
	for i := range proj.Post {
		ic := i
		add(proj.Post[i].Expr, func(e ast.Expr) { proj.Post[ic].Expr = e })
	}
	for i := range proj.OrderBy {
		ic := i
		add(proj.OrderBy[i].Expr, func(e ast.Expr) { proj.OrderBy[ic].Expr = e })
	}
	add(seg.PostWhere, func(e ast.Expr) { seg.PostWhere = e })
	if !*okp {
		return false
	}
	if !carried {
		return true
	}
	// Downstream segment: its stages must not read p except through
	// rewritable expressions, then its own boundary continues the chain.
	next := segs[k+1]
	for _, st := range next.Stages {
		if ms2, isMatch := st.(*MatchStage); isMatch {
			ms2c := ms2
			add(ms2.Where, func(e ast.Expr) { ms2c.Where = e })
		} else if stageMentionsVar(st, pName) {
			return false
		}
	}
	return elideBoundaryChain(segs, k+1, pName, synth, add, sites, okp)
}

// stageMentionsVar over-approximates whether a non-MATCH stage reads the
// variable; unknown stage kinds decline conservatively.
func stageMentionsVar(st Stage, name string) bool {
	switch s := st.(type) {
	case *UnwindStage:
		return MentionsVar(s.List, name)
	case *SpStage:
		// An SpStage binds its own path slot and carries no general
		// expressions reading outer variables by name.
		_ = s
		return false
	case *GateStage:
		return MentionsVar(s.Where, name)
	}
	return true
}

// rewritePathUses returns e with rels(p) -> synth and length(p) ->
// size(synth) applied, ok=false when the path is read any other way.
// Rewritten spines are fresh nodes; untouched subtrees (and any
// expression kind not modeled here that provably does not mention p)
// pass through shared.
func rewritePathUses(e ast.Expr, pv, synth string) (ast.Expr, bool) {
	if e == nil {
		return nil, true
	}
	switch n := e.(type) {
	case *ast.Var:
		if n.Name == pv {
			return nil, false
		}
		return e, true
	case *ast.Prop:
		if n.Var == pv {
			return nil, false
		}
		return e, true
	case *ast.HasLabelExpr:
		if n.Var == pv {
			return nil, false
		}
		return e, true
	case *ast.Lit:
		return e, true
	case *ast.Func:
		if !n.Star && len(n.Args) == 1 {
			if v, isVar := n.Args[0].(*ast.Var); isVar && v.Name == pv {
				if eqFold(n.Name, "rels") || eqFold(n.Name, "relationships") {
					return &ast.Var{Name: synth}, true
				}
				if eqFold(n.Name, "length") {
					return &ast.Func{Name: "size", Args: []ast.Expr{&ast.Var{Name: synth}}}, true
				}
				return nil, false
			}
		}
		args, changed, ok := rewriteAll(n.Args, pv, synth)
		if !ok {
			return nil, false
		}
		if !changed {
			return e, true
		}
		return &ast.Func{Name: n.Name, Distinct: n.Distinct, Star: n.Star, Args: args}, true
	case *ast.Binary:
		l, lok := rewritePathUses(n.LHS, pv, synth)
		r, rok := rewritePathUses(n.RHS, pv, synth)
		if !lok || !rok {
			return nil, false
		}
		if l == n.LHS && r == n.RHS {
			return e, true
		}
		return &ast.Binary{Op: n.Op, LHS: l, RHS: r}, true
	case *ast.Unary:
		x, ok := rewritePathUses(n.Expr, pv, synth)
		if !ok {
			return nil, false
		}
		if x == n.Expr {
			return e, true
		}
		return &ast.Unary{Op: n.Op, Expr: x}, true
	case *ast.IsNull:
		x, ok := rewritePathUses(n.Expr, pv, synth)
		if !ok {
			return nil, false
		}
		if x == n.Expr {
			return e, true
		}
		return &ast.IsNull{Expr: x, Negated: n.Negated}, true
	case *ast.In:
		x, xok := rewritePathUses(n.Expr, pv, synth)
		l, lok := rewritePathUses(n.List, pv, synth)
		if !xok || !lok {
			return nil, false
		}
		if x == n.Expr && l == n.List {
			return e, true
		}
		return &ast.In{Expr: x, List: l}, true
	case *ast.Index:
		b, bok := rewritePathUses(n.Base, pv, synth)
		i, iok := rewritePathUses(n.Idx, pv, synth)
		if !bok || !iok {
			return nil, false
		}
		if b == n.Base && i == n.Idx {
			return e, true
		}
		return &ast.Index{Base: b, Idx: i}, true
	case *ast.ListExpr:
		els, changed, ok := rewriteAll(n.Elems, pv, synth)
		if !ok {
			return nil, false
		}
		if !changed {
			return e, true
		}
		return &ast.ListExpr{Elems: els}, true
	case *ast.ListPred:
		if n.Var == pv {
			return e, true // shadowed inside
		}
		l, lok := rewritePathUses(n.List, pv, synth)
		p, pok := rewritePathUses(n.Pred, pv, synth)
		if !lok || !pok {
			return nil, false
		}
		if l == n.List && p == n.Pred {
			return e, true
		}
		return &ast.ListPred{Quant: n.Quant, Var: n.Var, List: l, Pred: p}, true
	case *ast.ListComp:
		if n.Var == pv {
			return e, true
		}
		l, lok := rewritePathUses(n.List, pv, synth)
		f, fok := rewritePathUses(n.Filter, pv, synth)
		m, mok := rewritePathUses(n.Map, pv, synth)
		if !lok || !fok || !mok {
			return nil, false
		}
		if l == n.List && f == n.Filter && m == n.Map {
			return e, true
		}
		return &ast.ListComp{Var: n.Var, List: l, Filter: f, Map: m}, true
	case *ast.Case:
		op, ook := rewritePathUses(n.Operand, pv, synth)
		el, eok := rewritePathUses(n.Else, pv, synth)
		if !ook || !eok {
			return nil, false
		}
		changed := op != n.Operand || el != n.Else
		whens := n.Whens
		for i := range n.Whens {
			c, cok := rewritePathUses(n.Whens[i].Cond, pv, synth)
			r, rok := rewritePathUses(n.Whens[i].Result, pv, synth)
			if !cok || !rok {
				return nil, false
			}
			if c != n.Whens[i].Cond || r != n.Whens[i].Result {
				if !changed || len(whens) == len(n.Whens) {
					whens = append([]ast.CaseWhen(nil), n.Whens...)
				}
				whens[i].Cond, whens[i].Result = c, r
				changed = true
			}
		}
		if !changed {
			return e, true
		}
		return &ast.Case{Operand: op, Whens: whens, Else: el}, true
	default:
		// Anything not modeled (subqueries, reductions, map forms,
		// slices, ...) passes through untouched only when it provably
		// does not read the path.
		if MentionsVar(e, pv) {
			return nil, false
		}
		return e, true
	}
}

func rewriteAll(es []ast.Expr, pv, synth string) ([]ast.Expr, bool, bool) {
	out := es
	changed := false
	for i, c := range es {
		r, ok := rewritePathUses(c, pv, synth)
		if !ok {
			return nil, false, false
		}
		if r != c {
			if !changed {
				out = append([]ast.Expr(nil), es...)
				changed = true
			}
			out[i] = r
		}
	}
	return out, changed, true
}
