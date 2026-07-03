// The bind-chain walker: iterative DFS over a MATCH stage's ops, binding
// each level's candidates into the row and pruning with the pushed-down
// conjuncts (port of the Rust gen_matches). M15 executes scan ops;
// expansion ops land in M17 and are rejected at plan validation.
package exec

import (
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// genScratch holds the per-row DFS buffers, reused across a stage's row
// loop so a scan/expand does not allocate per row.
type genScratch struct {
	cand [][]graph.NodeID
	pos  []int
}

// genMatches walks the ops' bind chain over one input row, handing each
// completed match row to the sink by reference (the sink copies).
func genMatches(ctx *eval.Ctx, ops []plan.BindOp, base []value.Value, sc *stageComp, slots map[string]int, sink func([]value.Value), scratch *genScratch) {
	// New match-call epoch: a loop-invariant carried IN list hashes once
	// for this call and reuses it across the call's candidates.
	ctx.MatchEpoch++
	n := len(ops)
	if n == 0 {
		sink(base)
		return
	}
	for len(scratch.cand) < n {
		scratch.cand = append(scratch.cand, nil)
	}
	scratch.pos = append(scratch.pos[:0], make([]int, n)...)
	row := base

	levelCandidates(ctx, &ops[0], sc.matchers[0], row, &scratch.cand[0])
	cur := 0
	for {
		switch {
		case scratch.pos[cur] < len(scratch.cand[cur]):
			node := scratch.cand[cur][scratch.pos[cur]]
			scratch.pos[cur]++
			row[slotOf(&ops[cur])] = value.Node(node)
			// Pushed-down predicates for this level: any failing conjunct
			// abandons the candidate before deeper ops expand from it.
			ok := true
			for _, f := range sc.levelFilters[cur] {
				if !f.Eval(ctx, row, slots).IsTruthy() {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			if cur+1 == n {
				sink(row)
			} else {
				cur++
				levelCandidates(ctx, &ops[cur], sc.matchers[cur], row, &scratch.cand[cur])
				scratch.pos[cur] = 0
			}
		case cur == 0:
			return
		default:
			cur--
		}
	}
}

// slotOf is the node slot an op binds.
func slotOf(op *plan.BindOp) int {
	if op.Kind == plan.OpScan {
		return op.Slot
	}
	return op.To
}

// relSlotOf is the slot a single traversed relationship binds to (a named
// fixed-length expand); NoSlot otherwise.
func relSlotOf(op *plan.BindOp) int {
	if op.Kind == plan.OpExpand {
		return op.RelSlot
	}
	return plan.NoSlot
}

// levelCandidates fills the pooled candidate buffer for binding an op's
// node slot.
func levelCandidates(ctx *eval.Ctx, op *plan.BindOp, m *graph.NodeMatcher, row []value.Value, cand *[]graph.NodeID) {
	*cand = (*cand)[:0]
	switch op.Source.Kind {
	case plan.ScanArg:
		// The node is already bound in a row slot (carried in / reused
		// across MATCH clauses); re-test this op's own constraints.
		if id, ok := row[op.Source.Slot].AsNode(); ok && ctx.G.NodeMatcherAccepts(m, id) {
			*cand = append(*cand, id)
		}
	case plan.ScanNodeIDVar:
		// Per-row id seek: the slot holds the target id.
		if id, ok := nodeIDSeekValue(ctx, row[op.Source.Slot]); ok && ctx.G.NodeMatcherAccepts(m, id) {
			*cand = append(*cand, id)
		}
	default:
		freshScan(ctx, &op.Source, m, scanMatcherRedundant(op), cand)
	}
}
