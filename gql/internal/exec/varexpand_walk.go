// Bounded trail enumeration: the depth-first walk that emits one endpoint
// per qualifying trail of length min..=max, enforcing relationship
// uniqueness (and ACYCLIC node uniqueness), carried-state pruning, and
// optional per-trail rel/pair collection. Split from varexpand.go, which
// holds the candidate generation and reachability (BFS) paths.
package exec

import (
	"slices"

	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// varWalk is the bounded trail enumeration's state: emitted endpoints (one
// per qualifying trail), the named-rel arena, the relationship-uniqueness
// edge stack, and per-depth candidate buffers reused across siblings.
type varWalk struct {
	ctx         *eval.Ctx
	op          *plan.BindOp
	m           *graph.NodeMatcher
	rm          *graph.RelMatcher
	gate        hopGate
	bound       graph.NodeID
	haveBound   bool
	collectRels bool
	max         uint64
	// MATCH-scope uniqueness: a checking op skips edges whose canonical
	// pair is on the scope's used stack; a contributing op collects each
	// trail's pairs (collectPairs) for the DFS to push while bound.
	uniq         *uniqEnv
	collectPairs bool

	pathRels  []uint32
	pathPairs [][2]graph.NodeID
	used      [][2]graph.NodeID
	// visited is the ACYCLIC mode's node stack (seeded with the start
	// node); empty means trail semantics (rel uniqueness only).
	visited []graph.NodeID
	scratch [][]nodePos

	cand       *[]graph.NodeID
	relData    *[]uint32
	relRanges  *[][2]int
	pairData   *[][2]graph.NodeID
	pairRanges *[][2]int

	// nbufN/nbufP are the per-hop batch buffers (parallel neighbor/pos),
	// reused across dfs levels: each level copies them into its own
	// scratch before recursing.
	nbufN []graph.NodeID
	nbufP []uint32
}

type nodePos struct {
	nb  graph.NodeID
	pos uint32
}

// dfs enumerates trails of length min..=max with no repeated relationship,
// emitting each qualifying endpoint (one entry per trail, matching the
// per-path semantics). st is the gate carry's per-path state, threaded
// down the recursion.
func (w *varWalk) dfs(cur graph.NodeID, depth uint64, st carryState) {
	if depth >= w.max {
		return
	}
	d := int(depth)
	for len(w.scratch) <= d {
		w.scratch = append(w.scratch, nil)
	}
	buf := w.scratch[d][:0]
	// Walk relationships (carrying positions) when the gate prunes by
	// position or the rel list is recorded; otherwise the leaner neighbor
	// batch. Batch appends replace the iter.Seq forms, which heap-allocate
	// their closures per call.
	if w.gate.pred != nil || w.gate.carry != nil || w.collectRels {
		w.nbufN, w.nbufP = w.ctx.G.AppendRelationshipsMatched(w.nbufN[:0], w.nbufP[:0], cur, w.op.Dir, w.rm)
		for i, nb := range w.nbufN {
			if p := w.nbufP[i]; w.gate.pred == nil || w.gate.pred.keep(w.ctx, p) {
				buf = append(buf, nodePos{nb, p})
			}
		}
	} else {
		w.nbufN = w.ctx.G.AppendNeighborsMatched(w.nbufN[:0], cur, w.op.Dir, w.rm)
		for _, nb := range w.nbufN {
			buf = append(buf, nodePos{nb, 0})
		}
	}
	w.scratch[d] = buf
	for i := 0; i < len(w.scratch[d]); i++ {
		nb, pos := w.scratch[d][i].nb, w.scratch[d][i].pos
		// Relationship-uniqueness: undirected edges are unordered.
		edge := [2]graph.NodeID{cur, nb}
		if w.op.Dir == graph.Both && nb < cur {
			edge = [2]graph.NodeID{nb, cur}
		}
		if containsEdge(w.used, edge) {
			continue
		}
		// MATCH-scope uniqueness: a checking op also skips edges whose
		// canonical pair an earlier op in the clause already bound.
		var pair [2]graph.NodeID
		if w.uniq != nil && w.op.Uniq != nil {
			pair[0], pair[1] = uniqPair(w.op.Dir, cur, nb)
			if w.op.Uniq.Check && w.uniq.used(w.op.Uniq.Scope, pair[0], pair[1]) {
				continue
			}
		}
		// ACYCLIC additionally rejects a hop that revisits any node on
		// this path (the start included).
		if w.op.Acyclic && containsNode(w.visited, nb) {
			continue
		}
		// Carried-state pruning: a hop the carry rejects (e.g. its key does
		// not continue the monotonic order) admits no qualifying trail
		// through it. A path's first hop starts the state, never fails it.
		next := st
		if w.gate.carry != nil {
			var ok bool
			if next, ok = w.gate.carry.step(w.ctx, pos, st); !ok {
				continue
			}
		}
		w.used = append(w.used, edge)
		if w.op.Acyclic {
			w.visited = append(w.visited, nb)
		}
		if w.collectRels {
			w.pathRels = append(w.pathRels, pos)
		}
		if w.collectPairs {
			w.pathPairs = append(w.pathPairs, pair)
		}
		nd := depth + 1
		if nd >= w.op.Min && (!w.haveBound || w.bound == nb) && w.ctx.G.NodeMatcherAccepts(w.m, nb) {
			*w.cand = append(*w.cand, nb)
			if w.collectRels {
				*w.relRanges = append(*w.relRanges, [2]int{len(*w.relData), len(w.pathRels)})
				*w.relData = append(*w.relData, w.pathRels...)
			}
			if w.collectPairs {
				*w.pairRanges = append(*w.pairRanges, [2]int{len(*w.pairData), len(w.pathPairs)})
				*w.pairData = append(*w.pairData, w.pathPairs...)
			}
		}
		w.dfs(nb, nd, next)
		if w.collectRels {
			w.pathRels = w.pathRels[:len(w.pathRels)-1]
		}
		if w.collectPairs {
			w.pathPairs = w.pathPairs[:len(w.pathPairs)-1]
		}
		if w.op.Acyclic {
			w.visited = w.visited[:len(w.visited)-1]
		}
		w.used = w.used[:len(w.used)-1]
	}
}

// containsNode is the acyclic stack's membership test -- same short
// linear scan rationale as containsEdge.
func containsNode(visited []graph.NodeID, n graph.NodeID) bool {
	return slices.Contains(visited, n)
}

// containsEdge is the trail stack's membership test -- a short linear scan
// (never longer than max, e.g. 3 for {1,3}) beats a hashing set.
func containsEdge(used [][2]graph.NodeID, e [2]graph.NodeID) bool {
	return slices.Contains(used, e)
}
