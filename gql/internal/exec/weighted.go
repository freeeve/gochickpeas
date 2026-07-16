// The weighted shortest-path form of an SpStage (the ANY SHORTEST ...
// COST clause): a Dijkstra over (cost, hops, node) states keyed on
// (node, hops) -- the hop cap makes cost non-monotonic per node -- with
// the path reconstructed from parent links so relationships(p) scores the
// exact path the search optimized.
package exec

import (
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
		// A key with no column reads 1.0 on every edge -- unit weights by
		// another name, so classify it as Missing and let the dispatch
		// take the BFS forms (tree memo included) instead of a unit-weight
		// Dijkstra through the property door.
		r := ctx.G.RelWeightReader(spec.Prop)
		if r == nil {
			return &pathWeight{kind: weightMissing}
		}
		return &pathWeight{kind: weightReader, reader: r}
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

func (h wpHeap) less(i, j int) bool {
	if h[i].cost != h[j].cost {
		return h[i].cost < h[j].cost
	}
	if h[i].hops != h[j].hops {
		return h[i].hops < h[j].hops
	}
	return h[i].node < h[j].node
}

// push and pop are hand-rolled sift operations: the container/heap form boxes
// every state into an interface value, allocating once per push -- and the
// weighted-shortest-path Dijkstra pushes once per explored edge, so on a large
// search (IC14, BI Q19) that boxing dominates the query's allocations.
func (h *wpHeap) push(s wpState) {
	*h = append(*h, s)
	a := *h
	for i := len(a) - 1; i > 0; {
		parent := (i - 1) / 2
		if !a.less(i, parent) {
			break
		}
		a[i], a[parent] = a[parent], a[i]
		i = parent
	}
}

func (h *wpHeap) pop() wpState {
	a := *h
	top := a[0]
	n := len(a) - 1
	a[0] = a[n]
	a = a[:n]
	*h = a
	for i := 0; ; {
		l := 2*i + 1
		if l >= n {
			break
		}
		small := l
		if r := l + 1; r < n && a.less(r, l) {
			small = r
		}
		if !a.less(small, i) {
			break
		}
		a[i], a[small] = a[small], a[i]
		i = small
	}
	return top
}

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

// wpScratch is the per-stage scratch a weighted-shortest-path stage reuses
// across its row loop's searches: the distance/parent maps are cleared per
// search (clear keeps the buckets, so a search allocates only when it
// outgrows every previous one), the heap keeps its backing, and the
// neighbor buffers feed AppendRelationshipsMatched instead of a per-visit
// iterator closure.
type wpScratch struct {
	dist   map[nodeHops]float64
	parent map[nodeHops]wpParent
	heap   wpHeap
	nbrs   []graph.NodeID
	poss   []uint32
}

// weightedShortestPath is the min-cost path a..b honoring the hop cap
// (default: the node count) and the per-hop predicate. A hop cap makes
// cost non-monotonic per node, so a bounded search keys its state on
// (node, hops); an unbounded one keys the node alone -- plain Dijkstra --
// so an unreachable target costs one component sweep instead of a
// (node, hops) state explosion.
// weightedSearches counts weightedShortestPath invocations -- the
// red-before/green-after oracle for the unit-weight dispatch: rows whose
// cost degrades to unit must take the BFS forms (tree memo included), so
// a stage of N such rows runs ZERO weighted searches.
var weightedSearches int

func weightedShortestPath(ctx *eval.Ctx, a, b graph.NodeID, sp *plan.SpStage, rm *graph.RelMatcher, hop *hopFilter, w *pathWeight, ws *wpScratch) *nodesRels {
	weightedSearches++
	if a == b {
		return &nodesRels{nodes: []graph.NodeID{a}}
	}
	unbounded := sp.Max == nil
	cap := uint64(ctx.G.NodeCount())
	if sp.Max != nil {
		cap = *sp.Max
	}
	key := func(n graph.NodeID, hops uint64) nodeHops {
		if unbounded {
			return nodeHops{n, 0}
		}
		return nodeHops{n, hops}
	}
	if ws.dist == nil {
		ws.dist = map[nodeHops]float64{}
		ws.parent = map[nodeHops]wpParent{}
	}
	clear(ws.dist)
	clear(ws.parent)
	dist, parent := ws.dist, ws.parent
	dist[nodeHops{a, 0}] = 0
	h := &ws.heap
	*h = append((*h)[:0], wpState{cost: 0, node: a, hops: 0})
	for len(*h) > 0 {
		st := h.pop()
		k := key(st.node, st.hops)
		if d, ok := dist[k]; ok && st.cost > d {
			continue
		}
		if st.node == b {
			var nodes []graph.NodeID
			var rels []uint32
			nodes = append(nodes, st.node)
			cur := k
			for cur.node != a || cur.hops != 0 {
				p := parent[cur]
				rels = append(rels, p.rel)
				nodes = append(nodes, p.prev)
				cur = key(p.prev, p.hops)
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
		ws.nbrs, ws.poss = ctx.G.AppendRelationshipsMatched(ws.nbrs[:0], ws.poss[:0], st.node, sp.Dir, rm)
		for i, nb := range ws.nbrs {
			pos := ws.poss[i]
			if hop != nil && !hop.keep(ctx, pos) {
				continue
			}
			edge, ok := w.at(ctx, pos)
			if !ok {
				continue
			}
			next := key(nb, st.hops+1)
			nextCost := st.cost + edge
			if d, seen := dist[next]; !seen || nextCost < d {
				dist[next] = nextCost
				parent[next] = wpParent{prev: st.node, hops: st.hops, rel: pos}
				h.push(wpState{cost: nextCost, node: nb, hops: st.hops + 1})
			}
		}
	}
	return nil
}
