// Hash-join detection: the cost model that decides where the rewrite in
// hashjoin.go applies. hashJoinOnce walks the stage list once, estimating
// each stage's input/output cardinality and the cumulative estimate at
// which every slot was first bound, then gates pivots on the thresholds
// below -- a stage worth protecting from a multiplying re-expansion. All
// decisions read estimated cardinality structure, never query identity.
package plan

import "github.com/freeeve/gochickpeas/gql/internal/graph"

// The decision thresholds are variables (not consts) so tests can force
// the rewrite onto small fixture graphs and compare both execution paths.
var (
	// HashJoinMinRows is the smallest estimated intermediate worth
	// protecting from a multiplying re-expansion.
	HashJoinMinRows = 1 << 16
	// HashJoinFanFactor is the minimum estimated output/input multiply of
	// the pivot stage before the rewrite is considered.
	HashJoinFanFactor = 64.0
	// HashJoinExtDivisor bounds the branch's external references: a slot
	// qualifies only when it was bound at a cumulative estimate at most
	// estIn(pivot)/HashJoinExtDivisor, which also bounds the number of
	// distinct memo tuples (re-builds) to the same fraction.
	HashJoinExtDivisor = 256.0
	// HashJoinMaxBuildRows caps the estimated materialized branch (a
	// memory bound; the win condition is the pivot gate's multiply).
	HashJoinMaxBuildRows = 1 << 22
)

// hashJoinStages applies the rewrite repeatedly until no pivot
// qualifies -- a segment can hold several independent multiplying
// branches (each pass consumes its pivot's ops, so the loop terminates).
func hashJoinStages(stages []Stage, slots map[string]int, inWidth int, g graph.Graph) []Stage {
	for range len(stages) + 1 {
		out := hashJoinOnce(stages, slots, inWidth, g)
		if out == nil {
			return stages
		}
		stages = out
	}
	return stages
}

func hashJoinOnce(stages []Stage, slots map[string]int, inWidth int, g graph.Graph) []Stage {
	boundEst := make(map[int]float64, inWidth+8)
	boundStage := make(map[int]int, inWidth+8)
	for s := range inWidth {
		boundEst[s] = 1
		boundStage[s] = -1
	}
	claim := func(slot, si int, est float64) {
		if slot < 0 {
			return
		}
		if _, seen := boundEst[slot]; !seen {
			boundEst[slot] = est
			boundStage[slot] = si
		}
	}
	rows := 1.0
	stageIn := make([]float64, len(stages))
	stageOut := make([]float64, len(stages))
	// Slots proven to hold exactly one concrete node (a resolved seek):
	// later hops from these price at the node's real degree, so the pivot
	// and build gates decide on fact where the type average would lie.
	resolved := make(map[int]graph.NodeID)
	for si, st := range stages {
		stageIn[si] = rows
		switch s := st.(type) {
		case *MatchStage:
			for oi := range s.Ops {
				op := &s.Ops[oi]
				if op.Kind != OpScan || op.Source.Kind == ScanArg {
					continue
				}
				if nodes, ok := resolveScanNodes(&op.Source, op.Labels, op.Props, g); ok && len(nodes) == 1 {
					resolved[op.Slot] = nodes[0]
				}
			}
			ests, out := matchEstAnchored(s, rows, g, resolved)
			for oi := range s.Ops {
				op := &s.Ops[oi]
				if op.Kind == OpScan {
					if op.Source.Kind != ScanArg {
						claim(op.Slot, si, float64(ests[oi]))
					}
				} else if !op.Rebind {
					claim(op.To, si, float64(ests[oi]))
				}
				if op.Kind != OpScan {
					claim(op.RelSlot, si, float64(ests[oi]))
				}
			}
			if s.PathBind != nil {
				claim(s.PathBind.PathSlot, si, out)
			}
			rows = out
		case *UnwindStage:
			rows *= unwindFanout
			claim(s.OutSlot, si, rows)
		case *SpStage:
			claim(s.PathSlot, si, rows)
		case *GateStage:
			claim(s.Sp.PathSlot, si, rows)
			for _, d := range s.Derived {
				claim(d.Slot, si, rows)
			}
		case *CallStage:
			claim(s.NodeSlot, si, rows)
			claim(s.ValueSlot, si, rows)
			claim(s.DepthSlot, si, rows)
		case *CallSubqueryStage:
			for _, o := range s.OutSlots {
				claim(o, si, rows)
			}
		}
		stageOut[si] = rows
	}
	for k := range stages {
		ms, ok := stages[k].(*MatchStage)
		if !ok || ms.Optional || ms.PathBind != nil {
			continue
		}
		if stageIn[k] < float64(HashJoinMinRows) || stageOut[k] < stageIn[k]*HashJoinFanFactor {
			continue
		}
		if out := tryHashJoin(stages, k, stageIn[k], boundEst, boundStage, resolved, slots, g); out != nil {
			return out
		}
	}
	return nil
}
