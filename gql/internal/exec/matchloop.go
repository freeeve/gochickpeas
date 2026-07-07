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
// loop so a scan/expand does not allocate per row. walk and dedup serve the
// bounded trail enumeration the same way reach serves the reachability BFS
// (one live walk per stage at a time).
type genScratch struct {
	cand      [][]graph.NodeID
	candRel   [][]uint32
	candData  [][]uint32
	candRange [][][2]int
	// candPairData/candPairRange carry a CONTRIBUTING var-expand's
	// per-trail uniqueness pairs (flat arena + (start, len) per
	// candidate), pushed onto the row's used stack while the candidate is
	// bound.
	candPairData  [][][2]graph.NodeID
	candPairRange [][][2]int
	// uniqPushed counts each level's live pair pushes, retired on the
	// level's next bind, exhaustion, or row end.
	uniqPushed []int
	pos        []int
	reach      reachScratch
	walk       varWalk
	dedup      map[graph.NodeID]struct{}
	semiBuf    []graph.NodeID
}

// reachScratch holds varReach's dedup'd-BFS working sets, reused across a
// stage's row loop (the walk runs to completion per call and is never
// nested, so a single set serves every var-length reach in the stage).
type reachScratch struct {
	expanded map[graph.NodeID]struct{}
	emitted  map[graph.NodeID]struct{}
	frontier []graph.NodeID
	next     []graph.NodeID
	nbuf     []graph.NodeID
	// pbuf carries rel positions parallel to nbuf when the hop gate's
	// predicate needs them.
	pbuf []uint32
}

// genMatches walks the ops' bind chain over one input row, handing each
// completed match row to the sink by reference (the sink copies). opRows
// is PROFILE's counter slice (one slot per op for bindings produced, plus
// a final slot for rows passing the stage WHERE); nil when not profiling.
func genMatches(ctx *eval.Ctx, ops []plan.BindOp, base []value.Value, sc *stageComp, slots map[string]int, uniq *uniqEnv, sink func([]value.Value), scratch *genScratch, opRows []uint64) {
	// New match-call epoch: a loop-invariant carried IN list hashes once
	// for this call and reuses it across the call's candidates.
	ctx.MatchEpoch++
	n := len(ops)
	if n == 0 {
		sink(base)
		if opRows != nil {
			opRows[0]++
		}
		return
	}
	for len(scratch.cand) < n {
		scratch.cand = append(scratch.cand, nil)
		scratch.candRel = append(scratch.candRel, nil)
		scratch.candData = append(scratch.candData, nil)
		scratch.candRange = append(scratch.candRange, nil)
		scratch.candPairData = append(scratch.candPairData, nil)
		scratch.candPairRange = append(scratch.candPairRange, nil)
	}
	scratch.pos = append(scratch.pos[:0], make([]int, n)...)
	scratch.uniqPushed = append(scratch.uniqPushed[:0], make([]int, n)...)
	// retire pops a level's live pair pushes off the used stack.
	retire := func(cur int) {
		if scratch.uniqPushed[cur] > 0 {
			uniq.stack = uniq.stack[:len(uniq.stack)-scratch.uniqPushed[cur]]
			scratch.uniqPushed[cur] = 0
		}
	}
	row := base

	levelCandidates(ctx, &ops[0], sc, 0, row, uniq, scratch)
	cur := 0
	for {
		switch {
		case scratch.pos[cur] < len(scratch.cand[cur]):
			p := scratch.pos[cur]
			node := scratch.cand[cur][p]
			scratch.pos[cur]++
			// MATCH-scope rel uniqueness: retire the PREVIOUS candidate's
			// pair pushes at this level, then check/push this candidate's.
			// An Expand keys the single hop (from, node); a tracked
			// var-length op's per-trail pairs were collected at gather time
			// (with the scope's used pairs already excluded during the
			// walk, so only the push remains).
			retire(cur)
			if ru := ops[cur].Uniq; ru != nil {
				switch ops[cur].Kind {
				case plan.OpExpand:
					if f, ok := row[ops[cur].From].AsNode(); ok {
						a, b := uniqPair(ops[cur].Dir, f, node)
						if ru.Check && uniq.used(ru.Scope, a, b) {
							continue
						}
						if ru.Contribute {
							uniq.stack = append(uniq.stack, uniqKey{scope: ru.Scope, a: a, b: b})
							scratch.uniqPushed[cur] = 1
						}
					}
				case plan.OpVarExpand:
					if ru.Contribute {
						rng := scratch.candPairRange[cur][p]
						for _, pr := range scratch.candPairData[cur][rng[0] : rng[0]+rng[1]] {
							uniq.stack = append(uniq.stack, uniqKey{scope: ru.Scope, a: pr[0], b: pr[1]})
						}
						scratch.uniqPushed[cur] = rng[1]
					}
				}
			}
			row[slotOf(&ops[cur])] = value.Node(node)
			// A named fixed-hop relationship binds its position; a named
			// variable-length relationship binds the trail's rel list.
			if ops[cur].Kind == plan.OpExpand && ops[cur].RelSlot != plan.NoSlot {
				row[ops[cur].RelSlot] = value.Rel(scratch.candRel[cur][p])
			}
			if ops[cur].Kind == plan.OpVarExpand && ops[cur].RelSlot != plan.NoSlot {
				rng := scratch.candRange[cur][p]
				rels := make([]value.Value, rng[1])
				for i := range rng[1] {
					rels[i] = value.Rel(scratch.candData[cur][rng[0]+i])
				}
				row[ops[cur].RelSlot] = value.List(rels)
			}
			// PROFILE: the binding counts before the level filters prune,
			// so pushdown effectiveness is visible per op.
			if opRows != nil {
				opRows[cur]++
			}
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
				if opRows != nil {
					opRows[n]++
				}
			} else {
				cur++
				levelCandidates(ctx, &ops[cur], sc, cur, row, uniq, scratch)
				scratch.pos[cur] = 0
			}
		case cur == 0:
			// Retire the last candidate's pair pushes so the env is empty
			// for the next row.
			retire(0)
			return
		default:
			retire(cur)
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

// levelCandidates fills the pooled candidate buffers for binding an op's
// node slot (and, for named relationships, its rel position buffers).
func levelCandidates(ctx *eval.Ctx, op *plan.BindOp, sc *stageComp, i int, row []value.Value, uniq *uniqEnv, scratch *genScratch) {
	m := sc.matchers[i]
	cand := &scratch.cand[i]
	*cand = (*cand)[:0]
	scratch.candRel[i] = scratch.candRel[i][:0]
	scratch.candData[i] = scratch.candData[i][:0]
	scratch.candRange[i] = scratch.candRange[i][:0]
	scratch.candPairData[i] = scratch.candPairData[i][:0]
	scratch.candPairRange[i] = scratch.candPairRange[i][:0]
	switch op.Kind {
	case plan.OpExpand:
		if sc.semijoins[i] != nil {
			semijoinCandidates(ctx, op, m, sc.relMatchers[i], sc.semijoins[i], row, cand, &scratch.semiBuf)
			return
		}
		expandCandidates(ctx, op, m, sc.relMatchers[i], row, cand, &scratch.candRel[i])
		return
	case plan.OpVarExpand:
		varExpandCandidates(ctx, op, m, sc.relMatchers[i], sc.hopGates[i], row, uniq, cand, &scratch.candData[i], &scratch.candRange[i], &scratch.candPairData[i], &scratch.candPairRange[i], scratch)
		return
	}
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
