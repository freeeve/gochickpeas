// Monotonic var-length predicate pushdown (port of the Rust plan/mono.rs):
// recognizes all(i IN range(0, size(rels(e))-2) WHERE rels(e)[i].k {<,>}
// rels(e)[i+1].k) -- and the projection-derived and violation-count forms
// -- and lifts them onto the bounded var-expand as a MonoHopSpec so
// non-monotonic hops are pruned during the walk. Mis-recognition is
// impossible, only missed optimization: an unconsumed conjunct stays a
// correct post-hoc filter.
package plan

import "github.com/freeeve/gochickpeas/gql/internal/ast"

// tryPushMonoPred lifts one inline conjunct as a MonoHopSpec onto the
// bounded var-expand for its rel variable; false leaves it in the WHERE.
func tryPushMonoPred(c ast.Expr, ops []BindOp) bool {
	lp, ok := c.(*ast.ListPred)
	if !ok || lp.Quant != ast.QuantAll {
		return false
	}
	sizeArg0 := monoRangeSizeArg(lp.List)
	if sizeArg0 == nil {
		return false
	}
	e := relsArg(sizeArg0)
	if e == "" {
		return false
	}
	b, ok := lp.Pred.(*ast.Binary)
	if !ok {
		return false
	}
	var ascending bool
	switch b.Op {
	case ast.OpLt:
		ascending = true
	case ast.OpGt:
		ascending = false
	default:
		return false
	}
	e1, k1, idx1, ok1 := monoIndexedProp(b.LHS)
	e2, k2, idx2, ok2 := monoIndexedProp(b.RHS)
	if !ok1 || !ok2 || e1 != e || e2 != e || k1 != k2 || !isVar(idx1, lp.Var) || !isVarPlusOne(idx2, lp.Var) {
		return false
	}
	for i := range ops {
		op := &ops[i]
		if op.Kind == OpVarExpand && op.RelVar == e {
			if op.MonoHop != nil || op.Max == nil {
				return false // one spec; bounded var-length only
			}
			op.MonoHop = &MonoHopSpec{RelKey: k1, Ascending: ascending}
			return true
		}
	}
	return false
}

// pushDerivedMonoPred is the projection-derived counterpart: the filter
// lives in the segment PostWhere and names a projected alias defined as
// [r IN rels(p) | r.k] (or over a bare rel-list variable); resolve the
// alias to its source var-expand and push the spec. The post-filter and
// projection stay in place (the pushdown is a pure pruning; the redundant
// filter guards correctness).
func pushDerivedMonoPred(proj *ProjPlan, postWhere ast.Expr, stages []Stage, slots map[string]int) {
	if postWhere == nil {
		return
	}
	var conjs []ast.Expr
	splitAndRef(postWhere, &conjs)
	for _, c := range conjs {
		alias, ascending, ok := derivedMonoShape(c)
		if !ok {
			alias, ascending, ok = violationCountMonoShape(c)
		}
		if !ok {
			continue
		}
		src, key, ok := derivedListSource(proj, alias)
		if !ok {
			continue
		}
		relVar, relSlot, ok := resolveMonoTarget(stages, slots, src)
		if !ok {
			continue
		}
		applyMonoTarget(stages, relVar, relSlot, key, ascending)
	}
}

// derivedMonoShape matches all(i IN range(0,size(L)-2) WHERE L[i] {<,>}
// L[i+1]) over a bare list variable L, returning (L, ascending).
func derivedMonoShape(c ast.Expr) (string, bool, bool) {
	lp, ok := c.(*ast.ListPred)
	if !ok || lp.Quant != ast.QuantAll {
		return "", false, false
	}
	sz := monoRangeSizeArg(lp.List)
	alias := asVarName(sz)
	if alias == "" {
		return "", false, false
	}
	b, ok := lp.Pred.(*ast.Binary)
	if !ok {
		return "", false, false
	}
	var ascending bool
	switch b.Op {
	case ast.OpLt:
		ascending = true
	case ast.OpGt:
		ascending = false
	default:
		return "", false, false
	}
	a1, idx1, ok1 := listIndex(b.LHS)
	a2, idx2, ok2 := listIndex(b.RHS)
	if !ok1 || !ok2 || a1 != alias || a2 != alias || !isVar(idx1, lp.Var) || !isVarPlusOne(idx2, lp.Var) {
		return "", false, false
	}
	return alias, ascending, true
}

// violationCountMonoShape matches size([i IN range(1, size(ts)) WHERE
// ts[i-1] <op> ts[i]]) = 0 -- "no consecutive pair violates the order".
// The inner predicate is the VIOLATION: <= means strictly descending, >=
// strictly ascending; a strict inner op would mean a non-strict order the
// spec can't express, so it is left as a post-hoc filter.
func violationCountMonoShape(c ast.Expr) (string, bool, bool) {
	b, ok := c.(*ast.Binary)
	if !ok || b.Op != ast.OpEq {
		return "", false, false
	}
	var comp ast.Expr
	switch {
	case isZero(b.RHS) && !isZero(b.LHS):
		comp = b.LHS
	case isZero(b.LHS) && !isZero(b.RHS):
		comp = b.RHS
	default:
		return "", false, false
	}
	inner := sizeArg(comp)
	lc, ok := inner.(*ast.ListComp)
	if !ok || lc.Filter == nil || lc.Map != nil {
		return "", false, false
	}
	alias := asVarName(violationRangeSizeArg(lc.List))
	if alias == "" {
		return "", false, false
	}
	f, ok := lc.Filter.(*ast.Binary)
	if !ok {
		return "", false, false
	}
	var ascending bool
	switch f.Op {
	case ast.OpGte:
		ascending = true // violation ts[i-1] >= ts[i] => strictly ascending
	case ast.OpLte:
		ascending = false // violation ts[i-1] <= ts[i] => strictly descending
	default:
		return "", false, false
	}
	a1, idx1, ok1 := listIndex(f.LHS)
	a2, idx2, ok2 := listIndex(f.RHS)
	if !ok1 || !ok2 || a1 != alias || a2 != alias || !isVarMinusOne(idx1, lc.Var) || !isVar(idx2, lc.Var) {
		return "", false, false
	}
	return alias, ascending, true
}

// listIndex matches <listVar>[<idx>].
func listIndex(e ast.Expr) (string, ast.Expr, bool) {
	ix, ok := e.(*ast.Index)
	if !ok {
		return "", nil, false
	}
	name := asVarName(ix.Base)
	if name == "" {
		return "", nil, false
	}
	return name, ix.Idx, true
}

// sizeArg matches size(<x>) returning <x>.
func sizeArg(e ast.Expr) ast.Expr {
	f, ok := e.(*ast.Func)
	if !ok || f.Star || len(f.Args) != 1 || !eqFold(f.Name, "size") {
		return nil
	}
	return f.Args[0]
}

// violationRangeSizeArg matches range(1, size(L)) returning L.
func violationRangeSizeArg(list ast.Expr) ast.Expr {
	f, ok := list.(*ast.Func)
	if !ok || f.Star || len(f.Args) != 2 || !eqFold(f.Name, "range") {
		return nil
	}
	if !isIntLit(f.Args[0], 1) {
		return nil
	}
	return sizeArg(f.Args[1])
}

// monoRangeSizeArg matches range(0, size(<x>) - 2) returning <x> -- the
// shared spine of the monotonic-range matcher.
func monoRangeSizeArg(list ast.Expr) ast.Expr {
	f, ok := list.(*ast.Func)
	if !ok || f.Star || len(f.Args) != 2 || !eqFold(f.Name, "range") {
		return nil
	}
	if !isIntLit(f.Args[0], 0) {
		return nil
	}
	sub, ok := f.Args[1].(*ast.Binary)
	if !ok || sub.Op != ast.OpSub || !isIntLit(sub.RHS, 2) {
		return nil
	}
	return sizeArg(sub.LHS)
}

// monoIndexedProp matches rels(e)[<idx>].k returning (e, k, idx).
func monoIndexedProp(e ast.Expr) (string, string, ast.Expr, bool) {
	po, ok := e.(*ast.PropOf)
	if !ok {
		return "", "", nil, false
	}
	ix, ok := po.Base.(*ast.Index)
	if !ok {
		return "", "", nil, false
	}
	rv := relsArg(ix.Base)
	if rv == "" {
		return "", "", nil, false
	}
	return rv, po.Key, ix.Idx, true
}

// derivedListSource resolves a projected alias to the rels source and
// property key of a simple [r IN rels(src) | r.key] comprehension (no
// filter -- a filter changes element positions) or [r IN e | r.key] over a
// bare rel-list variable.
func derivedListSource(proj *ProjPlan, alias string) (string, string, bool) {
	for i := range proj.Returns {
		if proj.Returns[i].Name != alias {
			continue
		}
		return derivedListSourceExpr(proj.Returns[i].Expr)
	}
	return "", "", false
}

// derivedListSourceExpr matches a [r IN rels(src) | r.key] comprehension
// expression to its rels source and property key.
func derivedListSourceExpr(e ast.Expr) (string, string, bool) {
	lc, ok := e.(*ast.ListComp)
	if !ok || lc.Filter != nil || lc.Map == nil {
		return "", "", false
	}
	src := relsArg(lc.List)
	if src == "" {
		src = asVarName(lc.List)
	}
	if src == "" {
		return "", "", false
	}
	switch m := lc.Map.(type) {
	case *ast.Prop:
		if m.Var == lc.Var {
			return src, m.Key, true
		}
	case *ast.PropOf:
		if isVar(m.Base, lc.Var) {
			return src, m.Key, true
		}
	}
	return "", "", false
}

// pushCrossSegmentMono handles the projection-derived monotonic constraint
// when GQL's LET (the ts = [r IN rels(p) | r.key] projection) and the FILTER
// that reads it land in different segments: the same-segment
// pushDerivedMonoPred cannot see the originating var-expand. For each later
// segment's monotonic-shaped filter conjunct, walk back to the segment that
// defines the alias as a rels-comprehension (requiring an unbroken
// passthrough in between), push the MonoHopSpec onto its bounded var-expand,
// and consume the conjunct.
//
// Consuming (rather than the same-segment form's redundant-guard keep) is
// safe because the walk emits a path only when every hop's key coerces to an
// int and strictly continues the order -- exactly the paths the all()/range
// filter accepts -- so the emitted set is a subset of the filtered set and
// re-checking removes nothing. TestCrossSegmentMonoDropCorrectness pins this
// against the plain filter semantics.
func pushCrossSegmentMono(segments []*Segment) {
	for fi := 1; fi < len(segments); fi++ {
		fseg := segments[fi]
		if fseg.PostWhere == nil {
			continue
		}
		var conjs []ast.Expr
		splitAndRef(fseg.PostWhere, &conjs)
		var kept []ast.Expr
		for _, c := range conjs {
			alias, ascending, ok := derivedMonoShape(c)
			if !ok {
				alias, ascending, ok = violationCountMonoShape(c)
			}
			if ok && tryPushMonoAcross(segments, fi, alias, ascending) {
				continue
			}
			kept = append(kept, c)
		}
		fseg.PostWhere = rebuildAnd(kept)
	}
}

// tryPushMonoAcross searches segments before fi for the one defining alias as
// the rels-comprehension and pushes the mono spec onto its var-expand. The
// alias must pass through unchanged from its definition to the filter, so a
// segment that rebinds it to a different expression stops the search.
func tryPushMonoAcross(segments []*Segment, fi int, alias string, ascending bool) bool {
	for di := fi - 1; di >= 0; di-- {
		expr, found := aliasReturnExpr(&segments[di].Proj, alias)
		if !found {
			return false
		}
		if src, key, ok := derivedListSourceExpr(expr); ok {
			relVar, relSlot, ok := resolveMonoTarget(segments[di].Stages, segments[di].Slots, src)
			if !ok {
				return false
			}
			applyMonoTarget(segments[di].Stages, relVar, relSlot, key, ascending)
			return true
		}
		if !isVar(expr, alias) {
			return false
		}
	}
	return false
}

// aliasReturnExpr returns the projection expression bound to alias.
func aliasReturnExpr(proj *ProjPlan, alias string) (ast.Expr, bool) {
	for i := range proj.Returns {
		if proj.Returns[i].Name == alias {
			return proj.Returns[i].Expr, true
		}
	}
	return nil, false
}

// resolveMonoTarget resolves rels(src) to the exact bounded var-expand to
// push onto: src is a rel variable, or a named path whose single
// var-length hop is that op. Ambiguity bails (the constraint stays a
// correct post-hoc filter). Returns (relVar, relSlot, ok) -- one of the
// two identifies the target.
func resolveMonoTarget(stages []Stage, slots map[string]int, src string) (string, int, bool) {
	for _, s := range stages {
		ms, ok := s.(*MatchStage)
		if !ok {
			continue
		}
		for i := range ms.Ops {
			op := &ms.Ops[i]
			if op.Kind == OpVarExpand && op.RelVar == src && op.Max != nil {
				return src, NoSlot, true
			}
		}
	}
	slotP, ok := slots[src]
	if !ok {
		return "", NoSlot, false
	}
	for _, s := range stages {
		ms, ok := s.(*MatchStage)
		if !ok || ms.PathBind == nil || ms.PathBind.PathSlot != slotP {
			continue
		}
		// rels(p) spans the whole path; only a single var-length hop
		// equals one op's rels.
		var hops []*BindOp
		for i := range ms.Ops {
			if ms.Ops[i].Kind == OpExpand || ms.Ops[i].Kind == OpVarExpand {
				hops = append(hops, &ms.Ops[i])
			}
		}
		if len(hops) == 1 && hops[0].Kind == OpVarExpand && hops[0].Max != nil && hops[0].RelSlot == ms.PathBind.RelsSlot {
			return "", ms.PathBind.RelsSlot, true
		}
		return "", NoSlot, false
	}
	return "", NoSlot, false
}

// applyMonoTarget sets MonoHop on the identified bounded var-expand (when
// it has no spec yet).
func applyMonoTarget(stages []Stage, relVar string, relSlot int, key string, ascending bool) {
	for _, s := range stages {
		ms, ok := s.(*MatchStage)
		if !ok {
			continue
		}
		for i := range ms.Ops {
			op := &ms.Ops[i]
			if op.Kind != OpVarExpand || op.Max == nil || op.MonoHop != nil {
				continue
			}
			hit := (relVar != "" && op.RelVar == relVar) || (relVar == "" && relSlot != NoSlot && op.RelSlot == relSlot)
			if hit {
				op.MonoHop = &MonoHopSpec{RelKey: key, Ascending: ascending}
				return
			}
		}
	}
}

func isVar(e ast.Expr, name string) bool {
	v, ok := e.(*ast.Var)
	return ok && v.Name == name
}

func asVarName(e ast.Expr) string {
	if v, ok := e.(*ast.Var); ok {
		return v.Name
	}
	return ""
}

func isIntLit(e ast.Expr, v int64) bool {
	l, ok := e.(*ast.Lit)
	return ok && l.Value.Kind == ast.LitInt && l.Value.I == v
}

func isZero(e ast.Expr) bool { return isIntLit(e, 0) }

// isVarPlusOne matches <name> + 1 (either operand order).
func isVarPlusOne(e ast.Expr, name string) bool {
	b, ok := e.(*ast.Binary)
	if !ok || b.Op != ast.OpAdd {
		return false
	}
	return (isVar(b.LHS, name) && isIntLit(b.RHS, 1)) || (isVar(b.RHS, name) && isIntLit(b.LHS, 1))
}

// isVarMinusOne matches <name> - 1.
func isVarMinusOne(e ast.Expr, name string) bool {
	b, ok := e.(*ast.Binary)
	if !ok || b.Op != ast.OpSub {
		return false
	}
	return isVar(b.LHS, name) && isIntLit(b.RHS, 1)
}
