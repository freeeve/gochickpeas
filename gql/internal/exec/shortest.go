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
	var out [][]value.Value

	if sp.All {
		for _, row := range rows {
			var paths []nodesRels
			if a, ok1 := row[sp.From].AsNode(); ok1 {
				if b, ok2 := row[sp.To].AsNode(); ok2 {
					paths = allShortestPaths(ctx, a, b, sp, rm, hop)
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
		var path *nodesRels
		if a, ok1 := row[sp.From].AsNode(); ok1 {
			if b, ok2 := row[sp.To].AsNode(); ok2 {
				switch {
				case pw != nil:
					path = weightedShortestPath(ctx, a, b, sp, rm, hop, pw)
				case srcFreq[a] >= 2:
					tree, ok := memo[a]
					if !ok {
						tree = buildSPTree(ctx, a, sp, rm, hop)
						memo[a] = tree
					}
					path = tree.pathTo(ctx, b, sp, rm, hop)
				default:
					path = shortestPath(ctx, a, b, sp, rm, hop)
				}
			}
		}
		switch {
		case path != nil:
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

// filteredNeighbors is the hop's neighbor set, honoring the per-hop
// predicate by reading relationship positions when one is present.
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
func spWalk(ctx *eval.Ctx, a graph.NodeID, sp *plan.SpStage, rm *graph.RelMatcher, hop *hopFilter, reach func(v, u graph.NodeID, d uint64) bool, levelDone func() bool) {
	visited := map[graph.NodeID]struct{}{a: {}}
	frontier := []graph.NodeID{a}
	halted := false
	for depth := uint64(0); len(frontier) > 0 && depth < spCap(sp); depth++ {
		var next []graph.NodeID
		for _, u := range frontier {
			filteredNeighbors(ctx, u, sp.Dir, rm, hop, func(v graph.NodeID) {
				if halted {
					return
				}
				if _, seen := visited[v]; seen {
					return
				}
				visited[v] = struct{}{}
				if reach(v, u, depth+1) {
					halted = true
					return
				}
				next = append(next, v)
			})
			if halted {
				return
			}
		}
		if levelDone != nil && levelDone() {
			return
		}
		frontier = next
	}
}

// pathFromParents reads the minimum-hop node chain a..b off parent links:
// a one-node chain when a == b, nil when b was never reached.
func pathFromParents(parent map[graph.NodeID]graph.NodeID, a, b graph.NodeID) []graph.NodeID {
	if a == b {
		return []graph.NodeID{a}
	}
	if _, ok := parent[b]; !ok {
		return nil
	}
	nodes := []graph.NodeID{b}
	for cur := b; cur != a; {
		cur = parent[cur]
		nodes = append(nodes, cur)
	}
	reverseNodes(nodes)
	return nodes
}

// shortestPath is the early-exit minimum-hop search a..b: the first time b
// is reached is at its minimum hop distance.
func shortestPath(ctx *eval.Ctx, a, b graph.NodeID, sp *plan.SpStage, rm *graph.RelMatcher, hop *hopFilter) *nodesRels {
	parent := map[graph.NodeID]graph.NodeID{}
	if a != b {
		spWalk(ctx, a, sp, rm, hop, func(v, u graph.NodeID, _ uint64) bool {
			parent[v] = u
			return v == b
		}, nil)
	}
	nodes := pathFromParents(parent, a, b)
	if nodes == nil {
		return nil
	}
	return &nodesRels{nodes: nodes, rels: pathRelPositions(ctx, nodes, sp.Dir, rm, hop)}
}

// spTree is a single-source minimum-hop parent tree, reused across every
// row sharing that source.
type spTree struct {
	source graph.NodeID
	parent map[graph.NodeID]graph.NodeID
}

// buildSPTree runs one bounded BFS from a, honoring the same filters the
// per-row search uses so reachability and distances are identical.
func buildSPTree(ctx *eval.Ctx, a graph.NodeID, sp *plan.SpStage, rm *graph.RelMatcher, hop *hopFilter) *spTree {
	parent := map[graph.NodeID]graph.NodeID{}
	spWalk(ctx, a, sp, rm, hop, func(v, u graph.NodeID, _ uint64) bool {
		parent[v] = u
		return false
	}, nil)
	return &spTree{source: a, parent: parent}
}

// pathTo reads the minimum-hop path source..b off the parent links.
func (t *spTree) pathTo(ctx *eval.Ctx, b graph.NodeID, sp *plan.SpStage, rm *graph.RelMatcher, hop *hopFilter) *nodesRels {
	nodes := pathFromParents(t.parent, t.source, b)
	if nodes == nil {
		return nil
	}
	return &nodesRels{nodes: nodes, rels: pathRelPositions(ctx, nodes, sp.Dir, rm, hop)}
}

// allShortestPaths enumerates every minimum-hop path a..b: a forward BFS
// records each reached node's minimum distance, then a backward DFS from b
// walks predecessor chains strictly descending the distance -- each
// completed chain is a distinct minimum-hop path.
func allShortestPaths(ctx *eval.Ctx, a, b graph.NodeID, sp *plan.SpStage, rm *graph.RelMatcher, hop *hopFilter) []nodesRels {
	if a == b {
		return []nodesRels{{nodes: []graph.NodeID{a}}}
	}
	dist := map[graph.NodeID]uint64{a: 0}
	spWalk(ctx, a, sp, rm, hop, func(v, _ graph.NodeID, d uint64) bool {
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
		out[i] = nodesRels{nodes: nodes, rels: pathRelPositions(ctx, nodes, sp.Dir, rm, hop)}
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
// position (the first accepted relationship between them).
func pathRelPositions(ctx *eval.Ctx, nodes []graph.NodeID, dir graph.Direction, rm *graph.RelMatcher, hop *hopFilter) []uint32 {
	rels := make([]uint32, 0, max(len(nodes)-1, 0))
	for i := 0; i+1 < len(nodes); i++ {
		for nb, p := range ctx.G.RelationshipsMatched(nodes[i], dir, rm) {
			if nb == nodes[i+1] && (hop == nil || hop.keep(ctx, p)) {
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
