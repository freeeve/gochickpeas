// Early shortest-path row gating: when a segment's whole job is one
// non-expanding ANY SHORTEST stage and the following boundary chain is a
// pure per-row LET/FILTER pipeline, the path search plus every
// gate-evaluable filter conjunct also runs INSIDE the previous segment,
// inserted before its remaining expansion stages -- so rows the chain
// would kill never pay the expansion (the BFS-first order a hand-written
// kernel uses). The injection is a semijoin-style copy, not a move: the
// downstream stages run unchanged, which keeps the transform sound by
// construction -- the gate only removes rows a later deterministic
// per-row filter would remove anyway (the path search is a pure function
// of its endpoint pair and the graph). Bindings hide past the segment's
// projected names, so nothing else can observe them.
package plan

import "github.com/freeeve/gochickpeas/gql/internal/ast"

// injectSPGates runs the gating pass over a branch's built segments.
func injectSPGates(segments []*Segment) {
	for i := 0; i+1 < len(segments); i++ {
		injectSPGate(segments, i)
	}
}

// injectSPGate considers hoisting segment i+1's shortest-path stage (and
// the pure filter chain behind it) into segment i as a GateStage. Every
// bail-out leaves the plan untouched.
func injectSPGate(segments []*Segment, i int) {
	a, b := segments[i], segments[i+1]
	// The gated segment must be exactly one required single-path stage: an
	// ALL form expands rows and an OPTIONAL one keeps them, so neither can
	// act as a filter.
	if len(b.Stages) != 1 {
		return
	}
	sp, ok := b.Stages[0].(*SpStage)
	if !ok || sp.All || sp.Optional {
		return
	}
	// The host's boundary must be a pure per-row projection: filtering
	// before it commutes only when it neither aggregates, dedups, orders,
	// paginates, nor post-computes.
	if !pureRowProj(&a.Proj) {
		return
	}
	// Resolve the stage's slots to names through its own segment.
	bName := make(map[int]string, len(b.Slots))
	for n, s := range b.Slots {
		bName[s] = n
	}
	fromName, toName, pName := bName[sp.From], bName[sp.To], bName[sp.PathSlot]
	if fromName == "" || toName == "" || pName == "" {
		return
	}
	// A referenced column is gate-evaluable when the host projects it as
	// itself (a bare variable under its own name): then the host's slot
	// holds exactly the value the chain saw.
	avail := map[string]bool{}
	for _, r := range a.Proj.Returns {
		if isVar(r.Expr, r.Name) {
			if _, bound := a.Slots[r.Name]; bound {
				avail[r.Name] = true
			}
		}
	}
	if !avail[fromName] || !avail[toName] {
		return
	}
	if _, clash := a.Slots[pName]; clash {
		return
	}
	avail[pName] = true

	// Walk the boundary chain from the gated segment forward while it
	// stays a pure per-row pipeline, gathering evaluable LET columns and
	// filter conjuncts. Conjuncts are copied, never consumed -- the
	// downstream filter re-running on survivors is idempotent.
	availList := func() []string {
		out := make([]string, 0, len(avail))
		for n := range avail {
			out = append(out, n)
		}
		return out
	}
	type derived struct {
		name string
		expr ast.Expr
	}
	var lets []derived
	var preds []ast.Expr
	need := map[string]bool{fromName: true, toName: true}
	for j := i + 1; j < len(segments); j++ {
		seg := segments[j]
		if j > i+1 && len(seg.Stages) != 0 {
			break
		}
		if !pureRowProj(&seg.Proj) {
			break
		}
		usable := true
		for _, r := range seg.Proj.Returns {
			if isVar(r.Expr, r.Name) {
				continue
			}
			if _, clash := a.Slots[r.Name]; clash || avail[r.Name] {
				usable = false
				continue
			}
			if len(freeVarsOutside(r.Expr, availList())) != 0 {
				continue
			}
			lets = append(lets, derived{name: r.Name, expr: r.Expr})
			avail[r.Name] = true
		}
		if !usable {
			break
		}
		if seg.PostWhere != nil {
			var conjs []ast.Expr
			SplitAnd(seg.PostWhere, &conjs)
			for _, c := range conjs {
				if len(freeVarsOutside(c, availList())) == 0 {
					preds = append(preds, c)
				}
			}
		}
	}
	if len(preds) == 0 {
		return
	}
	for _, d := range lets {
		collectAllVars(d.expr, need)
	}
	for _, p := range preds {
		collectAllVars(p, need)
	}

	// Insertion point: the earliest stage boundary where every host slot
	// the gate reads is bound, that does not split a relationship-
	// uniqueness scope, and that leaves at least one stage after it (a
	// gate just before the projection saves nothing).
	pos := gateInsertPos(a, need, avail)
	if pos < 0 || pos >= len(a.Stages) {
		return
	}

	// Materialize: hidden slots for the path and each hoisted LET, the
	// names registered so the copied expressions resolve against the
	// host's own slot map.
	gate := &GateStage{Sp: *sp, Where: rebuildAnd(preds)}
	w := a.RowWidth
	a.Slots[pName] = w
	gate.Sp.PathSlot = w
	gate.Sp.From = a.Slots[fromName]
	gate.Sp.To = a.Slots[toName]
	w++
	for _, d := range lets {
		a.Slots[d.name] = w
		gate.Derived = append(gate.Derived, GateDerived{Slot: w, Expr: d.expr})
		w++
	}
	a.RowWidth = w
	a.Stages = append(a.Stages[:pos], append([]Stage{gate}, a.Stages[pos:]...)...)
}

// pureRowProj reports whether a projection is per-row passthrough shaped:
// no aggregation, DISTINCT, ordering, pagination, or post-aggregate
// computation. Filtering its input commutes with it.
func pureRowProj(p *ProjPlan) bool {
	return !p.Aggregated && !p.Distinct && len(p.OrderBy) == 0 &&
		p.Skip == nil && p.Limit == nil && len(p.Post) == 0 && p.NHidden == 0
}

// gateInsertPos is the earliest stage boundary of a where every slot named
// by need (that a's slot map knows and the host can evaluate) is bound --
// by the segment's inputs or a preceding stage -- without splitting a
// relationship-uniqueness scope across the boundary; -1 when none.
func gateInsertPos(a *Segment, need, avail map[string]bool) int {
	needSlots := map[int]bool{}
	for n := range need {
		if !avail[n] {
			continue
		}
		if s, ok := a.Slots[n]; ok {
			needSlots[s] = true
		}
	}
	binds := make([][]int, len(a.Stages))
	stageBound := map[int]bool{}
	for si, st := range a.Stages {
		stageSlotBinds(st, func(s int) {
			binds[si] = append(binds[si], s)
			stageBound[s] = true
		})
	}
	// Slots the segment's stages never bind are inputs: bound before
	// stage 0.
	bound := map[int]bool{}
	for _, s := range a.Slots {
		if !stageBound[s] {
			bound[s] = true
		}
	}
	satisfied := func() bool {
		for s := range needSlots {
			if !bound[s] {
				return false
			}
		}
		return true
	}
	for pos := 0; pos <= len(a.Stages); pos++ {
		if satisfied() && !splitsUniqScope(a.Stages, pos) {
			return pos
		}
		if pos < len(a.Stages) {
			for _, s := range binds[pos] {
				bound[s] = true
			}
		}
	}
	return -1
}

// stageSlotBinds reports every slot a stage binds.
func stageSlotBinds(st Stage, set func(int)) {
	switch s := st.(type) {
	case *MatchStage:
		for i := range s.Ops {
			op := &s.Ops[i]
			if op.Kind == OpScan {
				set(op.Slot)
			} else {
				set(op.To)
			}
			if op.Kind == OpExpand || op.Kind == OpVarExpand {
				set(op.RelSlot)
			}
		}
		if s.PathBind != nil {
			set(s.PathBind.PathSlot)
		}
	case *HashJoinStage:
		for _, p := range s.PayloadSlots {
			set(p)
		}
		set(s.KeySlot)
	case *SpStage:
		set(s.PathSlot)
	case *GateStage:
		set(s.Sp.PathSlot)
		for _, d := range s.Derived {
			set(d.Slot)
		}
	case *CallStage:
		set(s.NodeSlot)
		set(s.ValueSlot)
		set(s.DepthSlot)
	case *UnwindStage:
		set(s.OutSlot)
	case *CallSubqueryStage:
		for _, o := range s.OutSlots {
			set(o)
		}
	}
}

// splitsUniqScope reports whether inserting a batch barrier at pos would
// separate two stages sharing a relationship-uniqueness scope: the
// barrier buffers rows, so used-relationship pairs pushed by the earlier
// stage would already be popped when the later stage checks them.
func splitsUniqScope(stages []Stage, pos int) bool {
	before := map[uint32]bool{}
	for _, st := range stages[:pos] {
		stageUniqScopes(st, func(sc uint32) { before[sc] = true })
	}
	split := false
	for _, st := range stages[pos:] {
		stageUniqScopes(st, func(sc uint32) {
			if before[sc] {
				split = true
			}
		})
	}
	return split
}

// stageUniqScopes reports the relationship-uniqueness scopes a stage
// participates in.
func stageUniqScopes(st Stage, add func(uint32)) {
	switch s := st.(type) {
	case *MatchStage:
		add(s.Scope)
	case *HashJoinStage:
		for _, bs := range s.Build {
			add(bs.Scope)
		}
		if s.Probe.Uniq != nil {
			add(s.Probe.Uniq.Scope)
		}
	}
}
