// Monotonic var-length predicate pushdown (port of the Rust plan/mono.rs):
// one normalizer answers "is this conjunct equivalent to sorted(L) /
// reverse-sorted(L)?" over a canonicalized consecutive-pair core, and one
// pass over the fully built segments lifts each recognized conjunct onto
// its bounded var-expand as a MonoHopSpec so non-monotonic hops are pruned
// during the walk. Mis-recognition is impossible, only missed
// optimization: an unconsumed conjunct stays a correct post-hoc filter.
package plan

import "github.com/freeeve/gochickpeas/gql/internal/ast"

// monoListRef identifies the list a sortedness conjunct constrains: a
// projected list alias (derived form), or rels(relVar) with the compared
// property key (inline form).
type monoListRef struct {
	alias  string
	relVar string
	key    string
}

// monoShape is the normalizer's result: which list must be sorted, in
// which direction, and the source shape's null semantics (NullsPass on
// MonoHopSpec: an incomparable pair prunes the all() family and passes
// the violation-count family).
type monoShape struct {
	ref       monoListRef
	ascending bool
	nullsPass bool
}

// pushMonoPreds is the single monotonic-pushdown pass, run once per UNION
// branch (and per CALL subquery body) over its fully built segments. An
// inline conjunct in a MATCH stage's WHERE pushes onto that same stage's
// var-expand; an alias conjunct in a segment's boundary WHERE resolves its
// alias through the defining projection -- the segment's own or, via an
// unbroken passthrough, an earlier segment's -- and pushes onto the
// originating bounded var-expand.
//
// Every push CONSUMES the conjunct, which is safe because the walk prunes
// with the exact filter semantics: hop pairs compare via the same
// three-valued value.Compare the filter's </> uses, an incomparable pair
// (missing key, NaN, mixed kinds) prunes for the all() shape and passes
// for the violation-count shape (NullsPass), and a path's first hop
// carries no comparison -- so the emitted set equals the filtered set and
// re-checking removes nothing. Min-0 quantifiers never get a spec (the
// reachability BFS has no walk order), keeping their conjunct as the post
// filter. TestCrossSegmentMonoDropCorrectness pins the equivalence against
// the plain filter semantics, including sparse and float keys.
func pushMonoPreds(segments []*Segment) {
	for fi, seg := range segments {
		for _, st := range seg.Stages {
			ms, ok := st.(*MatchStage)
			if !ok || ms.Where == nil {
				continue
			}
			ms.Where = consumeMonoConjuncts(ms.Where, func(sh monoShape) bool {
				// Inline form, same stage only: a stage WHERE is scoped
				// inside its own (possibly OPTIONAL) match, so pruning this
				// stage's walk is exactly the WHERE's effect -- but pushing
				// into another stage's op would filter rows an OPTIONAL
				// would have null-extended.
				if sh.ref.relVar == "" {
					return false
				}
				return applyMonoTarget([]Stage{ms}, sh.ref.relVar, NoSlot, sh.ref.key, sh.ascending, sh.nullsPass)
			})
		}
		if seg.PostWhere != nil {
			seg.PostWhere = consumeMonoConjuncts(seg.PostWhere, func(sh monoShape) bool {
				if sh.ref.alias == "" {
					return false
				}
				return pushMonoAlias(segments, fi, sh)
			})
		}
	}
}

// consumeMonoConjuncts hands each recognized sortedness conjunct of a
// WHERE's top-level AND chain to push, dropping the consumed ones and
// rebuilding the rest.
func consumeMonoConjuncts(where ast.Expr, push func(monoShape) bool) ast.Expr {
	var conjs []ast.Expr
	SplitAnd(where, &conjs)
	var kept []ast.Expr
	for _, c := range conjs {
		if sh, ok := monoConjunctShape(c); ok && push(sh) {
			continue
		}
		kept = append(kept, c)
	}
	return rebuildAnd(kept)
}

// pushMonoAlias resolves an alias-form conjunct seen at segment fi's
// boundary WHERE: walk from fi's own projection back through earlier
// segments to the one defining the alias as a rels-comprehension, then
// push onto its bounded var-expand. The alias must pass through unchanged
// from its definition to the filter -- fi's own projection included -- so
// a projection that rebinds it to any other expression stops the search
// (the filter constrains the rebound value, not the original walk).
func pushMonoAlias(segments []*Segment, fi int, sh monoShape) bool {
	for di := fi; di >= 0; di-- {
		expr, found := aliasReturnExpr(&segments[di].Proj, sh.ref.alias)
		if !found {
			return false
		}
		if src, key, ok := derivedListSourceExpr(expr); ok {
			relVar, relSlot, ok := resolveMonoTarget(segments[di].Stages, segments[di].Slots, src)
			if !ok {
				return false
			}
			return applyMonoTarget(segments[di].Stages, relVar, relSlot, key, sh.ascending, sh.nullsPass)
		}
		if !isVar(expr, sh.ref.alias) {
			return false
		}
	}
	return false
}

// monoConjunctShape is the normalizer: both surface families -- the all()
// quantifier over consecutive pairs and the violation-count form
// size([.. WHERE a pair violates the order]) = 0 -- reduce to one
// consecutive-pair core, so an unseen equivalent phrasing (shifted index
// offsets, either operand order, a widened violation range, the inline
// rels(e)[i].k elements or a bare list alias) lands on the same spec.
func monoConjunctShape(c ast.Expr) (monoShape, bool) {
	if lp, ok := c.(*ast.ListPred); ok && lp.Quant == ast.QuantAll {
		return monoPairCore(lp.Var, lp.List, lp.Pred, false)
	}
	b, ok := c.(*ast.Binary)
	if !ok || b.Op != ast.OpEq {
		return monoShape{}, false
	}
	var comp ast.Expr
	switch {
	case isZero(b.RHS) && !isZero(b.LHS):
		comp = b.LHS
	case isZero(b.LHS) && !isZero(b.RHS):
		comp = b.RHS
	default:
		return monoShape{}, false
	}
	lc, ok := sizeArg(comp).(*ast.ListComp)
	if !ok || lc.Filter == nil || lc.Map != nil {
		return monoShape{}, false
	}
	return monoPairCore(lc.Var, lc.List, lc.Filter, true)
}

// monoPairCore recognizes the shared consecutive-pair core: ivar iterates
// range(lo, size(REF)+m) (inclusive bounds) and pred compares REF elements
// at affine offsets ivar+c and ivar+c+1. The covered pair set must start
// exactly at pair 0 (a negative index would wrap to the list's end). For
// the strict all() family (op < or >) it must also end exactly at pair
// size-2 -- an out-of-range read is null, which fails all() -- and the
// order is the operator's. For the violation family the inner comparison
// is the VIOLATION (non-strict: >= violates ascending, <= descending; a
// strict violation would mean a non-strict order the spec can't express)
// and a longer range is fine (an out-of-range read is null, never a
// violation), but a shorter one misses pairs.
func monoPairCore(ivar string, list, pred ast.Expr, violation bool) (monoShape, bool) {
	lo, sizeOf, m, ok := monoRange(list)
	if !ok {
		return monoShape{}, false
	}
	b, ok := pred.(*ast.Binary)
	if !ok {
		return monoShape{}, false
	}
	ref1, off1, ok1 := monoElem(b.LHS, ivar)
	ref2, off2, ok2 := monoElem(b.RHS, ivar)
	if !ok1 || !ok2 || ref1 != ref2 || !sameMonoList(sizeOf, ref1) {
		return monoShape{}, false
	}
	d := off2 - off1
	if d != 1 && d != -1 {
		return monoShape{}, false
	}
	lhsEarlier := off1 < off2
	var ascending bool
	if violation {
		switch b.Op {
		case ast.OpGte:
			ascending = lhsEarlier // violation: earlier >= later
		case ast.OpLte:
			ascending = !lhsEarlier
		default:
			return monoShape{}, false
		}
	} else {
		switch b.Op {
		case ast.OpLt:
			ascending = lhsEarlier // order: earlier < later
		case ast.OpGt:
			ascending = !lhsEarlier
		default:
			return monoShape{}, false
		}
	}
	minOff := min(off1, off2)
	if lo+minOff != 0 {
		return monoShape{}, false
	}
	if last := m + minOff; (violation && last < -2) || (!violation && last != -2) {
		return monoShape{}, false
	}
	return monoShape{ref: ref1, ascending: ascending, nullsPass: violation}, true
}

// monoElem matches one comparison side as an element read at an affine
// offset of the iteration variable: L[ivar+c] (bare list alias) or
// rels(e)[ivar+c].k (inline form).
func monoElem(e ast.Expr, ivar string) (monoListRef, int64, bool) {
	key := ""
	if po, ok := e.(*ast.PropOf); ok {
		e, key = po.Base, po.Key
	}
	ix, ok := e.(*ast.Index)
	if !ok {
		return monoListRef{}, 0, false
	}
	off, ok := affineOffset(ix.Idx, ivar)
	if !ok {
		return monoListRef{}, 0, false
	}
	if key != "" {
		if rv := relsArg(ix.Base); rv != "" {
			return monoListRef{relVar: rv, key: key}, off, true
		}
		return monoListRef{}, 0, false
	}
	if name := asVarName(ix.Base); name != "" {
		return monoListRef{alias: name}, off, true
	}
	return monoListRef{}, 0, false
}

// sameMonoList reports whether the range's size argument names the same
// list the compared elements read (the alias, or rels(relVar)).
func sameMonoList(sizeOf ast.Expr, ref monoListRef) bool {
	if ref.alias != "" {
		return isVar(sizeOf, ref.alias)
	}
	return relsArg(sizeOf) == ref.relVar
}

// monoRange matches the two-argument range(<int lo>, size(<X>)+<m>) (a
// step argument could skip pairs), returning lo, the size argument X, m.
func monoRange(list ast.Expr) (int64, ast.Expr, int64, bool) {
	f, ok := list.(*ast.Func)
	if !ok || f.Star || len(f.Args) != 2 || !eqFold(f.Name, "range") {
		return 0, nil, 0, false
	}
	lo, ok := intLitVal(f.Args[0])
	if !ok {
		return 0, nil, 0, false
	}
	sizeOf, m, ok := sizePlus(f.Args[1])
	if !ok {
		return 0, nil, 0, false
	}
	return lo, sizeOf, m, true
}

// sizePlus matches size(<X>), size(<X>)+c, c+size(<X>), or size(<X>)-c,
// returning X and the signed offset.
func sizePlus(e ast.Expr) (ast.Expr, int64, bool) {
	if x := sizeArg(e); x != nil {
		return x, 0, true
	}
	b, ok := e.(*ast.Binary)
	if !ok {
		return nil, 0, false
	}
	switch b.Op {
	case ast.OpAdd:
		if x := sizeArg(b.LHS); x != nil {
			if c, ok := intLitVal(b.RHS); ok {
				return x, c, true
			}
		}
		if x := sizeArg(b.RHS); x != nil {
			if c, ok := intLitVal(b.LHS); ok {
				return x, c, true
			}
		}
	case ast.OpSub:
		if x := sizeArg(b.LHS); x != nil {
			if c, ok := intLitVal(b.RHS); ok {
				return x, -c, true
			}
		}
	}
	return nil, 0, false
}

// affineOffset matches <ivar>, <ivar>+c, c+<ivar>, or <ivar>-c, returning
// the signed offset relative to the iteration variable.
func affineOffset(e ast.Expr, ivar string) (int64, bool) {
	if isVar(e, ivar) {
		return 0, true
	}
	b, ok := e.(*ast.Binary)
	if !ok {
		return 0, false
	}
	switch b.Op {
	case ast.OpAdd:
		if isVar(b.LHS, ivar) {
			if c, ok := intLitVal(b.RHS); ok {
				return c, true
			}
		}
		if isVar(b.RHS, ivar) {
			if c, ok := intLitVal(b.LHS); ok {
				return c, true
			}
		}
	case ast.OpSub:
		if isVar(b.LHS, ivar) {
			if c, ok := intLitVal(b.RHS); ok {
				return -c, true
			}
		}
	}
	return 0, false
}

// sizeArg matches size(<x>) returning <x>.
func sizeArg(e ast.Expr) ast.Expr {
	f, ok := e.(*ast.Func)
	if !ok || f.Star || len(f.Args) != 1 || !eqFold(f.Name, "size") {
		return nil
	}
	return f.Args[0]
}

// derivedListSourceExpr matches a [r IN rels(src) | r.key] comprehension
// (no filter -- a filter changes element positions; src may also be a bare
// rel-list variable) to its rels source and property key.
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
// var-length hop is that op. Ambiguity bails, and so does an OPTIONAL
// stage: the boundary WHERE this path serves sits OUTSIDE the optional
// match, so pruning its walk would null-extend rows the filter drops (the
// constraint stays a correct post-hoc filter). Returns (relVar, relSlot,
// ok) -- one of the two identifies the target.
func resolveMonoTarget(stages []Stage, slots map[string]int, src string) (string, int, bool) {
	for _, s := range stages {
		ms, ok := s.(*MatchStage)
		if !ok {
			continue
		}
		for i := range ms.Ops {
			op := &ms.Ops[i]
			if op.Kind == OpVarExpand && op.RelVar == src && op.Max != nil {
				if ms.Optional {
					return "", NoSlot, false
				}
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
		if ms.Optional {
			return "", NoSlot, false
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
// it has no spec yet), reporting whether it attached. A min-0 quantifier
// never gets a spec: exec resolves it via the reachability BFS, which has
// no walk order to prune, so the caller must keep the conjunct.
func applyMonoTarget(stages []Stage, relVar string, relSlot int, key string, ascending, nullsPass bool) bool {
	for _, s := range stages {
		ms, ok := s.(*MatchStage)
		if !ok {
			continue
		}
		for i := range ms.Ops {
			op := &ms.Ops[i]
			if op.Kind != OpVarExpand {
				continue
			}
			hit := (relVar != "" && op.RelVar == relVar) || (relVar == "" && relSlot != NoSlot && op.RelSlot == relSlot)
			if !hit {
				continue
			}
			if op.Max == nil || op.Min == 0 || op.MonoHop != nil {
				return false
			}
			op.MonoHop = &MonoHopSpec{RelKey: key, Ascending: ascending, NullsPass: nullsPass}
			return true
		}
	}
	return false
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

// intLitVal returns an integer literal's value.
func intLitVal(e ast.Expr) (int64, bool) {
	l, ok := e.(*ast.Lit)
	if !ok || l.Value.Kind != ast.LitInt {
		return 0, false
	}
	return l.Value.I, true
}

func isIntLit(e ast.Expr, v int64) bool {
	c, ok := intLitVal(e)
	return ok && c == v
}

func isZero(e ast.Expr) bool { return isIntLit(e, 0) }
