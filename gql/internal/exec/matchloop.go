// The bind-chain walker: iterative DFS over a MATCH stage's ops, binding
// each level's candidates into the row and pruning with the pushed-down
// conjuncts (port of the Rust gen_matches). M15 executes scan ops;
// expansion ops land in M17 and are rejected at plan validation.
package exec

import (
	chickpeas "github.com/freeeve/gochickpeas"
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
	// swept marks levels whose specialized predicates already ran over
	// the freshly filled candidate buffer (the fill-time sweep), so the
	// pop loop skips them and counts bindings from the sweep's credit.
	swept []bool
	// keep is the sweep's columnar mask, reused across levels.
	keep []bool
	// chainRoots / chainFunc cache each var-expand op's chain-collapse
	// and functionality resolutions for this execution (presence =
	// checked). Plan ops are shared across cached executions, so the
	// resolutions cannot live on the op itself.
	chainRoots map[*plan.BindOp]chickpeas.RootsVia
	chainFunc  map[*plan.BindOp]bool
	// seedFrontier/seedNext are the EXISTS-seed walk's level buffers.
	seedFrontier []graph.NodeID
	seedNext     []graph.NodeID
}

// chainRootsFor resolves (once per op per execution) whether op's
// reachable walk collapses to a root-array lookup.
func (s *genScratch) chainRootsFor(ctx *eval.Ctx, op *plan.BindOp) (chickpeas.RootsVia, bool) {
	if s.chainRoots == nil {
		s.chainRoots = map[*plan.BindOp]chickpeas.RootsVia{}
	}
	roots, seen := s.chainRoots[op]
	if !seen {
		roots, _ = ctx.G.ChainRootsVia(op.Types, op.Dir, op.Labels)
		s.chainRoots[op] = roots
	}
	return roots, roots != nil
}

// chainFuncFor resolves (once per op per execution) whether op's rel
// type is functional in its direction.
func (s *genScratch) chainFuncFor(ctx *eval.Ctx, op *plan.BindOp) bool {
	if s.chainFunc == nil {
		s.chainFunc = map[*plan.BindOp]bool{}
	}
	ok, seen := s.chainFunc[op]
	if !seen {
		ok = op.Dir != chickpeas.Both && ctx.G.FunctionalVia(op.Types, op.Dir)
		s.chainFunc[op] = ok
	}
	return ok
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
// Reports the sink's keep-going verdict: on a stop the walk abandons its
// remaining candidates, retiring every level's pair pushes on the way out
// so the shared uniqueness env is exactly as empty as a completed walk
// leaves it.
func genMatches(ctx *eval.Ctx, ops []plan.BindOp, base []value.Value, sc *stageComp, slots map[string]int, uniq *uniqEnv, sink func([]value.Value) bool, scratch *genScratch, opRows []uint64) bool {
	// New match-call epoch: a loop-invariant carried IN list hashes once
	// for this call and reuses it across the call's candidates.
	ctx.MatchEpoch++
	n := len(ops)
	if n == 0 {
		more := sink(base)
		if opRows != nil {
			opRows[0]++
		}
		return more
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
	scratch.swept = append(scratch.swept[:0], make([]bool, n)...)
	// retire pops a level's live pair pushes off the used stack.
	retire := func(cur int) {
		if scratch.uniqPushed[cur] > 0 {
			uniq.stack = uniq.stack[:len(uniq.stack)-scratch.uniqPushed[cur]]
			scratch.uniqPushed[cur] = 0
		}
	}
	row := base

	levelCandidates(ctx, &ops[0], sc, 0, row, uniq, scratch)
	scratch.swept[0] = sweepLevel(ctx, &ops[0], sc, 0, row, scratch, opRows)
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
						// Capture mode (hash-join build) also records a
						// Check-only op's pair as a dead entry, for the
						// probe's replay against the outer env.
						if ru.Contribute || (uniq.capture && ru.Check) {
							uniq.stack = append(uniq.stack, uniqKey{scope: ru.Scope, a: a, b: b, dead: !ru.Contribute, check: ru.Check})
							scratch.uniqPushed[cur] = 1
						}
					}
				case plan.OpVarExpand:
					if ru.Contribute {
						rng := scratch.candPairRange[cur][p]
						for _, pr := range scratch.candPairData[cur][rng[0] : rng[0]+rng[1]] {
							uniq.stack = append(uniq.stack, uniqKey{scope: ru.Scope, a: pr[0], b: pr[1], check: ru.Check})
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
			// so pushdown effectiveness is visible per op (a swept level
			// credited its pre-sweep volume at fill time).
			if opRows != nil && !scratch.swept[cur] {
				opRows[cur]++
			}
			// Pushed-down predicates for this level: any failing conjunct
			// abandons the candidate before deeper ops expand from it.
			// Specialized per-candidate predicates ran at fill time on a
			// swept level and run here otherwise; then the general filters.
			ok := true
			if !scratch.swept[cur] {
				for _, p := range sc.levelPreds[cur] {
					if !p(ctx, row, node) {
						ok = false
						break
					}
				}
			}
			if ok {
				for _, f := range sc.levelFilters[cur] {
					if !f.Eval(ctx, row, slots).IsTruthy() {
						ok = false
						break
					}
				}
			}
			if !ok {
				continue
			}
			if cur+1 == n {
				more := sink(row)
				if opRows != nil {
					opRows[n]++
				}
				if !more {
					// Stop: unwind every live level's pair pushes exactly
					// as the exhausted path below would, then tell the
					// caller to stop too.
					for ; cur > 0; cur-- {
						retire(cur)
					}
					retire(0)
					return false
				}
			} else {
				cur++
				levelCandidates(ctx, &ops[cur], sc, cur, row, uniq, scratch)
				scratch.swept[cur] = sweepLevel(ctx, &ops[cur], sc, cur, row, scratch, opRows)
				scratch.pos[cur] = 0
			}
		case cur == 0:
			// Retire the last candidate's pair pushes so the env is empty
			// for the next row.
			retire(0)
			return true
		default:
			retire(cur)
			cur--
		}
	}
}

// sweepLevel runs a level's specialized predicates over the freshly
// filled candidate buffer, compacting survivors in place (parallel
// rel-position and range buffers stay in sync) and crediting the level's
// PROFILE binding count with the pre-sweep volume. Every buffered
// candidate is popped exactly once and the DFS never exits early, so
// fill-time counting equals the former pop-time counting EXCEPT when the
// op is uniqueness-tracked -- a uniq-rejected candidate must keep not
// counting -- so tracked ops keep the pop-time path (reported false).
func sweepLevel(ctx *eval.Ctx, op *plan.BindOp, sc *stageComp, cur int, row []value.Value, scratch *genScratch, opRows []uint64) bool {
	preds := sc.levelPreds[cur]
	batch := sc.levelBatch[cur]
	if (len(preds) == 0 && len(batch) == 0) || op.Uniq != nil {
		return false
	}
	cand := scratch.cand[cur]
	if opRows != nil {
		opRows[cur] += uint64(len(cand))
	}
	// Columnar conjuncts first: each sweeps the whole buffer as one
	// typed pass over a keep mask, then a single compaction applies the
	// remaining per-candidate predicates to the survivors.
	var keep []bool
	if len(batch) > 0 {
		if cap(scratch.keep) < len(cand) {
			scratch.keep = make([]bool, len(cand))
		}
		keep = scratch.keep[:len(cand)]
		for i := range keep {
			keep[i] = true
		}
		for _, b := range batch {
			b(ctx, row, cand, keep)
		}
	}
	rel := scratch.candRel[cur]
	rng := scratch.candRange[cur]
	relPar := len(rel) == len(cand)
	rngPar := len(rng) == len(cand)
	kept := 0
	for i, id := range cand {
		if keep != nil && !keep[i] {
			continue
		}
		ok := true
		for _, p := range preds {
			if !p(ctx, row, id) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		cand[kept] = id
		if relPar {
			rel[kept] = rel[i]
		}
		if rngPar {
			rng[kept] = rng[i]
		}
		kept++
	}
	scratch.cand[cur] = cand[:kept]
	if relPar {
		scratch.candRel[cur] = rel[:kept]
	}
	if rngPar {
		scratch.candRange[cur] = rng[:kept]
	}
	return true
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
	case plan.ScanExistsSeed:
		// EXISTS-driven candidate superset; past the fan-out cap fall
		// back to the base source (the kept conjunct finalizes either
		// way).
		if !existsSeedCandidates(ctx, op, m, sc.seedRel[i], sc.seedNode[i], row, cand, scratch) {
			base := op.Source
			base.Kind = baseScanKind(&op.Source)
			freshScan(ctx, &base, m, false, cand)
		}
	default:
		freshScan(ctx, &op.Source, m, scanMatcherRedundant(op), cand)
	}
}

// baseScanKind is the source a ScanExistsSeed degrades to when its walk
// is abandoned: the label scan when one exists, else every node.
func baseScanKind(src *plan.ScanSource) plan.ScanKind {
	if src.Label != "" {
		return plan.ScanLabel
	}
	return plan.ScanAll
}
