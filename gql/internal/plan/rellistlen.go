// Rel-list length elision: a variable-length hop's bound relationship
// list that every reader consumes only as size(x) does not need the list
// -- the trail's hop count answers every read. The uses rewrite to a
// synthetic integer column aliasing the slot and the op is marked
// RelLenOnly, so execution binds the count and skips the per-row rel
// slice and List box that dominate trail-enumeration allocation (the
// FinBench CR1 shape after the named-path elision and dead-LET nulling
// have reduced its comprehension to a size read). Any other use of the
// list (indexing, iteration, a bare output projection) declines; a list
// still feeding a live PathBind's assembly declines outright.
//
// Runs LAST in the segment-rewrite pipeline: the named-path elision and
// the dead-LET reductions first collapse rels(p)-derived reads down to
// size(<rel-list column>), which is exactly the form this pass elides.
package plan

import (
	"sort"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// DisableRelLenOnly pins differential tests to the materialized-list
// execution; relLenElides counts rewrites so tests cannot pass vacuously.
var (
	DisableRelLenOnly bool
	relLenElides      int
)

func elideRelListLens(segs []*Segment) {
	if DisableRelLenOnly {
		return
	}
	for i := range segs {
		elideSegmentRelLens(segs, i)
	}
}

func elideSegmentRelLens(segs []*Segment, segIdx int) {
	seg := segs[segIdx]
	for si, st := range seg.Stages {
		ms, ok := st.(*MatchStage)
		if !ok {
			continue
		}
		for oi := range ms.Ops {
			op := &ms.Ops[oi]
			if op.Kind != OpVarExpand || op.RelSlot == NoSlot || op.RelLenOnly {
				continue
			}
			// A surviving path bind assembles from the rel list; the
			// named-path elision must have removed it first.
			if ms.PathBind != nil && ms.PathBind.RelsSlot == op.RelSlot {
				continue
			}
			// Every name aliasing the slot must convert. More than one
			// READ name would need coordinated rewrites (and never arises
			// from the pipeline: the path elision leaves one alias);
			// unread extra aliases are simply dropped with the rewrite.
			var names []string
			for nm, s := range seg.Slots {
				if s == op.RelSlot {
					names = append(names, nm)
				}
			}
			if len(names) == 0 {
				continue
			}
			sort.Strings(names)
			read := readNames(segs, segIdx, si, names)
			if len(read) > 1 {
				continue
			}
			synth := "__rellen_" + names[0]
			if _, taken := seg.Slots[synth]; taken {
				continue
			}
			ok2 := true
			var sites []elideSite
			if len(read) == 1 {
				pName := read[0]
				add := func(e ast.Expr, apply func(ast.Expr)) {
					if e == nil || !ok2 {
						return
					}
					if r, rok := rewriteSizeUses(e, pName, synth); rok {
						if r != e {
							sites = append(sites, elideSite{r, apply})
						}
					} else {
						ok2 = false
					}
				}
				for sj := si; sj < len(seg.Stages) && ok2; sj++ {
					switch s2 := seg.Stages[sj].(type) {
					case *MatchStage:
						s2c := s2
						add(s2.Where, func(e ast.Expr) { s2c.Where = e })
					default:
						ok2 = !stageMentionsVar(seg.Stages[sj], pName)
					}
				}
				if !elideBoundaryChain(segs, segIdx, pName, synth, add, &sites, &ok2) {
					ok2 = false
				}
			}
			if !ok2 {
				continue
			}
			for _, s := range sites {
				s.apply(s.expr)
			}
			for _, nm := range names {
				delete(seg.Slots, nm)
			}
			seg.Slots[synth] = op.RelSlot
			op.RelLenOnly = true
			relLenElides++
		}
	}
}

// readNames filters names down to those actually read anywhere the
// segment's bindings are visible: this segment's stage expressions and
// boundary, plus all downstream segments (a bare carry keeps the name
// alive downstream, so the conservative scan just checks everything).
func readNames(segs []*Segment, segIdx, si int, names []string) []string {
	var read []string
	for _, nm := range names {
		if nameRead(segs, segIdx, si, nm) {
			read = append(read, nm)
		}
	}
	return read
}

func nameRead(segs []*Segment, segIdx, si int, nm string) bool {
	seg := segs[segIdx]
	for sj := si; sj < len(seg.Stages); sj++ {
		if ms, ok := seg.Stages[sj].(*MatchStage); ok {
			if MentionsVar(ms.Where, nm) {
				return true
			}
			continue
		}
		if stageMentionsVar(seg.Stages[sj], nm) {
			return true
		}
	}
	for k := segIdx; k < len(segs); k++ {
		s := segs[k]
		if k > segIdx {
			for _, st := range s.Stages {
				if ms, ok := st.(*MatchStage); ok {
					if MentionsVar(ms.Where, nm) {
						return true
					}
					continue
				}
				if stageMentionsVar(st, nm) {
					return true
				}
			}
		}
		p := &s.Proj
		for i := range p.Returns {
			if MentionsVar(p.Returns[i].Expr, nm) {
				return true
			}
		}
		for i := range p.Aggs {
			if MentionsVar(p.Aggs[i].Arg, nm) || MentionsVar(p.Aggs[i].Arg2, nm) {
				return true
			}
		}
		for i := range p.Post {
			if MentionsVar(p.Post[i].Expr, nm) {
				return true
			}
		}
		for i := range p.OrderBy {
			if MentionsVar(p.OrderBy[i].Expr, nm) {
				return true
			}
		}
		if MentionsVar(s.PostWhere, nm) {
			return true
		}
		if s.CSEResidual != nil && MentionsVar(s.CSEResidual, nm) {
			return true
		}
	}
	return false
}
