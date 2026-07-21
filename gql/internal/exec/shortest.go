// Path-search stages (ANY SHORTEST / ALL SHORTEST): minimum-hop BFS with
// parent links, a single-source tree memo for sources shared by many rows,
// and the all-shortest backward enumeration over the distance map. Every
// form runs on the one spWalk frontier core, so the hop-filter and
// depth-bound semantics live in a single place. The weighted form is the
// COST clause: MATCH p = ANY SHORTEST <pattern> COST <expr>.
package exec

import (
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// maxAllShortestPaths caps the minimum-hop paths ALL SHORTEST enumerates
// per endpoint pair -- a min-hop DAG can fan out combinatorially, so
// enumeration stops at this safety valve rather than blow up.
const maxAllShortestPaths = 1024

// runSPStage binds the path slot to the minimum-hop path(s) between the
// bound endpoint slots. The single form binds one path per input row; the
// ALL form is row-expanding. A required stage drops rows with no path; an
// optional one keeps a single row with the path null. The stage's rel
// matcher compiles once so the walks skip per-call type-name resolution.
func runSPStage(ctx *eval.Ctx, sp *plan.SpStage, rows [][]value.Value) [][]value.Value {
	var hop *hopFilter
	if sp.RelPred != nil {
		scope := map[string]int{sp.RelPred.Var: 0}
		hop = &hopFilter{eval: compileEval(ctx, sp.RelPred.Pred, scope), scope: scope}
	}
	rm := ctx.G.CompileRelMatcher(sp.Types)
	scr := newSPScratch()
	var out [][]value.Value

	if sp.All {
		for _, row := range rows {
			var paths []nodesRels
			if a, ok1 := row[sp.From].AsNode(); ok1 {
				if b, ok2 := row[sp.To].AsNode(); ok2 {
					paths = allShortestPaths(ctx, a, b, sp, rm, hop, scr)
				}
			}
			if len(paths) == 0 {
				if sp.Optional {
					row[sp.PathSlot] = value.Null()
					out = append(out, row)
				}
				continue
			}
			for _, p := range paths {
				r := make([]value.Value, len(row))
				copy(r, row)
				r[sp.PathSlot] = value.Path(p.nodes, p.rels)
				out = append(out, r)
			}
		}
		return out
	}

	// Single shortest path: a source shared by >=2 rows is traversed once
	// into a parent tree and each target's path is read off it; a
	// single-row source keeps the early-exit search.
	srcFreq := map[graph.NodeID]int{}
	for _, row := range rows {
		if a, ok1 := row[sp.From].AsNode(); ok1 {
			if _, ok2 := row[sp.To].AsNode(); ok2 {
				srcFreq[a]++
			}
		}
	}
	// The weighted form compiles its per-edge weight once for the stage
	// and runs the Dijkstra per row (no tree memo: the weighted search
	// early-exits per target). A constant weight (or one degraded to unit
	// weights) makes every path's cost proportional to its hop count, so
	// the minimum-cost path IS a minimum-hop path: those route to the BFS
	// forms below -- a constant-weight Dijkstra pays a heap push and pop
	// plus map probes per relationship to rediscover the ordering a BFS
	// frontier yields for free. Reachability, path length, and the hop cap
	// are identical; among equal-length paths ANY SHORTEST may bind any.
	var pw *pathWeight
	if sp.Weight != nil {
		if pw = buildPathWeight(ctx, sp); pw.kind == weightConstant || pw.kind == weightMissing {
			pw = nil
		}
	}
	memo := map[graph.NodeID]*spTree{}
	// paths memoizes the fully MATERIALIZED path per (source, target)
	// pair: a stage joined against many rows repeats few distinct pairs,
	// and re-reading the parent chain plus re-scanning each hop's
	// adjacency for its relationship position dominates such stages. A
	// present nil entry records "no path". Path values are immutable, so
	// rows share the backing arrays.
	type pairKey struct{ a, b graph.NodeID }
	paths := map[pairKey]*nodesRels{}
	var ws wpScratch
	for _, row := range rows {
		var path nodesRels
		found := false
		if a, ok1 := row[sp.From].AsNode(); ok1 {
			if b, ok2 := row[sp.To].AsNode(); ok2 {
				switch {
				case pw != nil:
					if p := weightedShortestPath(ctx, a, b, sp, rm, hop, pw, &ws); p != nil {
						path, found = *p, true
					}
				case srcFreq[a] >= 2:
					if p, seen := paths[pairKey{a, b}]; seen {
						if p != nil {
							path, found = *p, true
						}
					} else {
						tree, ok := memo[a]
						if !ok {
							tree = buildSPTree(ctx, a, sp, rm, hop, scr)
							memo[a] = tree
						}
						path, found = tree.pathTo(ctx, b, sp, rm, hop, scr)
						if found {
							cp := path
							paths[pairKey{a, b}] = &cp
						} else {
							paths[pairKey{a, b}] = nil
						}
					}
				default:
					path, found = shortestPath(ctx, a, b, sp, rm, hop, scr)
				}
			}
		}
		switch {
		case found:
			row[sp.PathSlot] = value.Path(path.nodes, path.rels)
			out = append(out, row)
		case sp.Optional:
			row[sp.PathSlot] = value.Null()
			out = append(out, row)
		}
	}
	return out
}

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
}

func newSPScratch() *spScratch { return &spScratch{} }

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

// spCap is the hop cap (max nil = unbounded).
func spCap(sp *plan.SpStage) uint64 {
	if sp.Max != nil {
		return *sp.Max
	}
	return 1<<63 - 1
}

// spWalk is the one bounded minimum-hop frontier walk behind every
// path-search form: level-synchronous expansion from a through
// filteredNeighbors under the hop cap, calling reach exactly once per
// first-reached node with its predecessor and depth. reach returns true to
// halt the whole walk immediately (the early-exit single-path search);
// levelDone (nil allowed) is consulted after each completed level -- the
// all-shortest walk must finish a level so every minimum-hop predecessor
// gets a distance before stopping.
func spWalk(ctx *eval.Ctx, a graph.NodeID, sp *plan.SpStage, rm *graph.RelMatcher, hop *hopFilter, scr *spScratch, reach func(v, u graph.NodeID, d uint64) bool, levelDone func() bool) {
	fs := scr.begin(int(ctx.G.IDSpace()))
	scr.gen[a], scr.dist[a] = fs, 0
	frontier := append(scr.frontier[:0], a)
	next := scr.next[:0]
	defer func() { scr.frontier, scr.next = frontier, next }()
	for depth := uint64(0); len(frontier) > 0 && depth < spCap(sp); depth++ {
		next = next[:0]
		for _, u := range frontier {
			for _, v := range appendHopNeighbors(ctx, scr, u, sp.Dir, rm, hop) {
				if scr.gen[v] == fs {
					continue
				}
				scr.gen[v] = fs
				if reach(v, u, depth+1) {
					return
				}
				next = append(next, v)
			}
		}
		if levelDone != nil && levelDone() {
			return
		}
		frontier, next = next, frontier
	}
}

// pathFromParents reads the minimum-hop node chain a..b off parent links:
// a one-node chain when a == b, nil when b was never reached. Two passes
// (count, then fill backward) give one exactly-sized allocation.
func pathFromParents(scr *spScratch, parent map[graph.NodeID]graph.NodeID, a, b graph.NodeID) []graph.NodeID {
	if a == b {
		s := scr.nodeSlice(1)
		s[0] = a
		return s
	}
	if _, ok := parent[b]; !ok {
		return nil
	}
	n := 2
	for cur := parent[b]; cur != a; cur = parent[cur] {
		n++
	}
	nodes := scr.nodeSlice(n)
	cur := b
	for i := n - 1; i >= 0; i-- {
		nodes[i] = cur
		cur = parent[cur]
	}
	return nodes
}

// shortestPath is the single-pair minimum-hop search a..b: a bidirectional
// BFS growing a frontier from each end (the backward half walking the
// reversed direction), always expanding the smaller frontier by one full
// level, stopping at the first node stamped by both halves. Cost is edges
// touched, not nodes visited: expanding the smaller frontier means a hub
// adjacent to one endpoint never has its ply expanded at all.
//
// Minimality: once the halves have fully explored depths df and db, every
// path of length <= df+db crosses a node both halves reached, so its
// second stamping would already have been a meet; with none seen, an
// undiscovered path is at least df+db+1 long. Expanding a ply can then
// only find meets totalling exactly df+db+1 -- every first-ply meet is
// optimal, so the search stops at the first one. The same bound makes the
// hop cap exact: the loop stops once df+db reaches the cap, and any meet
// found before that totals within it.
func shortestPath(ctx *eval.Ctx, a, b graph.NodeID, sp *plan.SpStage, rm *graph.RelMatcher, hop *hopFilter, scr *spScratch) (nodesRels, bool) {
	if a == b {
		return nodesRels{nodes: []graph.NodeID{a}}, true
	}
	fs := scr.begin(int(ctx.G.IDSpace()))
	bs := fs + 1
	scr.gen[a], scr.dist[a], scr.parent[a] = fs, 0, a
	scr.gen[b], scr.dist[b], scr.parent[b] = bs, 0, b
	fFront := append(scr.frontier[:0], a)
	fNext := scr.next[:0]
	bFront := append(scr.bFrontier[:0], b)
	bNext := scr.bNext[:0]
	defer func() {
		scr.frontier, scr.next = fFront, fNext
		scr.bFrontier, scr.bNext = bFront, bNext
	}()
	dirB := flipDir(sp.Dir)
	var fMeet, bMeet graph.NodeID
	found := false
	df, db := uint64(0), uint64(0)
	// The side to expand is chosen by frontier NODE COUNT. The degree-sum
	// alternative (pending-edge count) was measured deterministically
	// (sp_frontier_ab_test.go): on realistic moderate-skew graphs it saves
	// under 1% of edge touches while paying a Degree read per accepted
	// node -- pure overhead -- and only wins (42% fewer edges) on extreme
	// synthetic hubs. If a hub-dominated workload ever appears, the
	// degree-sum form can return behind a skew heuristic; the A/B harness
	// is the record of the trade.
	for len(fFront) > 0 && len(bFront) > 0 && df+db < spCap(sp) && !found {
		if len(fFront) <= len(bFront) {
			fNext = fNext[:0]
			for _, u := range fFront {
				for _, v := range appendHopNeighbors(ctx, scr, u, sp.Dir, rm, hop) {
					switch scr.gen[v] {
					case fs:
					case bs:
						fMeet, bMeet, found = u, v, true
					default:
						scr.gen[v], scr.parent[v], scr.dist[v] = fs, u, uint32(df+1)
						fNext = append(fNext, v)
					}
					if found {
						break
					}
				}
				if found {
					break
				}
			}
			fFront, fNext = fNext, fFront
			df++
		} else {
			bNext = bNext[:0]
			for _, u := range bFront {
				for _, v := range appendHopNeighbors(ctx, scr, u, dirB, rm, hop) {
					switch scr.gen[v] {
					case bs:
					case fs:
						fMeet, bMeet, found = v, u, true
					default:
						scr.gen[v], scr.parent[v], scr.dist[v] = bs, u, uint32(db+1)
						bNext = append(bNext, v)
					}
					if found {
						break
					}
				}
				if found {
					break
				}
			}
			bFront, bNext = bNext, bFront
			db++
		}
	}
	if !found {
		return nodesRels{}, false
	}
	nodes := stitchPath(scr, a, b, fMeet, bMeet)
	return nodesRels{nodes: nodes, rels: pathRelPositions(ctx, scr, nodes, sp.Dir, rm, hop)}, true
}

// stitchPath joins the forward parent chain a..fMeet and the backward
// chain bMeet..b into one node sequence, sized exactly. Each source is its
// own parent, terminating the chain walks.
func stitchPath(scr *spScratch, a, b, fMeet, bMeet graph.NodeID) []graph.NodeID {
	n := 1
	for cur := fMeet; cur != a; cur = scr.parent[cur] {
		n++
	}
	m := 1
	for cur := bMeet; cur != b; cur = scr.parent[cur] {
		m++
	}
	nodes := scr.nodeSlice(n + m)
	cur := fMeet
	for i := n - 1; i >= 0; i-- {
		nodes[i] = cur
		cur = scr.parent[cur]
	}
	cur = bMeet
	for i := range m {
		nodes[n+i] = cur
		cur = scr.parent[cur]
	}
	return nodes
}
