// The weighted shortest-path form of an SpStage (engine-reachable; no GQL
// surface yet): a Dijkstra over (cost, hops, node) states keyed on
// (node, hops) -- the hop cap makes cost non-monotonic per node -- with
// the path reconstructed from parent links so relationships(p) scores the
// exact path the search optimized.
package exec

import (
	"container/heap"
	"math"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// pathWeightKind discriminates a pathWeight.
type pathWeightKind uint8

const (
	weightConstant pathWeightKind = iota
	weightReader
	weightMissing
	weightExpr
)

// pathWeight is the per-edge weight for a weighted search, compiled once
// per stage. An invalid constant (non-finite or negative) degrades to
// Missing -- unit weights -- mirroring the Rust build; a per-edge value
// that is non-finite, negative, or non-numeric excludes that edge.
type pathWeight struct {
	kind   pathWeightKind
	c      float64
	reader func(pos uint32) float64
	eval   RowEval
	scope  map[string]int
	// A formula weight can be an expensive correlated subquery; an edge's
	// weight is fixed by its position, so memoize per position.
	cache map[uint32]*float64
}

// buildPathWeight compiles an SpStage's CostSpec.
func buildPathWeight(ctx *eval.Ctx, sp *plan.SpStage) *pathWeight {
	spec := sp.Weight
	switch spec.Kind {
	case ast.CostConstant:
		if valid(spec.Const) {
			return &pathWeight{kind: weightConstant, c: spec.Const}
		}
		return &pathWeight{kind: weightMissing}
	case ast.CostProperty:
		return &pathWeight{kind: weightReader, reader: ctx.G.RelWeightReader(spec.Prop)}
	default:
		scope := map[string]int{sp.WeightVar: 0}
		return &pathWeight{
			kind:  weightExpr,
			eval:  compileEval(ctx, spec.Expr, scope),
			scope: scope,
			cache: map[uint32]*float64{},
		}
	}
}

// at is the edge's weight at a CSR position; ok=false excludes the edge.
func (w *pathWeight) at(ctx *eval.Ctx, pos uint32) (float64, bool) {
	var v float64
	switch w.kind {
	case weightConstant:
		v = w.c
	case weightReader:
		v = w.reader(pos)
	case weightMissing:
		v = 1.0
	default:
		if cached, ok := w.cache[pos]; ok {
			if cached == nil {
				return 0, false
			}
			return *cached, true
		}
		f, ok := w.eval.Eval(ctx, []value.Value{value.Rel(pos)}, w.scope).AsFloat()
		if !ok {
			f = math.NaN()
		}
		if !valid(f) {
			w.cache[pos] = nil
			return 0, false
		}
		w.cache[pos] = &f
		return f, true
	}
	if !valid(v) {
		return 0, false
	}
	return v, true
}

func valid(w float64) bool {
	return !math.IsInf(w, 0) && !math.IsNaN(w) && w >= 0
}

// wpState is one Dijkstra frontier entry.
type wpState struct {
	cost float64
	node graph.NodeID
	hops uint64
}

// wpHeap is a min-heap on (cost, hops, node) -- ties break on fewer hops,
// then the smaller node id, matching the Rust ordering exactly.
type wpHeap []wpState

func (h wpHeap) Len() int { return len(h) }
func (h wpHeap) Less(i, j int) bool {
	if h[i].cost != h[j].cost {
		return h[i].cost < h[j].cost
	}
	if h[i].hops != h[j].hops {
		return h[i].hops < h[j].hops
	}
	return h[i].node < h[j].node
}
func (h wpHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *wpHeap) Push(x any)   { *h = append(*h, x.(wpState)) }
func (h *wpHeap) Pop() any     { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }

// nodeHops keys the distance/parent maps.
type nodeHops struct {
	node graph.NodeID
	hops uint64
}

type wpParent struct {
	prev graph.NodeID
	hops uint64
	rel  uint32
}

// weightedShortestPath is the min-cost path a..b honoring the hop cap
// (default: the node count) and the per-hop predicate.
func weightedShortestPath(ctx *eval.Ctx, a, b graph.NodeID, sp *plan.SpStage, rm *graph.RelMatcher, hop *hopFilter, w *pathWeight) *nodesRels {
	if a == b {
		return &nodesRels{nodes: []graph.NodeID{a}}
	}
	cap := uint64(ctx.G.NodeCount())
	if sp.Max != nil {
		cap = *sp.Max
	}
	dist := map[nodeHops]float64{{a, 0}: 0}
	parent := map[nodeHops]wpParent{}
	h := &wpHeap{{cost: 0, node: a, hops: 0}}
	for h.Len() > 0 {
		st := heap.Pop(h).(wpState)
		key := nodeHops{st.node, st.hops}
		if d, ok := dist[key]; ok && st.cost > d {
			continue
		}
		if st.node == b {
			var nodes []graph.NodeID
			var rels []uint32
			nodes = append(nodes, st.node)
			cur := key
			for cur.node != a || cur.hops != 0 {
				p := parent[cur]
				rels = append(rels, p.rel)
				nodes = append(nodes, p.prev)
				cur = nodeHops{p.prev, p.hops}
			}
			reverseNodes(nodes)
			for i, j := 0, len(rels)-1; i < j; i, j = i+1, j-1 {
				rels[i], rels[j] = rels[j], rels[i]
			}
			return &nodesRels{nodes: nodes, rels: rels}
		}
		if st.hops >= cap {
			continue
		}
		for nb, pos := range ctx.G.RelationshipsMatched(st.node, sp.Dir, rm) {
			if hop != nil && !hop.keep(ctx, pos) {
				continue
			}
			edge, ok := w.at(ctx, pos)
			if !ok {
				continue
			}
			next := nodeHops{nb, st.hops + 1}
			nextCost := st.cost + edge
			if d, seen := dist[next]; !seen || nextCost < d {
				dist[next] = nextCost
				parent[next] = wpParent{prev: st.node, hops: st.hops, rel: pos}
				heap.Push(h, wpState{cost: nextCost, node: nb, hops: st.hops + 1})
			}
		}
	}
	return nil
}
