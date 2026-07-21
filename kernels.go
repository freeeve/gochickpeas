// Neighborhood aggregation and co-occurrence kernels: forest roots of a
// functional rel chain (RootsVia/RootVia/NeighborVia), one-mode network
// folding (FoldVia), seeded co-occurrence (CoOccurring), and neighbor /
// common-neighbor histograms (NeighborCounts, CommonNeighbors,
// CommonNeighborCounts). NeighborGroups lives in neighborgroups.go.

package chickpeas

import (
	"github.com/freeeve/gochickpeas/internal/parallel"
)

// RootsVia is a built forest-root array: index it by node id for the
// terminal of that node's functional-relationship chain. Shared and
// immutable -- hot loops hold it and index lock-free.
type RootsVia []NodeID

// NoNeighbor is the sentinel a NeighborVia array holds for a node with no
// such neighbor.
const NoNeighbor = NodeID(^uint32(0))

type rootsKey struct {
	dir Direction
	t   RelType
}

// RootsVia is the per-node forest-root array for the functional relType
// chain in dir: roots[node] is the node reached by following the single
// relType rel until one with no such rel (a terminal node maps to itself).
// Built once per (direction, type) with path compression, then cached --
// call once and index the slice, rather than RootVia per node. Intended
// for a rel that is functional in dir; malformed data (multiple such rels,
// or a cycle) follows the first neighbor in CSR order and is broken by a
// depth cap, resolving deterministically.
func (g *Snapshot) RootsVia(t RelType, dir Direction) RootsVia {
	key := rootsKey{dir: dir, t: t}
	g.rootsMu.Lock()
	cached, ok := g.rootsViaIndex[key]
	g.rootsMu.Unlock()
	if ok {
		return cached
	}
	// Build outside the lock; a racing build is discarded (identical).
	built := g.buildRootsVia(dir, t)
	g.rootsMu.Lock()
	defer g.rootsMu.Unlock()
	if existing, ok := g.rootsViaIndex[key]; ok {
		return existing
	}
	g.rootsViaIndex[key] = built
	return built
}

// RootVia is the terminal of node's functional relType chain in dir (a
// terminal node maps to itself) -- a convenience over RootsVia; in a hot
// loop index the array instead.
func (g *Snapshot) RootVia(node NodeID, t RelType, dir Direction) NodeID {
	roots := g.RootsVia(t, dir)
	if int(node) < len(roots) {
		return roots[node]
	}
	return node
}

// terminalKey keys the terminal-exclusivity cache.
type terminalKey struct {
	dir Direction
	t   RelType
	l   Label
}

// ChainCollapseVia reports whether an unbounded zero-minimum
// reachable-set expansion over relType in dir, filtered to label, is
// equivalent to one RootsVia lookup -- and returns the root array when it
// is. Two structural facts, each verified (never assumed) and cached:
// the type must be FUNCTIONAL in dir (every node has at most one such
// rel, so the reachable set is exactly the ancestor chain), and label
// must be TERMINAL-EXCLUSIVE for it (no labeled node has such a rel
// outgoing in dir, so only a chain's terminal can carry the label --
// making "reachable nodes with the label" either the root or nothing).
func (g *Snapshot) ChainCollapseVia(relType string, dir Direction, label string) (RootsVia, bool) {
	if dir == Both {
		return nil, false
	}
	t, ok := g.RelType(relType)
	if !ok {
		return nil, false
	}
	l, ok := g.Label(label)
	if !ok {
		return nil, false
	}
	if !g.functionalCached(t, dir) {
		return nil, false
	}
	tk := terminalKey{dir: dir, t: t, l: l}
	g.rootsMu.Lock()
	tOK, tSeen := g.terminalOnly[tk]
	g.rootsMu.Unlock()
	if !tSeen {
		tOK = g.checkTerminalOnly(t, dir, l)
		g.rootsMu.Lock()
		g.terminalOnly[tk] = tOK
		g.rootsMu.Unlock()
	}
	if !tOK {
		return nil, false
	}
	return g.RootsVia(t, dir), true
}

// FunctionalVia reports whether relType is functional in dir -- every
// node has at most one such rel, so its reachability structure is a
// forest of chains (RootsVia's precondition). Verified once and cached.
func (g *Snapshot) FunctionalVia(relType string, dir Direction) bool {
	if dir == Both {
		return false
	}
	t, ok := g.RelType(relType)
	if !ok {
		return false
	}
	return g.functionalCached(t, dir)
}

// functionalCached is FunctionalVia's verify-once core.
func (g *Snapshot) functionalCached(t RelType, dir Direction) bool {
	g.rootsMu.Lock()
	fOK, fSeen := g.functionalVia[rootsKey{dir: dir, t: t}]
	g.rootsMu.Unlock()
	if fSeen {
		return fOK
	}
	fOK = g.checkFunctionalVia(t, dir)
	g.rootsMu.Lock()
	g.functionalVia[rootsKey{dir: dir, t: t}] = fOK
	g.rootsMu.Unlock()
	return fOK
}

// checkFunctionalVia verifies every node has at most one relType rel in
// dir. One CSR pass.
func (g *Snapshot) checkFunctionalVia(t RelType, dir Direction) bool {
	offsets, types := g.outOffsets, g.outTypes
	if dir == Incoming {
		offsets, types = g.inOffsets, g.inTypes
	}
	for v := 0; v+1 < len(offsets); v++ {
		n := 0
		for k := offsets[v]; k < offsets[v+1]; k++ {
			if types[k] == t {
				n++
				if n > 1 {
					return false
				}
			}
		}
	}
	return true
}

// checkTerminalOnly verifies no l-labeled node has a relType rel
// outgoing in dir. One pass over the label's members.
func (g *Snapshot) checkTerminalOnly(t RelType, dir Direction, l Label) bool {
	set, ok := g.labelIndex[l]
	if !ok {
		return true
	}
	offsets, types := g.outOffsets, g.outTypes
	if dir == Incoming {
		offsets, types = g.inOffsets, g.inTypes
	}
	for id := range set.Iter() {
		if int(id)+1 >= len(offsets) {
			continue
		}
		for k := offsets[id]; k < offsets[id+1]; k++ {
			if types[k] == t {
				return false
			}
		}
	}
	return true
}

// NeighborVia is the single neighbor each node reaches via the functional
// relType in dir (one hop -- e.g. a message's hasCreator); the depth-1
// sibling of RootsVia. Node-indexed; a node with no such neighbor maps to
// NoNeighbor. Built fresh, not cached.
func (g *Snapshot) NeighborVia(t RelType, dir Direction) RootsVia {
	n := int(g.CSRIDSpace())
	m := MatchType(t)
	out := make(RootsVia, n)
	for node := range NodeID(n) {
		out[node] = NoNeighbor
		for nb := range g.NeighborsMatch(node, dir, m) {
			out[node] = nb
			break
		}
	}
	return out
}

// buildRootsVia resolves every node's chain terminal once via path
// compression; a depth cap of the id-space size breaks any malformed
// cycle, so the build always terminates deterministically.
func (g *Snapshot) buildRootsVia(dir Direction, t RelType) RootsVia {
	n := int(g.CSRIDSpace())
	m := MatchType(t)
	const unresolved = NodeID(^uint32(0))
	root := make(RootsVia, n)
	for i := range root {
		root[i] = unresolved
	}
	var path []NodeID
	for start := range NodeID(n) {
		if root[start] != unresolved {
			continue
		}
		path = path[:0]
		cur := start
		var terminal NodeID
		for {
			if resolved := root[cur]; resolved != unresolved {
				terminal = resolved
				break
			}
			next, ok := g.FirstNeighborMatch(cur, dir, m)
			// path length <= n bounds a malformed cycle (a valid chain has
			// at most n-1 rels, so this never trips on a forest).
			if !ok || len(path) > n {
				terminal = cur
				break
			}
			path = append(path, cur)
			cur = next
		}
		for _, node := range path {
			root[node] = terminal
		}
		root[start] = terminal
	}
	return root
}

// FirstNeighborMatch is FirstNeighbor over a pre-resolved RelMatch.
func (g *Snapshot) FirstNeighborMatch(node NodeID, dir Direction, m RelMatch) (NodeID, bool) {
	for n := range g.NeighborsMatch(node, dir, m) {
		return n, true
	}
	return 0, false
}

// NodePair is an unordered node pair (Lo <= Hi), the key of FoldVia.
type NodePair struct {
	Lo, Hi NodeID
}

// FoldVia folds the m-matched rels (in dir) into a weighted node-pair map
// by projecting both endpoints of each rel through projection -- the
// one-mode / bipartite projection ("network folding") of a relation onto a
// derived node set. For every matched rel a -> b, a' = projection[a] and
// b' = projection[b] add one to the unordered pair count; self-pairs and
// endpoints projecting to NoNeighbor are skipped. projection is a flat
// node -> node array (a NeighborVia or RootsVia). Runs in parallel over
// the id space, per-chunk maps merged small-into-large.
func (g *Snapshot) FoldVia(m RelMatch, dir Direction, projection []NodeID) map[NodePair]uint64 {
	n := int(g.CSRIDSpace())
	return parallel.Fold(n,
		func() map[NodePair]uint64 { return map[NodePair]uint64{} },
		func(acc map[NodePair]uint64, i int) map[NodePair]uint64 {
			if i >= len(projection) {
				return acc
			}
			a := projection[i]
			if a == NoNeighbor {
				return acc
			}
			for dst := range g.NeighborsMatch(NodeID(i), dir, m) {
				if int(dst) >= len(projection) {
					continue
				}
				b := projection[dst]
				if b == NoNeighbor || a == b {
					continue
				}
				pair := NodePair{Lo: min(a, b), Hi: max(a, b)}
				acc[pair]++
			}
			return acc
		},
		func(x, y map[NodePair]uint64) map[NodePair]uint64 {
			if len(x) < len(y) {
				x, y = y, x
			}
			for k, v := range y {
				x[k] += v
			}
			return x
		})
}

// CoWeight selects how CoOccurring accumulates each co-occurring node's
// weight (declarative -- the kernel needs no per-element callback).
type CoWeight struct {
	distinct    bool
	distinctKey string
}

// CoCount weights by the number of shared centers (co-occurrence events).
func CoCount() CoWeight {
	return CoWeight{}
}

// CoDistinct weights by the number of distinct values of the property key
// read off each shared center (e.g. distinct co-occurrence days); a center
// lacking the key contributes nothing.
func CoDistinct(key string) CoWeight {
	return CoWeight{distinct: true, distinctKey: key}
}

// CoOccurring is seeded co-occurrence -- one-mode projection by shared
// neighbor: from seed over the m-matched rels, the nodes sharing a
// neighbor with seed (seed -(m,dir)-> centers -(m,reversed)-> others),
// seed itself excluded, each weighted per w. The seeded row of the
// node-node co-occurrence matrix; the by-shared-neighbor complement of
// FoldVia's by-rel-endpoint projection.
func (g *Snapshot) CoOccurring(seed NodeID, m RelMatch, dir Direction, w CoWeight) map[NodeID]uint64 {
	back := dir.Reverse()
	if !w.distinct {
		counts := map[NodeID]uint64{}
		for center := range g.NeighborsMatch(seed, dir, m) {
			for other := range g.NeighborsMatch(center, back, m) {
				if other != seed {
					counts[other]++
				}
			}
		}
		return counts
	}
	keyID, ok := g.PropertyKey(w.distinctKey)
	if !ok {
		return map[NodeID]uint64{}
	}
	column, ok := g.columns[keyID]
	if !ok {
		return map[NodeID]uint64{}
	}
	sets := map[NodeID]map[Value]struct{}{}
	for center := range g.NeighborsMatch(seed, dir, m) {
		val, ok := column.Get(center)
		if !ok {
			continue
		}
		for other := range g.NeighborsMatch(center, back, m) {
			if other == seed {
				continue
			}
			s, ok := sets[other]
			if !ok {
				s = map[Value]struct{}{}
				sets[other] = s
			}
			s[val] = struct{}{}
		}
	}
	out := make(map[NodeID]uint64, len(sets))
	for other, s := range sets {
		out[other] = uint64(len(s))
	}
	return out
}
