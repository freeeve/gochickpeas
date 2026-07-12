// Columnar-aggregate candidate marking: a segment whose whole job is a
// bare single-label scan feeding an aggregation is a candidate for the
// fused one-pass column operator (exec's tryColumnarAgg). The plan side
// checks only STRUCTURE -- stage and aggregate shapes -- and never looks
// at expressions or the graph; the executor classifies every filter,
// group key, and aggregate argument against typed columns at run time
// and falls back to the general chain when anything declines, so the
// marking is purely an opportunity, never a semantic commitment. The
// trigger is shape-generic by construction: any unseen query with the
// same structure is marked identically.
package plan

// lowerColumnarAggs marks each candidate chain head of a built branch.
func lowerColumnarAggs(segments []*Segment) {
	for i := range segments {
		segments[i].ColAgg = ColAggChainLen(segments, i) > 0
	}
}

// ColAggChainLen is the number of segments a columnar-aggregate fusion
// starting at i would consume: the scan segment itself when its own
// boundary aggregates, or the scan plus a run of stage-less pure per-row
// boundaries (the LET pipeline) ending in a stage-less aggregated
// segment. 0 means segment i heads no fusable chain. Shared with the
// executor so the plan marking and the runtime walk agree.
func ColAggChainLen(segments []*Segment, i int) int {
	seg := segments[i]
	if !colAggScanStage(seg) {
		return 0
	}
	if seg.Proj.Aggregated {
		if colAggProj(&seg.Proj) {
			return 1
		}
		return 0
	}
	if !colAggPassthrough(seg) {
		return 0
	}
	for j := i + 1; j < len(segments); j++ {
		s := segments[j]
		if len(s.Stages) != 0 {
			return 0
		}
		if s.Proj.Aggregated {
			if colAggProj(&s.Proj) {
				return j - i + 1
			}
			return 0
		}
		if !colAggPassthrough(s) {
			return 0
		}
	}
	return 0
}

// colAggScanStage reports the head shape: one non-optional bare
// single-label scan stage, no expands, no path bind, no inline props.
func colAggScanStage(seg *Segment) bool {
	if len(seg.Stages) != 1 {
		return false
	}
	ms, ok := seg.Stages[0].(*MatchStage)
	if !ok || ms.Optional || ms.PathBind != nil || len(ms.Ops) != 1 {
		return false
	}
	op := &ms.Ops[0]
	if op.Kind != OpScan || op.Source.Kind != ScanLabel {
		return false
	}
	return len(op.Props) == 0 && len(op.Labels) <= 1
}

// colAggPassthrough reports a pure per-row boundary with no filter: rows
// map 1:1 through it, so the fused pass can absorb its derived columns.
func colAggPassthrough(seg *Segment) bool {
	p := &seg.Proj
	return !p.Aggregated && !p.Distinct && len(p.OrderBy) == 0 &&
		p.Skip == nil && p.Limit == nil && len(p.Post) == 0 &&
		p.NHidden == 0 && seg.PostWhere == nil
}

// colAggProj reports a fusable aggregated boundary: non-DISTINCT, only
// count/sum aggregates without DISTINCT.
func colAggProj(p *ProjPlan) bool {
	if p.Distinct {
		return false
	}
	for _, a := range p.Aggs {
		if a.Distinct || (a.Kind != AggCount && a.Kind != AggSum) {
			return false
		}
	}
	return true
}
