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
	// early-exits per target).
	var pw *pathWeight
	if sp.Weight != nil {
		pw = buildPathWeight(ctx, sp)
	}
	memo := map[graph.NodeID]*spTree{}
	for _, row := range rows {
		var path nodesRels
		found := false
		if a, ok1 := row[sp.From].AsNode(); ok1 {
			if b, ok2 := row[sp.To].AsNode(); ok2 {
				switch {
				case pw != nil:
					if p := weightedShortestPath(ctx, a, b, sp, rm, hop, pw); p != nil {
						path, found = *p, true
					}
				case srcFreq[a] >= 2:
					tree, ok := memo[a]
					if !ok {
						tree = buildSPTree(ctx, a, sp, rm, hop, scr)
						memo[a] = tree
					}
					path, found = tree.pathTo(ctx, b, sp, rm, hop, scr)
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

// spScratch is a stage's reusable path-search state: the walk's visited
// set and double-buffered frontier, the single-path parent links, the
// all-shortest distance map, and the batch neighbor buffers. One row's
// search allocates only its result path once the maps and buffers warm
// up. Retained structures (the spTree memo's parent maps, result node
// chains) always allocate fresh -- scratch never escapes a call.
type spScratch struct {
	visited        map[graph.NodeID]struct{}
	frontier, next []graph.NodeID
	parent         map[graph.NodeID]graph.NodeID
	dist           map[graph.NodeID]uint64
	nbNodes        []graph.NodeID
	nbPoss         []uint32
}

func newSPScratch() *spScratch {
	return &spScratch{
		visited: map[graph.NodeID]struct{}{},
		parent:  map[graph.NodeID]graph.NodeID{},
		dist:    map[graph.NodeID]uint64{},
	}
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
	clear(scr.visited)
	scr.visited[a] = struct{}{}
	frontier := append(scr.frontier[:0], a)
	next := scr.next[:0]
	defer func() { scr.frontier, scr.next = frontier, next }()
	for depth := uint64(0); len(frontier) > 0 && depth < spCap(sp); depth++ {
		next = next[:0]
		for _, u := range frontier {
			for _, v := range appendHopNeighbors(ctx, scr, u, sp.Dir, rm, hop) {
				if _, seen := scr.visited[v]; seen {
					continue
				}
				scr.visited[v] = struct{}{}
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
func pathFromParents(parent map[graph.NodeID]graph.NodeID, a, b graph.NodeID) []graph.NodeID {
	if a == b {
		return []graph.NodeID{a}
	}
	if _, ok := parent[b]; !ok {
		return nil
	}
	n := 2
	for cur := parent[b]; cur != a; cur = parent[cur] {
		n++
	}
	nodes := make([]graph.NodeID, n)
	cur := b
	for i := n - 1; i >= 0; i-- {
		nodes[i] = cur
		cur = parent[cur]
	}
	return nodes
}

// shortestPath is the early-exit minimum-hop search a..b: the first time b
// is reached is at its minimum hop distance. Returned by value -- one
// found path per row, no per-path box.
func shortestPath(ctx *eval.Ctx, a, b graph.NodeID, sp *plan.SpStage, rm *graph.RelMatcher, hop *hopFilter, scr *spScratch) (nodesRels, bool) {
	clear(scr.parent)
	if a != b {
		spWalk(ctx, a, sp, rm, hop, scr, func(v, u graph.NodeID, _ uint64) bool {
			scr.parent[v] = u
			return v == b
		}, nil)
	}
	nodes := pathFromParents(scr.parent, a, b)
	if nodes == nil {
		return nodesRels{}, false
	}
	return nodesRels{nodes: nodes, rels: pathRelPositions(ctx, scr, nodes, sp.Dir, rm, hop)}, true
}

// spTree is a single-source minimum-hop parent tree, reused across every
// row sharing that source.
type spTree struct {
	source graph.NodeID
	parent map[graph.NodeID]graph.NodeID
}

// buildSPTree runs one bounded BFS from a, honoring the same filters the
// per-row search uses so reachability and distances are identical. The
// parent map is retained in the stage memo, so it allocates fresh.
func buildSPTree(ctx *eval.Ctx, a graph.NodeID, sp *plan.SpStage, rm *graph.RelMatcher, hop *hopFilter, scr *spScratch) *spTree {
	parent := map[graph.NodeID]graph.NodeID{}
	spWalk(ctx, a, sp, rm, hop, scr, func(v, u graph.NodeID, _ uint64) bool {
		parent[v] = u
		return false
	}, nil)
	return &spTree{source: a, parent: parent}
}

// pathTo reads the minimum-hop path source..b off the parent links,
// returned by value.
func (t *spTree) pathTo(ctx *eval.Ctx, b graph.NodeID, sp *plan.SpStage, rm *graph.RelMatcher, hop *hopFilter, scr *spScratch) (nodesRels, bool) {
	nodes := pathFromParents(t.parent, t.source, b)
	if nodes == nil {
		return nodesRels{}, false
	}
	return nodesRels{nodes: nodes, rels: pathRelPositions(ctx, scr, nodes, sp.Dir, rm, hop)}, true
}

// allShortestPaths enumerates every minimum-hop path a..b: a forward BFS
// records each reached node's minimum distance, then a backward DFS from b
// walks predecessor chains strictly descending the distance -- each
// completed chain is a distinct minimum-hop path.
func allShortestPaths(ctx *eval.Ctx, a, b graph.NodeID, sp *plan.SpStage, rm *graph.RelMatcher, hop *hopFilter, scr *spScratch) []nodesRels {
	if a == b {
		return []nodesRels{{nodes: []graph.NodeID{a}}}
	}
	clear(scr.dist)
	dist := scr.dist
	dist[a] = 0
	spWalk(ctx, a, sp, rm, hop, scr, func(v, _ graph.NodeID, d uint64) bool {
		dist[v] = d
		return false
	}, func() bool {
		_, reached := dist[b]
		return reached
	})
	if _, reached := dist[b]; !reached {
		return nil
	}
	rdir := flipDir(sp.Dir)
	var chains [][]graph.NodeID
	suffix := []graph.NodeID{b}
	enumeratePaths(ctx, a, dist, rdir, rm, hop, &suffix, &chains)
	out := make([]nodesRels, len(chains))
	for i, nodes := range chains {
		out[i] = nodesRels{nodes: nodes, rels: pathRelPositions(ctx, scr, nodes, sp.Dir, rm, hop)}
	}
	return out
}

// enumeratePaths extends the reversed suffix [b..v] by each predecessor u
// with dist[u] == dist[v]-1; reaching a completes one path.
func enumeratePaths(ctx *eval.Ctx, a graph.NodeID, dist map[graph.NodeID]uint64, rdir graph.Direction, rm *graph.RelMatcher, hop *hopFilter, suffix *[]graph.NodeID, out *[][]graph.NodeID) {
	if len(*out) >= maxAllShortestPaths {
		return
	}
	v := (*suffix)[len(*suffix)-1]
	if v == a {
		path := make([]graph.NodeID, len(*suffix))
		copy(path, *suffix)
		reverseNodes(path)
		*out = append(*out, path)
		return
	}
	want := dist[v] - 1
	filteredNeighbors(ctx, v, rdir, rm, hop, func(u graph.NodeID) {
		if len(*out) >= maxAllShortestPaths {
			return
		}
		if d, ok := dist[u]; ok && d == want {
			*suffix = append(*suffix, u)
			enumeratePaths(ctx, a, dist, rdir, rm, hop, suffix, out)
			*suffix = (*suffix)[:len(*suffix)-1]
		}
	})
}

// pathRelPositions resolves each consecutive node pair's relationship
// position (the first accepted relationship between them), reading each
// hop's candidates through the batch seam into the scratch buffers.
func pathRelPositions(ctx *eval.Ctx, scr *spScratch, nodes []graph.NodeID, dir graph.Direction, rm *graph.RelMatcher, hop *hopFilter) []uint32 {
	rels := make([]uint32, 0, max(len(nodes)-1, 0))
	for i := 0; i+1 < len(nodes); i++ {
		scr.nbNodes, scr.nbPoss = ctx.G.AppendRelationshipsMatched(scr.nbNodes[:0], scr.nbPoss[:0], nodes[i], dir, rm)
		for j, nb := range scr.nbNodes {
			if p := scr.nbPoss[j]; nb == nodes[i+1] && (hop == nil || hop.keep(ctx, p)) {
				rels = append(rels, p)
				break
			}
		}
	}
	return rels
}

// reverseNodes reverses a node sequence in place.
func reverseNodes(nodes []graph.NodeID) {
	for i, j := 0, len(nodes)-1; i < j; i, j = i+1, j-1 {
		nodes[i], nodes[j] = nodes[j], nodes[i]
	}
}
