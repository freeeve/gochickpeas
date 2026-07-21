// All-shortest-paths enumeration: a reusable single-source minimum-hop
// parent tree (spTree) for hop-minimal single paths, plus the forward-BFS
// + backward-DFS that enumerates every distinct minimum-hop path a..b and
// resolves each path's relationship positions. Split from shortest.go,
// which holds the stage runner, scratch, and single-path search.
package exec

import (
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

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
	nodes := pathFromParents(scr, t.parent, t.source, b)
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
		s := scr.nodeSlice(1)
		s[0] = a
		return []nodesRels{{nodes: s}}
	}
	// spWalk opens the generation; dist entries are valid under its stamp
	// (scr.cur) until the next search begins.
	spWalk(ctx, a, sp, rm, hop, scr, func(v, _ graph.NodeID, d uint64) bool {
		scr.dist[v] = uint32(d)
		return false
	}, func() bool {
		return scr.gen[b] == scr.cur
	})
	if scr.gen[b] != scr.cur {
		return nil
	}
	rdir := flipDir(sp.Dir)
	var chains [][]graph.NodeID
	suffix := []graph.NodeID{b}
	enumeratePaths(ctx, a, scr, rdir, rm, hop, &suffix, &chains)
	out := make([]nodesRels, len(chains))
	for i, nodes := range chains {
		out[i] = nodesRels{nodes: nodes, rels: pathRelPositions(ctx, scr, nodes, sp.Dir, rm, hop)}
	}
	return out
}

// enumeratePaths extends the reversed suffix [b..v] by each predecessor u
// whose stamped distance is dist[v]-1; reaching a completes one path.
func enumeratePaths(ctx *eval.Ctx, a graph.NodeID, scr *spScratch, rdir graph.Direction, rm *graph.RelMatcher, hop *hopFilter, suffix *[]graph.NodeID, out *[][]graph.NodeID) {
	if len(*out) >= maxAllShortestPaths {
		return
	}
	v := (*suffix)[len(*suffix)-1]
	if v == a {
		path := scr.nodeSlice(len(*suffix))
		copy(path, *suffix)
		reverseNodes(path)
		*out = append(*out, path)
		return
	}
	want := scr.dist[v] - 1
	filteredNeighbors(ctx, v, rdir, rm, hop, func(u graph.NodeID) {
		if len(*out) >= maxAllShortestPaths {
			return
		}
		if scr.gen[u] == scr.cur && scr.dist[u] == want {
			*suffix = append(*suffix, u)
			enumeratePaths(ctx, a, scr, rdir, rm, hop, suffix, out)
			*suffix = (*suffix)[:len(*suffix)-1]
		}
	})
}

// pathRelPositions resolves each consecutive node pair's relationship
// position (the first accepted relationship between them). Each hop reads
// from its lower-degree endpoint -- resolving from the higher-degree side
// pays a hub's whole adjacency, and a path found by never expanding a
// hub's ply must not re-pay the hub here. The read goes through the batch
// seam into the walk scratch's neighbor buffers (an iterator closure per
// hop dominated shortest-path stages' allocations); it runs only after a
// walk completes, so the buffers are free (same rule as
// appendHopNeighbors). Positions read identically from either side (the
// incoming seam maps to stored positions), so the side pick cannot change
// the resolved rel set.
func pathRelPositions(ctx *eval.Ctx, scr *spScratch, nodes []graph.NodeID, dir graph.Direction, rm *graph.RelMatcher, hop *hopFilter) []uint32 {
	rels := scr.relSlice(max(len(nodes)-1, 0))[:0]
	rdir := flipDir(dir)
	for i := 0; i+1 < len(nodes); i++ {
		x, y := nodes[i], nodes[i+1]
		from, fdir, want := x, dir, y
		if ctx.G.Degree(y, rdir) < ctx.G.Degree(x, dir) {
			from, fdir, want = y, rdir, x
		}
		scr.nbNodes, scr.nbPoss = ctx.G.AppendRelationshipsMatched(scr.nbNodes[:0], scr.nbPoss[:0], from, fdir, rm)
		for j, nb := range scr.nbNodes {
			if nb == want && (hop == nil || hop.keep(ctx, scr.nbPoss[j])) {
				rels = append(rels, scr.nbPoss[j])
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
