// Anchor scan-source selection and pruning analysis for match-stage
// lowering: choosing an anchor's scan source (id seek, text-index scan,
// EXISTS-seed candidate scan, or the base label/property scan), deriving
// the backward seed walks from a correlated EXISTS, and flagging bounded
// var-expands whose duplicate endpoints can collapse under a
// multiplicity-insensitive projection. Split from build.go, which holds
// the segment and match-stage lowering.
package plan

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
)

// startScanSource picks the anchor's scan source: an id seek from a WHERE
// conjunct, a per-row id-variable seek, a substring-index candidate scan
// (labelled start, no inline anchor property), else the label/property/all
// scan.
func startScanSource(pattern *ast.Pattern, where ast.Expr, slots map[string]int, bound map[int]bool, g graph.Graph) ScanSource {
	sv := pattern.Start.Var
	if sv != "" {
		if lit := idSeekLiteral(where, sv); lit != nil {
			return ScanSource{Kind: ScanNodeID, Value: *lit}
		}
		if s := idSeekVar(where, sv, slots, bound); s != NoSlot {
			return ScanSource{Kind: ScanNodeIDVar, Slot: s}
		}
		// A value seek -- an inline prop OR a WHERE equality (bestPropSeek sees
		// both) -- outranks the substring / EXISTS-seed fallbacks; only reach
		// for those when nothing seeks by value.
		if _, hasSeek := bestPropSeek(&pattern.Start, where, g); !hasSeek {
			if len(pattern.Start.Labels) > 0 {
				if ts := textMatchSeek(where, sv); ts != nil {
					return ScanSource{Kind: ScanTextMatch, Label: pattern.Start.Labels[0], Field: ts.field, Mode: ts.mode, Value: ts.needle}
				}
			}
			// An EXISTS conjunct correlated to sv and anchored at a bound
			// variable narrows the scan to its candidate superset; the kept
			// conjunct finalizes. Only worth it against a broad base source.
			if seeds := existsSeedChains(where, sv, slots, bound); seeds != nil {
				src := scanSource(&pattern.Start, where, g)
				src.Kind = ScanExistsSeed
				src.Seeds = seeds
				return src
			}
		}
	}
	return scanSource(&pattern.Start, where, g)
}

// existsSeedChains recognizes a WHERE conjunct that is EXISTS { pattern }
// (or an OR of such EXISTSes, all of which must qualify) where the
// pattern runs between sv at one end and a bound variable at the other
// over fixed single hops. It returns one backward walk per disjunct
// (anchor -> sv, directions flipped when the anchor is the pattern's
// end), or nil when no conjunct qualifies.
func existsSeedChains(where ast.Expr, sv string, slots map[string]int, bound map[int]bool) []SeedChain {
	if where == nil {
		return nil
	}
	var conjs []ast.Expr
	SplitAnd(where, &conjs)
	for _, c := range conjs {
		if chains := seedDisjuncts(c, sv, slots, bound); chains != nil {
			return chains
		}
	}
	return nil
}

// seedDisjuncts qualifies one conjunct: a single EXISTS, or an OR tree
// whose every leaf is a qualifying EXISTS (a non-qualifying leaf voids
// the whole disjunction -- its rows would be missed).
func seedDisjuncts(e ast.Expr, sv string, slots map[string]int, bound map[int]bool) []SeedChain {
	switch n := e.(type) {
	case *ast.Binary:
		if n.Op != ast.OpOr {
			return nil
		}
		l := seedDisjuncts(n.LHS, sv, slots, bound)
		if l == nil {
			return nil
		}
		r := seedDisjuncts(n.RHS, sv, slots, bound)
		if r == nil {
			return nil
		}
		return append(l, r...)
	case *ast.Exists:
		if ch := seedChainOf(n.Pattern, sv, slots, bound); ch != nil {
			return []SeedChain{*ch}
		}
	}
	return nil
}

// seedChainOf builds the anchor->sv walk for one EXISTS pattern, or nil
// when the shape doesn't qualify (sv not at an end, no bound anchor at
// the other end, or a quantified hop).
func seedChainOf(p *ast.Pattern, sv string, slots map[string]int, bound map[int]bool) *SeedChain {
	if p == nil || len(p.Hops) == 0 {
		return nil
	}
	for _, h := range p.Hops {
		if h.Rel.Length != nil {
			return nil
		}
	}
	endVar := p.Hops[len(p.Hops)-1].Node.Var
	boundSlot := func(v string) (int, bool) {
		if v == "" || v == sv {
			return 0, false
		}
		s, ok := slots[v]
		return s, ok && bound[s]
	}
	if p.Start.Var == sv {
		if s, ok := boundSlot(endVar); ok {
			// Walk end -> start: hops reversed, directions flipped; each
			// step lands on the PRIOR hop's node (finally the start).
			ch := SeedChain{AnchorSlot: s}
			for i := len(p.Hops) - 1; i >= 0; i-- {
				land := &p.Start
				if i > 0 {
					land = &p.Hops[i-1].Node
				}
				ch.Hops = append(ch.Hops, SeedHop{
					Dir:    mapDir(p.Hops[i].Rel.Dir).Reverse(),
					Types:  p.Hops[i].Rel.Types,
					Labels: land.Labels,
				})
			}
			return &ch
		}
		return nil
	}
	if endVar == sv {
		if s, ok := boundSlot(p.Start.Var); ok {
			// Walk start -> end in pattern order.
			ch := SeedChain{AnchorSlot: s}
			for i := range p.Hops {
				ch.Hops = append(ch.Hops, SeedHop{
					Dir:    mapDir(p.Hops[i].Rel.Dir),
					Types:  p.Hops[i].Rel.Types,
					Labels: p.Hops[i].Node.Labels,
				})
			}
			return &ch
		}
	}
	return nil
}

// flagDedupEndpoints marks bounded var-expands whose relationships/path
// are unused so each distinct endpoint binds once under a DISTINCT
// projection (duplicate rows would collapse anyway). Conservative: only
// ops with no rel slot in a stage with no path bind, where no projected
// expression or ORDER BY key references a rel variable (none can -- the
// rel slot is unbound), qualify.
// projCannotSeeDuplicateRows reports whether the projection's output is
// invariant under dropping an EXACT-duplicate row (every column identical) --
// the precondition for collapsing a var-expand's per-trail rows to one row per
// endpoint. True for a plain DISTINCT, and for a grouped aggregation whose
// every aggregate is multiplicity-insensitive: a duplicate row lands in the
// same group (its keys are the same row) and contributes nothing new. Nested
// aggregate wrappers (Post) are declined conservatively -- the direct-aggregate
// case (e.g. LDBC IC6's count(DISTINCT ...)) is the one that matters.
func projCannotSeeDuplicateRows(proj *ProjPlan) bool {
	if proj.Distinct && !proj.Aggregated {
		return true
	}
	if !proj.Aggregated || len(proj.Aggs) == 0 || len(proj.Post) > 0 {
		return false
	}
	for i := range proj.Aggs {
		if !aggIgnoresRowMultiplicity(&proj.Aggs[i]) {
			return false
		}
	}
	return true
}

// aggIgnoresRowMultiplicity reports whether an aggregate's result is unchanged
// by dropping an exact-duplicate row. min/max are idempotent under duplicates;
// a DISTINCT count/sum/avg already ignores value multiplicity, so a same-value
// duplicate contributes nothing. count(*) / count(x) / sum / avg without
// DISTINCT grow with each duplicate, and collect is order-bearing even with
// DISTINCT (the list carries encounter order, which the collapse changes) --
// none of those are eligible.
func aggIgnoresRowMultiplicity(a *AggCol) bool {
	switch a.Kind {
	case AggMin, AggMax:
		return true
	case AggCollect:
		return false
	default: // AggCount, AggSum, AggAvg
		return a.Distinct
	}
}

func flagDedupEndpoints(stages []Stage, proj *ProjPlan, slots map[string]int) {
	_ = proj
	_ = slots
	for _, s := range stages {
		ms, ok := s.(*MatchStage)
		if !ok || ms.PathBind != nil {
			continue
		}
		for i := range ms.Ops {
			op := &ms.Ops[i]
			if op.Kind == OpVarExpand && op.Max != nil && op.RelSlot == NoSlot && op.RelPred == nil {
				op.DedupEndpoints = true
			}
		}
	}
}
