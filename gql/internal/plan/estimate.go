// Heuristic forward cardinality estimates for EXPLAIN/PROFILE (port of
// the Rust estimate.rs): one estimated output row-count per operator,
// propagated from the anchor's exact leaf cardinality through each
// expand's average fan-out, plus the reconstructed [anchor: ...] note for
// each anchored MATCH stage. Estimates describe the shape of cardinality
// growth, not a guarantee; they render unconditionally here (the planner
// is always cost-based).
package plan

import (
	"fmt"
	"math"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

// whereSel is the flat selectivity applied per stage WHERE (a predicate
// keeps ~half the rows).
const whereSel = 0.5

// untypedFanout is the fan-out assumed for an untyped expand.
const untypedFanout = 3.0

// unwindFanout is the rows assumed per FOR over a list of unseen length.
const unwindFanout = 4.0

// Estimates holds per-segment estimates in branch-major order.
type Estimates struct {
	Segs []SegEst
}

// SegEst estimates one segment; mirrors the profiler's segment shape so
// the renderer can zip them.
type SegEst struct {
	Stages []StageEst
	// ProjRows is the estimated rows after projection; nil when aggregated
	// (group count not estimated).
	ProjRows *uint64
	// PostWhereRows is the estimated rows after a boundary WHERE, if any.
	PostWhereRows *uint64
	// AnchorNotes has one entry per stage: the [anchor: ...] note for a
	// stage that made a cost anchor choice, else "".
	AnchorNotes []string
}

// StageEst estimates one stage: Match holds one estimate per op plus a
// trailing one for the stage WHERE; Single is a one-output stage.
type StageEst struct {
	Match  []uint64
	Single *uint64
}

// rnd rounds a possibly-infinite float estimate to a row count, saturating.
func rnd(x float64) uint64 {
	if math.IsNaN(x) || x < 0 {
		return 0
	}
	if math.IsInf(x, 1) || x >= float64(math.MaxUint64) {
		return math.MaxUint64
	}
	return uint64(math.Round(x))
}

// Estimate computes every segment's estimates for a planned query.
func Estimate(p *Plan, g graph.Graph) *Estimates {
	out := &Estimates{}
	for _, segments := range p.Branches {
		for _, seg := range segments {
			out.Segs = append(out.Segs, segEst(seg, g))
		}
	}
	return out
}

func segEst(seg *Segment, g graph.Graph) SegEst {
	names := stageNames(seg)
	rows := 1.0 // one seed row enters the first stage
	stages := make([]StageEst, 0, len(seg.Stages))
	notes := make([]string, 0, len(seg.Stages))
	for _, stage := range seg.Stages {
		switch s := stage.(type) {
		case *MatchStage:
			ests, outRows := matchEst(s, rows, g)
			rows = outRows
			stages = append(stages, StageEst{Match: ests})
			notes = append(notes, anchorNote(s, names, g))
		case *UnwindStage:
			rows *= unwindFanout
			n := rnd(rows)
			stages = append(stages, StageEst{Single: &n})
			notes = append(notes, "")
		case *HashJoinStage:
			// Probe hits per row bounded by both the probe expansion's
			// fan-out and the built branch's size; the moved cross-branch
			// conjuncts filter like a stage WHERE.
			bRows := 1.0
			for _, bs := range s.Build {
				_, bRows = matchEst(bs, bRows, g)
			}
			rows *= math.Min(fanout(s.Probe.Types, s.Probe.Dir, g), math.Max(bRows, 1))
			if s.Where != nil {
				rows *= whereSel
			}
			n := rnd(rows)
			stages = append(stages, StageEst{Single: &n})
			notes = append(notes, "")
		default:
			// Shortest path, CALL, CALL subquery: no cardinality model --
			// carry the running estimate.
			n := rnd(rows)
			stages = append(stages, StageEst{Single: &n})
			notes = append(notes, "")
		}
	}
	se := SegEst{Stages: stages, AnchorNotes: notes}
	se.ProjRows = projEst(&seg.Proj, rows)
	if seg.PostWhere != nil && se.ProjRows != nil {
		n := rnd(float64(*se.ProjRows) * whereSel)
		se.PostWhereRows = &n
	}
	return se
}

// projEst is the estimated rows after a segment's projection; nil when
// aggregated, otherwise the row count clamped by LIMIT.
func projEst(proj *ProjPlan, rows float64) *uint64 {
	if proj.Aggregated {
		return nil
	}
	n := rows
	if proj.Limit != nil {
		n = math.Min(n, float64(*proj.Limit))
	}
	r := rnd(n)
	return &r
}

// matchEst estimates each operator of a MATCH stage, threading the running
// row count; returns the per-op estimates (plus a trailing one for the
// stage WHERE) and the rows leaving the stage.
func matchEst(ms *MatchStage, inRows float64, g graph.Graph) ([]uint64, float64) {
	return matchEstAnchored(ms, inRows, g, nil)
}

// matchEstAnchored is matchEst with cross-stage anchor resolution: an
// expand whose From slot was proven at plan time to hold exactly one
// concrete node prices that hop at the node's REAL degree instead of the
// type average -- fact over an immutable snapshot, immune to
// average-degree lies in either direction (a hub anchor on a sparse
// type, a sparse anchor on a hub-heavy type). Parameter seeks never
// resolve: a cached template plan must not embed one parameter value's
// degree.
func matchEstAnchored(ms *MatchStage, inRows float64, g graph.Graph, resolved map[int]graph.NodeID) ([]uint64, float64) {
	rows := inRows
	ests := make([]uint64, 0, len(ms.Ops)+1)
	// The anchor's first hop fans out by the anchor's RESOLVED local
	// degree (the hub-aware signal), not the global average; later hops
	// fall back to avg_degree -- except a hop from a resolved single-node
	// slot, which prices exactly.
	anchorDeg, anchorDegOK := anchorFirstHopDegree(ms, g)
	for i := range ms.Ops {
		var firstHop *float64
		if i == 1 && anchorDegOK {
			firstHop = &anchorDeg
		} else if resolved != nil {
			op := &ms.Ops[i]
			if op.Kind == OpExpand || op.Kind == OpVarExpand {
				if n, ok := resolved[op.From]; ok {
					d := resolvedDegree([]graph.NodeID{n}, true, op.Types, op.Dir, g)
					dv := float64(d.val)
					firstHop = &dv
				}
			}
		}
		rows = opEst(&ms.Ops[i], rows, firstHop, g)
		ests = append(ests, rnd(rows))
	}
	if ms.Where != nil {
		rows *= whereSel
		ests = append(ests, rnd(rows))
	}
	return ests, rows
}

// anchorFirstHopDegree is the anchor's resolved first-hop degree (exact,
// when the anchor is a concrete seek with a hop).
func anchorFirstHopDegree(ms *MatchStage, g graph.Graph) (float64, bool) {
	if len(ms.Ops) < 2 || ms.Ops[0].Kind != OpScan {
		return 0, false
	}
	scan := &ms.Ops[0]
	hop := &ms.Ops[1]
	if hop.Kind != OpExpand && hop.Kind != OpVarExpand {
		return 0, false
	}
	nodes, ok := resolveScanNodes(&scan.Source, scan.Labels, scan.Props, g)
	if !ok || len(nodes) == 0 {
		return 0, false
	}
	d := resolvedDegree(nodes, true, hop.Types, hop.Dir, g)
	return float64(d.val), true
}

// opEst applies one operator to the running row estimate; firstHopDeg
// overrides the hop's average fan-out with the anchor's resolved degree.
func opEst(op *BindOp, rows float64, firstHopDeg *float64, g graph.Graph) float64 {
	switch op.Kind {
	case OpScan:
		return rows * float64(scanCard(&op.Source, op.Props, g))
	case OpExpand:
		pop := targetPop(op.Labels, g)
		deg := fanout(op.Types, op.Dir, g)
		if firstHopDeg != nil {
			deg = *firstHopDeg
		}
		if op.Rebind {
			// Joining into a bound node: an existence check, output <=
			// input, scaled by the chance a matching rel exists.
			return rows * math.Min(deg/math.Max(pop, 1), 1)
		}
		return rows * deg
	default: // OpVarExpand
		d := fanout(op.Types, op.Dir, g)
		if firstHopDeg != nil {
			d = *firstHopDeg
		}
		pop := targetPop(op.Labels, g)
		var out float64
		if op.Max != nil {
			// Bounded trail: sum the per-hop path counts d^min .. d^max.
			mult := 0.0
			for h := op.Min; h <= *op.Max; h++ {
				mult += math.Pow(d, float64(h))
			}
			out = rows * mult
		} else {
			// Unbounded reachable set ~ the whole target population.
			out = pop
		}
		if op.DedupEndpoints || op.Max == nil {
			return math.Min(out, math.Max(pop, 1))
		}
		return out
	}
}

// scanCard is the exact leaf cardinality of a scan source (the same
// quantity the planner's anchorCard uses).
func scanCard(source *ScanSource, props []ast.PropEntry, g graph.Graph) uint64 {
	switch source.Kind {
	case ScanProperty:
		if isConcrete(source.Value) {
			return uint64(setLen(g.NodesWithProperty(source.Label, source.Key, semantics.LitValue(source.Value))))
		}
		return g.LabelCardinality(source.Label)
	case ScanLabel:
		// A label scan with extra concrete inline props narrows;
		// approximate that filter.
		base := float64(g.LabelCardinality(source.Label))
		return rnd(base * propSel(props))
	case ScanTextMatch:
		return g.LabelCardinality(source.Label)
	case ScanExistsSeed:
		// Estimate as the base label scan so join ordering is unchanged
		// by the seeding -- the narrowing is purely an execution-time
		// candidate source (like ScanTextMatch's contract).
		if source.Label != "" {
			return g.LabelCardinality(source.Label)
		}
		return uint64(g.NodeCount())
	case ScanNodeID, ScanNodeIDVar, ScanArg:
		return 1
	default: // ScanAll
		return uint64(g.NodeCount())
	}
}

// fanout is the combined average fan-out for a hop over types in dir.
func fanout(types []string, dir graph.Direction, g graph.Graph) float64 {
	if len(types) == 0 {
		return untypedFanout
	}
	total := 0.0
	for _, t := range types {
		total += g.AvgDegree(t, dir)
	}
	return total
}

// targetPop is the population of the target label (or all nodes).
func targetPop(labels []string, g graph.Graph) float64 {
	if len(labels) > 0 {
		return float64(g.LabelCardinality(labels[0]))
	}
	return float64(g.NodeCount())
}

// propSel is the selectivity of concrete inline props beyond the scan key.
func propSel(props []ast.PropEntry) float64 {
	concrete := 0
	for i := range props {
		if isConcrete(props[i].Val) {
			concrete++
		}
	}
	return math.Max(math.Pow(0.1, float64(concrete)), math.SmallestNonzeroFloat64)
}

func isConcrete(l ast.Literal) bool {
	return l.Kind != ast.LitParam && l.Kind != ast.LitNamedParam && l.Kind != ast.LitNull
}

// stageNames is the slot -> variable-name map for a segment.
func stageNames(seg *Segment) []string {
	names := make([]string, seg.RowWidth)
	for name, slot := range seg.Slots {
		if slot < len(names) {
			names[slot] = name
		}
	}
	return names
}

// anchorNote reconstructs the [anchor: ...] note for a MATCH stage whose
// anchor was a real choice between two endpoints: each end's exact leaf
// cardinality and resolved first-hop degree (the hub-aware signal), and
// the inferred decision rung. "" when there is no two-endpoint choice.
func anchorNote(ms *MatchStage, names []string, g graph.Graph) string {
	if len(ms.Ops) < 2 || ms.Ops[0].Kind != OpScan {
		return ""
	}
	scan := &ms.Ops[0]
	if scan.Source.Kind == ScanArg {
		return "" // anchor was forced (carried-in), not chosen
	}
	hop := &ms.Ops[1]
	if hop.Kind != OpExpand && hop.Kind != OpVarExpand {
		return ""
	}
	last := &ms.Ops[len(ms.Ops)-1]
	if last.Kind != OpExpand && last.Kind != OpVarExpand {
		return ""
	}

	c1 := scanCard(&scan.Source, scan.Props, g)
	n1, ok1 := resolveScanNodes(&scan.Source, scan.Labels, scan.Props, g)
	d1 := resolvedDegree(n1, ok1, hop.Types, hop.Dir, g)
	c2 := nodeCard(last.Labels, last.Props, g)
	n2, ok2 := resolveByProps(last.Labels, last.Props, g)
	d2 := resolvedDegree(n2, ok2, last.Types, revDir(last.Dir), g)

	chosen := endpointDesc(scan.Slot, names, scan.Labels, scan.Props)
	other := endpointDesc(last.To, names, last.Labels, last.Props)
	var reason string
	switch {
	case c1 < c2:
		reason = "smaller leaf cardinality"
	case c1 == c2 && d1.val < d2.val:
		reason = "smaller first-hop degree (hub-aware)"
	case c1 == c2 && d1.val == d2.val:
		reason = "avg-degree tie-break"
	default:
		reason = "selectivity tier"
	}
	return fmt.Sprintf("[anchor: %s card=%d, fan-out %s · vs %s card=%d, fan-out %s → %s]",
		chosen, c1, d1.render(), other, c2, d2.render(), reason)
}

// degree is a first-hop fan-out: exact when the endpoint resolved to
// concrete nodes, else an avg_degree estimate.
type degree struct {
	val   uint64
	exact bool
}

func (d degree) render() string {
	if d.exact {
		return GroupDigits(d.val)
	}
	return "~" + GroupDigits(d.val)
}

// resolvedDegree sums the resolved nodes' real degree over types/dir; with
// no resolvable node, falls back to the average degree of the hop type.
func resolvedDegree(nodes []graph.NodeID, ok bool, types []string, dir graph.Direction, g graph.Graph) degree {
	if !ok {
		return degree{val: rnd(fanout(types, dir, g))}
	}
	var total uint64
	for _, n := range nodes {
		total += countNeighbors(g, n, dir, types)
	}
	return degree{val: total, exact: true}
}

// resolveScanNodes pins a scan's concrete seek to node ids, for an exact
// degree read; ok=false for label-only/text/all sources.
func resolveScanNodes(source *ScanSource, labels []string, props []ast.PropEntry, g graph.Graph) ([]graph.NodeID, bool) {
	switch source.Kind {
	case ScanProperty:
		if isConcrete(source.Value) {
			return setSlice(g.NodesWithProperty(source.Label, source.Key, semantics.LitValue(source.Value))), true
		}
		return nil, false
	case ScanLabel:
		return resolveByProps(labels, props, g)
	}
	return nil, false
}

// resolveByProps resolves a node from its label plus a concrete inline
// property; ok=false when it has none.
func resolveByProps(labels []string, props []ast.PropEntry, g graph.Graph) ([]graph.NodeID, bool) {
	if len(labels) == 0 {
		return nil, false
	}
	for i := range props {
		if isConcrete(props[i].Val) {
			return setSlice(g.NodesWithProperty(labels[0], props[i].Key, semantics.LitValue(props[i].Val))), true
		}
	}
	return nil, false
}

// nodeCard is the leaf cardinality of a node given only labels + props
// (the rejected endpoint, which has no ScanSource).
func nodeCard(labels []string, props []ast.PropEntry, g graph.Graph) uint64 {
	if len(labels) == 0 {
		return uint64(g.NodeCount())
	}
	for i := range props {
		if isConcrete(props[i].Val) {
			return uint64(setLen(g.NodesWithProperty(labels[0], props[i].Key, semantics.LitValue(props[i].Val))))
		}
	}
	return g.LabelCardinality(labels[0])
}

// endpointDesc renders var:Label {k} for an anchor-note endpoint.
func endpointDesc(slot int, names []string, labels []string, props []ast.PropEntry) string {
	v := ""
	if slot >= 0 && slot < len(names) {
		v = names[slot]
	}
	label := ""
	if len(labels) > 0 {
		label = ":" + labels[0]
	}
	key := ""
	for i := range props {
		if isConcrete(props[i].Val) {
			key = " {" + props[i].Key + "}"
			break
		}
	}
	return v + label + key
}

func revDir(d graph.Direction) graph.Direction {
	switch d {
	case graph.Outgoing:
		return graph.Incoming
	case graph.Incoming:
		return graph.Outgoing
	}
	return graph.Both
}

// GroupDigits renders 2860664 as "2,860,664" (shared with the renderer).
func GroupDigits(n uint64) string {
	s := fmt.Sprintf("%d", n)
	out := make([]byte, 0, len(s)+len(s)/3)
	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	return string(out)
}
