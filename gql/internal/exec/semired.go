// Semijoin reduction for constant-anchored chains (task 205 round 8): a
// run of anonymous expand hops that leads from a pattern variable into a
// node that is CONSTANT across the stage's input rows is a fixed unary
// predicate on that variable -- (c)-[:LOC]->(:City)-[:PART]->(country)
// with country identical on every seeded row tests nothing but c. The
// chain's ops are absorbed: the qualifying node set is enumerated ONCE by
// walking the chain backwards from the constant, and the variable's
// binding level sweeps candidates against it at fill time, so the
// per-row hop enumeration (and its row traffic) disappears. Fires on
// generic structure -- an into-bound tail whose target the existing
// batch-constant analysis proves invariant -- never on query identity.
package exec

import (
	"slices"

	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// constChainAbsorbs counts absorbed chains -- the engagement oracle: a
// qualifying chain must absorb exactly once per stage compile; 0 on the
// qualifying shape means every row still walks the hops.
var constChainAbsorbs int

// disableConstChainAbsorb pins result identity in tests: the general
// per-row enumeration must produce exactly the rows the absorbed form
// does.
var disableConstChainAbsorb bool

// absorbedStage is compileStage's rewritten view of a stage whose
// constant-anchored chains were absorbed.
type absorbedStage struct {
	ops []plan.BindOp
	// profMap maps an exec-op index to its original index, so PROFILE
	// counts land on the right operator lines (absorbed ops report 0).
	profMap []int
	// gates test the chains' carried head variables, once per input row.
	gates []memberGate
}

// memberGate tests a CARRIED slot (bound upstream of this stage) against
// an absorbed chain's qualifying set, once per input row.
type memberGate struct {
	slot int
	set  []uint32
}

func (g *memberGate) pass(row []value.Value) bool {
	id, ok := row[g.slot].AsNode()
	if !ok {
		return false
	}
	_, found := slices.BinarySearch(g.set, uint32(id))
	return found
}

// absorbConstChains detects and absorbs constant-anchored chains. Every
// gate fails closed:
//   - the segment's projection must be DISTINCT (absorbing collapses the
//     chain's match multiplicity, which only a distinct boundary erases);
//   - the stage must not be OPTIONAL and must bind no named path;
//   - every chain op is a plain fixed expand with no named rel, no
//     uniqueness participation, and a fresh, unnamed, otherwise-unused
//     intermediate node;
//   - the tail is an into-bound expand whose target slot every seeded
//     input agrees on (uniformIn -- unlike constIn it tolerates the
//     rebind itself, which never changes a bound value);
//   - the variable under test is bound by an untracked level (its fill
//     sweeps run only there).
func absorbConstChains(ctx *eval.Ctx, stage *plan.MatchStage, slots map[string]int, uniformIn func(int) bool, sample []value.Value, segDistinct bool) *absorbedStage {
	if disableConstChainAbsorb || !segDistinct || stage.Optional || stage.PathBind != nil {
		return nil
	}
	ops := stage.Ops
	named := map[int]bool{}
	for _, s := range slots {
		named[s] = true
	}
	// slotUses counts references to a slot across all ops, so an
	// intermediate consumed by anything outside its chain refuses.
	slotUses := map[int]int{}
	use := func(s int) {
		if s >= 0 && s != plan.NoSlot {
			slotUses[s]++
		}
	}
	for i := range ops {
		op := &ops[i]
		if op.Kind == plan.OpScan {
			use(op.Slot)
		} else {
			use(op.From)
			use(op.To)
			use(op.RelSlot)
		}
	}

	chainOp := func(op *plan.BindOp) bool {
		return op.Kind == plan.OpExpand && op.Uniq == nil && op.RelSlot == plan.NoSlot
	}
	absorbedIdx := map[int]bool{}
	var gates []memberGate
	for j := len(ops) - 1; j >= 1; j-- {
		tail := &ops[j]
		if absorbedIdx[j] || !chainOp(tail) || !tail.Rebind {
			continue
		}
		t := tail.To
		if !uniformIn(t) || t >= len(sample) {
			continue
		}
		constNode, isNode := sample[t].AsNode()
		if !isNode {
			continue
		}
		// Extend the chain upward through fresh anonymous intermediates:
		// ops[i..j] with ops[k].From == ops[k-1].To.
		i := j
		for i-1 >= 0 {
			prev := &ops[i-1]
			link := ops[i].From
			if !chainOp(prev) || prev.Rebind || prev.To != link {
				break
			}
			// The intermediate must be invisible: unnamed and read by
			// exactly its two chain neighbors.
			if named[link] || slotUses[link] != 2 {
				break
			}
			i--
		}
		v := ops[i].From
		// Only a CARRIED head variable absorbs: when this stage itself
		// binds v fresh, the planner saw the whole pattern and anchoring
		// from the constant was its call -- a per-row gate could not run
		// before v binds anyway. A ScanArg re-scan of v does NOT count:
		// it re-tests a value the row already carries (the gate and the
		// re-test are both filters, order-independent).
		carried := true
		for k := 0; k < i; k++ {
			op := &ops[k]
			if op.Kind == plan.OpScan && op.Source.Kind == plan.ScanArg {
				continue
			}
			if slotOf(op) == v {
				carried = false
			}
		}
		if !carried {
			continue
		}
		set := chainMembers(ctx, ops[i:j+1], constNode)
		for k := i; k <= j; k++ {
			absorbedIdx[k] = true
		}
		gates = append(gates, memberGate{slot: v, set: set})
		constChainAbsorbs++
		j = i // resume scanning above the chain
	}
	if len(gates) == 0 {
		return nil
	}
	ab := &absorbedStage{gates: gates}
	for k := range ops {
		if !absorbedIdx[k] {
			ab.ops = append(ab.ops, ops[k])
			ab.profMap = append(ab.profMap, k)
		}
	}
	return ab
}

// chainMembers enumerates the nodes that can start the chain, walking it
// backwards from the constant tail node: each step collects the reversed
// neighbors over the op's rel types and applies the PREVIOUS op's target
// matcher (the constraints the forward walk would have tested on that
// intermediate). The result is sorted for binary-search membership.
func chainMembers(ctx *eval.Ctx, chain []plan.BindOp, constNode graph.NodeID) []uint32 {
	last := &chain[len(chain)-1]
	props := make([]graph.PropSpec, len(last.Props))
	for i, p := range last.Props {
		props[i] = graph.PropSpec{Key: p.Key, Val: eval.LitValue(ctx, p.Val)}
	}
	if !ctx.G.NodeMatcherAccepts(ctx.G.CompileNodeMatcher(last.Labels, props), constNode) {
		return nil
	}
	frontier := []graph.NodeID{constNode}
	var next []graph.NodeID
	for k := len(chain) - 1; k >= 0; k-- {
		op := &chain[k]
		rm := ctx.G.CompileRelMatcher(op.Types)
		var nm *graph.NodeMatcher
		if k > 0 {
			p := &chain[k-1]
			pp := make([]graph.PropSpec, len(p.Props))
			for i, pe := range p.Props {
				pp[i] = graph.PropSpec{Key: pe.Key, Val: eval.LitValue(ctx, pe.Val)}
			}
			nm = ctx.G.CompileNodeMatcher(p.Labels, pp)
		}
		next = next[:0]
		for _, f := range frontier {
			next = ctx.G.AppendNeighborsMatched(next, f, flipDir(op.Dir), rm)
		}
		slices.Sort(next)
		next = slices.Compact(next)
		if nm != nil {
			w := 0
			for _, n := range next {
				if ctx.G.NodeMatcherAccepts(nm, n) {
					next[w] = n
					w++
				}
			}
			next = next[:w]
		}
		frontier, next = next, frontier
	}
	set := make([]uint32, len(frontier))
	for i, n := range frontier {
		set[i] = uint32(n)
	}
	slices.Sort(set)
	return set
}
