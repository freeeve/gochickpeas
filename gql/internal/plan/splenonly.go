// Shortest-path materialization elision: an ANY SHORTEST stage whose
// bound path is read ONLY as length(p) does not need the path at all --
// the search's distance answers every read. The uses rewrite to a
// synthetic integer column aliasing the path slot and the stage is marked
// LengthOnly, so execution binds the hop count and skips the node-chain
// stitch, the per-hop relationship-position scan, the Path value, and the
// per-pair materialized-path memo. Any other use of the path (rels(p),
// nodes(p), a bare projection), the ALL form (path multiplicity is
// row multiplicity), and the weighted form all decline.
package plan

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// DisableSpLenOnly pins differential tests to the materialized-path
// execution; spLenElides counts rewrites so tests cannot pass vacuously.
var (
	DisableSpLenOnly bool
	spLenElides      int
)

// elideSpMaterialization rewrites each qualifying stage in place. It runs
// before gate injection so a hoisted GateStage copies the marked stage.
func elideSpMaterialization(segs []*Segment) {
	if DisableSpLenOnly {
		return
	}
	for i := range segs {
		elideSegmentSpLen(segs, i)
	}
}

func elideSegmentSpLen(segs []*Segment, segIdx int) {
	seg := segs[segIdx]
	for si, st := range seg.Stages {
		sp, ok := st.(*SpStage)
		if !ok || sp.All || sp.Weight != nil || sp.LengthOnly {
			continue
		}
		pName := ""
		for nm, s := range seg.Slots {
			if s == sp.PathSlot {
				pName = nm
			}
		}
		if pName == "" {
			continue
		}
		synth := "__plen_" + pName
		if _, taken := seg.Slots[synth]; taken {
			continue
		}
		// Two-phase like the named-path elision: trial-rewrite every
		// site the path variable is visible from; apply only when all
		// succeed.
		var sites []elideSite
		ok = true
		add := func(e ast.Expr, apply func(ast.Expr)) {
			if e == nil || !ok {
				return
			}
			if r, rok := rewriteLenUses(e, pName, synth); rok {
				if r != e {
					sites = append(sites, elideSite{r, apply})
				}
			} else {
				ok = false
			}
		}
		for sj := si + 1; sj < len(seg.Stages) && ok; sj++ {
			switch s2 := seg.Stages[sj].(type) {
			case *MatchStage:
				s2c := s2
				add(s2.Where, func(e ast.Expr) { s2c.Where = e })
			default:
				ok = !stageMentionsVar(seg.Stages[sj], pName)
			}
		}
		if !elideBoundaryChain(segs, segIdx, pName, synth, add, &sites, &ok) {
			ok = false
		}
		if !ok {
			continue
		}
		for _, s := range sites {
			s.apply(s.expr)
		}
		// The path name is REMOVED: only the synthetic length column
		// aliases the slot, so downstream passes (gate injection resolves
		// the slot's name from this map) see one deterministic name.
		seg.Slots[synth] = sp.PathSlot
		delete(seg.Slots, pName)
		sp.LengthOnly = true
		spLenElides++
	}
}
