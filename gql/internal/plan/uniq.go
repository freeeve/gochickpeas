// MATCH-scope relationship-uniqueness marking (port of the Rust
// recognize.rs::mark_rel_uniqueness): within one MATCH clause (= one
// scope, spanning its comma patterns and any planner splits) every
// relationship must be pairwise distinct -- ISO GQL's DIFFERENT EDGES
// default match mode / openCypher's relationship isomorphism.
package plan

// markRelUniqueness marks each rel-binding op's uniqueness participation.
// Tracking is gated on relationship-TYPE intersection: two ops whose type
// sets cannot name the same relationship (disjoint, non-empty) never
// conflict, so a typical multi-hop clause over distinct types stays
// completely untracked and zero-cost. An op with an intersecting op
// EARLIER in execution order gets Check (its candidate must not reuse a
// used pair); one with an intersecting op LATER gets Contribute (it
// pushes its pair(s)). A contributing var-length loses DedupEndpoints
// (trails with different rel sets must stay distinct rows for the
// downstream exclusion to be exact).
func markRelUniqueness(stages []Stage) {
	type info struct {
		scope  uint32
		types  []string
		si, oi int
	}
	var infos []info
	for si, stage := range stages {
		ms, ok := stage.(*MatchStage)
		if !ok || ms.Walk {
			// REPEATABLE ELEMENTS: walk semantics, no uniqueness pairs.
			continue
		}
		for oi := range ms.Ops {
			op := &ms.Ops[oi]
			if op.Kind == OpExpand || op.Kind == OpVarExpand {
				infos = append(infos, info{scope: ms.Scope, types: op.Types, si: si, oi: oi})
			}
		}
	}
	for i := range infos {
		cur := &infos[i]
		check, contribute := false, false
		for j := range infos[:i] {
			if infos[j].scope == cur.scope && intersects(cur.types, infos[j].types) {
				check = true
				break
			}
		}
		for j := i + 1; j < len(infos); j++ {
			if infos[j].scope == cur.scope && intersects(cur.types, infos[j].types) {
				contribute = true
				break
			}
		}
		if !check && !contribute {
			continue
		}
		ms := stages[cur.si].(*MatchStage)
		op := &ms.Ops[cur.oi]
		op.Uniq = &RelUniq{Scope: cur.scope, Check: check, Contribute: contribute}
		if op.Kind == OpVarExpand && contribute {
			op.DedupEndpoints = false
		}
	}
}

// intersects reports whether two type lists can name the same relationship.
// An empty type list matches any relationship, so it intersects everything.
func intersects(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	for _, t := range a {
		for _, u := range b {
			if t == u {
				return true
			}
		}
	}
	return false
}

// remarkRelUniqueness recomputes every tracked op's Check/Contribute over
// the FINAL stage list's EFFECTIVE execution order: hash-join extraction
// moves the consumed connecting expand (the probe) and the collected build
// ops relative to the ops left in place, so flags assigned on the nested
// order can invert -- an op that was the last of its scope (Check-only)
// can come to execute FIRST as the join's probe, pushing nothing while the
// op now after it contributes to nobody, and a reused relationship sails
// through. Extraction never changes WHICH ops intersect (only their
// order), so the tracked set is exactly the ops already carrying Uniq;
// their recorded scope survives the move. Effective order is stage order
// with a hash join expanding to its build stages' ops then its probe
// (builds materialize before any probe emission).
func remarkRelUniqueness(stages []Stage) {
	type info struct {
		scope uint32
		types []string
		op    *BindOp
	}
	var infos []info
	add := func(ms *MatchStage) {
		if ms.Walk {
			return
		}
		for oi := range ms.Ops {
			if op := &ms.Ops[oi]; op.Uniq != nil {
				infos = append(infos, info{scope: op.Uniq.Scope, types: op.Types, op: op})
			}
		}
	}
	for _, stage := range stages {
		switch s := stage.(type) {
		case *MatchStage:
			add(s)
		case *HashJoinStage:
			for _, b := range s.Build {
				add(b)
			}
			if s.Probe.Uniq != nil {
				infos = append(infos, info{scope: s.Probe.Uniq.Scope, types: s.Probe.Types, op: &s.Probe})
			}
		}
	}
	for i := range infos {
		cur := &infos[i]
		check, contribute := false, false
		for j := range infos[:i] {
			if infos[j].scope == cur.scope && intersects(cur.types, infos[j].types) {
				check = true
				break
			}
		}
		for j := i + 1; j < len(infos); j++ {
			if infos[j].scope == cur.scope && intersects(cur.types, infos[j].types) {
				contribute = true
				break
			}
		}
		cur.op.Uniq = &RelUniq{Scope: cur.scope, Check: check, Contribute: contribute}
		if cur.op.Kind == OpVarExpand && contribute {
			cur.op.DedupEndpoints = false
		}
	}
}
