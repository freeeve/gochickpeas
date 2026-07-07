// CALL procedure kernels: dispatch to the engine's analytics and index
// kernels through the Native capability (the per-row crossing sink lives
// in stream.go). Per-node procedures cross each input row with one row per
// node id (NodeSlot = the node, ValueSlot = its scalar); the index-backed
// search procedures yield one row per hit node in ascending id order.
package exec

import (
	"math"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
	"github.com/freeeve/gochickpeas/nodeset"
)

// perNodeValues is the per-node scalar vector for a node-analytics
// procedure, indexed by node id; ok is false for the search procedures.
// Value kinds mirror the Rust engine exactly: components as Int, scores
// and distances-with-weights as Float, BFS hop distances as Int with
// MaxInt64 for unreachable nodes.
func perNodeValues(proc *plan.CallProc, g *chickpeas.Snapshot) ([]value.Value, bool) {
	switch proc.Kind {
	case plan.ProcWcc:
		return intValues(g.WCCVia(g.Match(proc.RelType), proc.Direction)), true
	case plan.ProcWccAll:
		return intValues(g.WCC()), true
	case plan.ProcBfs:
		dir := chickpeas.Both
		if proc.Directed {
			dir = chickpeas.Outgoing
		}
		dists := g.BFSDistances(proc.Source, dir, chickpeas.MatchAll(), chickpeas.NoMaxDepth)
		out := make([]value.Value, g.CSRIDSpace())
		for i := range out {
			if d, ok := dists[graph.NodeID(i)]; ok {
				out[i] = value.Int(int64(d))
			} else {
				// An unreachable node has no distance entry.
				out[i] = value.Int(math.MaxInt64)
			}
		}
		return out, true
	case plan.ProcPageRank:
		return floatValues(g.PageRank(proc.Directed, proc.Damping, int(proc.Iters))), true
	case plan.ProcCdlp:
		init := cdlpInit(g, proc.SeedProp)
		return intValues(g.CDLPSeeded(proc.Directed, int(proc.Iters), init)), true
	case plan.ProcLcc:
		return floatValues(g.LCC(proc.Directed)), true
	case plan.ProcSssp:
		weightKey := ""
		if proc.Weighted {
			weightKey = "weight"
		}
		return floatValues(g.SSSP(proc.Source, proc.Directed, weightKey)), true
	}
	return nil, false
}

func intValues(xs []uint32) []value.Value {
	out := make([]value.Value, len(xs))
	for i, x := range xs {
		out[i] = value.Int(int64(x))
	}
	return out
}

func floatValues(xs []float64) []value.Value {
	out := make([]value.Value, len(xs))
	for i, x := range xs {
		out[i] = value.Float(x)
	}
	return out
}

// cdlpInit seeds each node's initial CDLP label from its seedProp integer
// property (a missing value falls back to the dense id); no seed property
// (or a non-integer column) seeds with dense node ids.
func cdlpInit(g *chickpeas.Snapshot, seedProp string) []uint32 {
	n := g.CSRIDSpace()
	init := make([]uint32, n)
	var col chickpeas.I64Col
	haveCol := false
	if seedProp != "" {
		if c, ok := g.Col(seedProp); ok && c.Dtype() == chickpeas.DtypeI64 {
			col = c.I64()
			haveCol = true
		}
	}
	for i := uint32(0); i < n; i++ {
		init[i] = i
		if haveCol {
			if v, ok := col.Get(i); ok {
				init[i] = uint32(v)
			}
		}
	}
	return init
}

// callSearchHits is the node hit-set for an index-backed search procedure.
func callSearchHits(proc *plan.CallProc, g *chickpeas.Snapshot) *nodeset.Set {
	switch proc.Kind {
	case plan.ProcFtsSearch:
		return g.FullTextSearch(proc.Label, proc.Field, proc.Query)
	case plan.ProcGeoWithinRadius:
		return g.GeoWithinRadius(proc.Label, proc.LatField, proc.LonField, proc.Lat, proc.Lon, proc.Km)
	default: // plan.ProcGeoWithinBBox
		return g.GeoWithinBBox(proc.Label, proc.LatField, proc.LonField, proc.MinLat, proc.MinLon, proc.MaxLat, proc.MaxLon)
	}
}

// callResults computes a resolved procedure's result set in the shape its
// kind emits: propagation rows, a per-node scalar vector, or a search
// hit-set iteration.
func callResults(proc *plan.CallProc, g *chickpeas.Snapshot) ([]value.Value, func(func(graph.NodeID) bool), []chickpeas.PropagateResult) {
	if proc.Kind == plan.ProcPropagate {
		return nil, nil, callPropagateRows(proc, g)
	}
	if values, ok := perNodeValues(proc, g); ok {
		return values, nil, nil
	}
	if hits := callSearchHits(proc, g); hits != nil {
		return nil, hits.Iter(), nil
	}
	return nil, nil, nil
}

// callPropagateRows runs first-claim value propagation for a resolved
// algo.propagate call; one output row per reached node.
func callPropagateRows(proc *plan.CallProc, g *chickpeas.Snapshot) []chickpeas.PropagateResult {
	seeds := make([]chickpeas.PropagateSeed, len(proc.Seeds))
	for i, n := range proc.Seeds {
		seeds[i] = chickpeas.PropagateSeed{Node: n, Value: proc.SeedVals[i]}
	}
	return g.PropagateBFS(seeds, chickpeas.PropagateOpts{
		RelTypes:   proc.RelTypes,
		Direction:  proc.Direction,
		MaxDepth:   proc.MaxDepth,
		ValueProp:  proc.ValueProp,
		Desc:       proc.Desc,
		TruncLimit: proc.TruncLimit,
		MinValue:   proc.MinValue,
		FilterProp: proc.FilterProp,
		FilterMin:  proc.FilterMin,
		FilterMax:  proc.FilterMax,
	})
}
