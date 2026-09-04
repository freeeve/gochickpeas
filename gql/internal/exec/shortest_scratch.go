// The path-search scratch: the pooled, generation-stamped search state
// every shortest-path form shares (dense id-space arrays, frontiers,
// batch neighbor buffers, append-only result slabs) and the hop-filtered
// neighbor seams over it. Split from shortest.go, which holds the stage
// runners and the walks.
package exec

import (
	"sync"

	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
)

// nodesRels is one materialized path.
type nodesRels struct {
	nodes []graph.NodeID
	rels  []uint32
}

// spScratch is a stage's reusable path-search state: dense node-indexed
// search arrays with a generation stamp (a slot belongs to the current
// search iff gen[id] carries its stamp, so a run needs no O(n) clear and
// no hash per neighbor probe), the double-buffered frontiers of both
// search halves, and the batch neighbor buffers. Arrays size by IDSpace
// -- sparse CSR slots make NodeCount too small -- growing on first use
// per graph. Retained structures (the spTree memo's parent maps, result
// node chains) always allocate fresh; scratch never escapes a call.
type spScratch struct {
	gen              []uint32
	cur              uint32 // forward stamp; cur+1 is the backward stamp
	parent           []graph.NodeID
	dist             []uint32
	frontier, next   []graph.NodeID
	bFrontier, bNext []graph.NodeID
	nbNodes          []graph.NodeID
	nbPoss           []uint32
	// arNodes/arRels are append-only path slabs: a stage emits thousands
	// of short paths and one make per path dominated its allocations.
	// Handed-out slices are RETAINED by emitted rows, so slabs are never
	// reused or reset -- the per-stage-run scratch is simply abandoned,
	// amortizing allocation count to one per slab.
	arNodes []graph.NodeID
	arRels  []uint32
	// preds memoizes per-node predecessor sequences for one ALL SHORTEST
	// enumeration (cleared per endpoint pair); see predsOf.
	preds map[graph.NodeID][]graph.NodeID
}

// spScratchPool recycles search state across stage runs: the dense
// arrays are id-space sized (~36MB at SF1), so a fresh scratch per run
// dominated a path stage's allocated bytes. Reuse is safe by
// construction -- generation stamps invalidate stale slots (across
// graphs too: a foreign stamp never matches a new one, and wraparound
// clears), the frontier buffers are length-reset per walk, and the path
// slabs only ever append past handed-out regions.
var spScratchPool = sync.Pool{New: func() any { return &spScratch{} }}

func newSPScratch() *spScratch { return spScratchPool.Get().(*spScratch) }

// nodeSlice hands out a full-capacity n-slice from the node slab.
func (scr *spScratch) nodeSlice(n int) []graph.NodeID {
	if cap(scr.arNodes)-len(scr.arNodes) < n {
		scr.arNodes = make([]graph.NodeID, 0, max(4096, n))
	}
	off := len(scr.arNodes)
	scr.arNodes = scr.arNodes[:off+n]
	return scr.arNodes[off : off+n : off+n]
}

// relSlice hands out a full-capacity n-slice from the rel slab.
func (scr *spScratch) relSlice(n int) []uint32 {
	if cap(scr.arRels)-len(scr.arRels) < n {
		scr.arRels = make([]uint32, 0, max(4096, n))
	}
	off := len(scr.arRels)
	scr.arRels = scr.arRels[:off+n]
	return scr.arRels[off : off+n : off+n]
}

// begin sizes the dense arrays for the graph's id space and opens a new
// generation, returning its forward stamp (backward = stamp+1). Stamp
// wraparound clears the gen array once; slot zero never collides because
// stamps start at 2.
func (scr *spScratch) begin(n int) uint32 {
	if len(scr.gen) < n {
		gen := make([]uint32, n)
		copy(gen, scr.gen)
		scr.gen = gen
		parent := make([]graph.NodeID, n)
		copy(parent, scr.parent)
		scr.parent = parent
		dist := make([]uint32, n)
		copy(dist, scr.dist)
		scr.dist = dist
	}
	if scr.cur >= ^uint32(0)-2 {
		clear(scr.gen)
		scr.cur = 0
	}
	scr.cur += 2
	return scr.cur
}

// appendHopNeighbors fills scr.nbNodes with node's accepted hop neighbors
// through the batch seam (relationship positions are consulted only under
// a per-hop predicate), compacting in place -- no per-call iterator
// closures. The result is valid until the next scratch use; callers that
// nest (the all-shortest enumeration) must not use it.
func appendHopNeighbors(ctx *eval.Ctx, scr *spScratch, node graph.NodeID, dir graph.Direction, rm *graph.RelMatcher, hop *hopFilter) []graph.NodeID {
	if hop == nil {
		scr.nbNodes = ctx.G.AppendNeighborsMatched(scr.nbNodes[:0], node, dir, rm)
		return scr.nbNodes
	}
	scr.nbNodes, scr.nbPoss = ctx.G.AppendRelationshipsMatched(scr.nbNodes[:0], scr.nbPoss[:0], node, dir, rm)
	kept := scr.nbNodes[:0]
	for i, p := range scr.nbPoss {
		if hop.keep(ctx, p) {
			kept = append(kept, scr.nbNodes[i])
		}
	}
	scr.nbNodes = kept
	return kept
}

// filteredNeighbors is the iterator form of the hop's neighbor set, kept
// for the recursive all-shortest enumeration whose nesting cannot share
// the scratch buffers.
func filteredNeighbors(ctx *eval.Ctx, node graph.NodeID, dir graph.Direction, rm *graph.RelMatcher, hop *hopFilter, visit func(graph.NodeID)) {
	if hop == nil {
		for nb := range ctx.G.NeighborsMatched(node, dir, rm) {
			visit(nb)
		}
		return
	}
	for nb, p := range ctx.G.RelationshipsMatched(node, dir, rm) {
		if hop.keep(ctx, p) {
			visit(nb)
		}
	}
}
