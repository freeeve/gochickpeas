// The correlated-subquery DFS walk: level-by-level candidate generation
// over a shaped pattern (anchored endpoints, lazily compiled matchers,
// the quantified-hop reach) -- the evaluation half of subquery.go's
// shape/count entry points.
package eval

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
)

// dfs enumerates the pattern's matches level by level, invoking onMatch at
// each full assignment; onMatch returning true stops the search (the
// EXISTS fast path). Level candidates fill the shape's per-level scratch.
func (s *subqueryShape) dfs(ctx *Ctx, level int, onMatch func() bool) bool {
	if level == len(s.nodes) {
		return onMatch()
	}
	node := s.nodes[level]
	slot := s.nodeSlots[level]
	candidates := s.cand[level][:0]
	if level == 0 {
		if s.anchored[0] {
			if id, ok := s.row[slot].AsNode(); ok && ctx.G.NodeMatcherAccepts(s.nodeMatcherFor(ctx, 0), id) {
				candidates = append(candidates, id)
			}
		} else {
			candidates = append(candidates, s.existsScan(ctx, node)...)
		}
	} else {
		rel := &s.pattern.Hops[level-1].Rel
		fromID, ok := s.row[s.nodeSlots[level-1]].AsNode()
		if !ok {
			return false
		}
		var bound graph.NodeID
		isBound := false
		if s.anchored[level] {
			b, ok := s.row[slot].AsNode()
			if !ok {
				// An outer variable bound to null (e.g. an unmatched
				// OPTIONAL MATCH variable) cannot match any node.
				return false
			}
			bound, isBound = b, true
		}
		switch {
		case rel.Length == nil && isBound:
			// Both endpoints bound: count the matching relationships
			// directly instead of enumerating a candidate set, appending
			// the bound node once per relationship so match multiplicity
			// (COUNT forms) is preserved exactly.
			if ctx.G.NodeMatcherAccepts(s.nodeMatcherFor(ctx, level), bound) {
				n := ctx.G.CountNeighborsMatched(fromID, bound, engineDir(rel.Dir), s.matcherFor(ctx, level-1))
				for range n {
					candidates = append(candidates, bound)
				}
			}
		default:
			if rel.Length != nil {
				candidates = s.existsReach(ctx, fromID, rel, level-1, candidates)
			} else {
				candidates = ctx.G.AppendNeighborsMatched(candidates, fromID, engineDir(rel.Dir), s.matcherFor(ctx, level-1))
			}
			// Filter the appended tail in place: endpoint binding and the
			// pattern node's own constraints.
			m := s.nodeMatcherFor(ctx, level)
			kept := candidates[:0]
			for _, nid := range candidates {
				if (!isBound || bound == nid) && ctx.G.NodeMatcherAccepts(m, nid) {
					kept = append(kept, nid)
				}
			}
			candidates = kept
		}
	}
	s.cand[level] = candidates
	for _, c := range candidates {
		s.row[slot] = value.Node(c)
		if s.dfs(ctx, level+1, onMatch) {
			return true
		}
	}
	return false
}

// existsScan is the level-0 candidate source for an unanchored start
// node: the first label's index set (or the whole id space with no
// label), filtered by the full pattern-node match. The result is
// evaluation-invariant, memoized on the shape.
func (s *subqueryShape) existsScan(ctx *Ctx, node *ast.NodePat) []graph.NodeID {
	if s.scan0Done {
		return s.scan0
	}
	m := s.nodeMatcherFor(ctx, 0)
	if len(node.Labels) > 0 {
		if set := ctx.G.NodesWithLabel(node.Labels[0]); set != nil {
			for id := range set.Iter() {
				if ctx.G.NodeMatcherAccepts(m, id) {
					s.scan0 = append(s.scan0, id)
				}
			}
		}
	} else {
		// A sparse id space contains gap ids that are not nodes; the
		// existence oracle keeps them out (same rule as the exec fresh
		// scan's all-nodes arm).
		for id := graph.NodeID(0); id < ctx.G.IDSpace(); id++ {
			if ctx.G.NodeExists(id) && ctx.G.NodeMatcherAccepts(m, id) {
				s.scan0 = append(s.scan0, id)
			}
		}
	}
	s.scan0Done = true
	return s.scan0
}

// existsReach is a quantified hop's candidate set inside a subquery: the
// distinct nodes reachable in min..=max hops appended to dst (dedup'd BFS;
// min 0 includes the start, a nil max is unbounded). An existence test
// needs the reachable set, not per-path enumeration -- the same collapse
// varReach applies in the main pipeline. BFS state reuses the shape's
// scratch.
// nodeMatcherFor lazily compiles (once per shape) level i's node matcher;
// the shape is Ctx-cached and single-threaded, so plain lazy fill is safe.
func (s *subqueryShape) nodeMatcherFor(ctx *Ctx, level int) *graph.NodeMatcher {
	if s.nodeMatchers == nil {
		s.nodeMatchers = make([]*graph.NodeMatcher, len(s.nodes))
	}
	if s.nodeMatchers[level] == nil {
		node := s.nodes[level]
		props := make([]graph.PropSpec, len(node.Props))
		for i, p := range node.Props {
			props[i] = graph.PropSpec{Key: p.Key, Val: LitValue(ctx, p.Val)}
		}
		s.nodeMatchers[level] = ctx.G.CompileNodeMatcher(node.Labels, props)
	}
	return s.nodeMatchers[level]
}

// matcherFor lazily resolves (once per shape) hop i's rel matcher; the
// shape is Ctx-cached and single-threaded, so plain lazy fill is safe.
func (s *subqueryShape) matcherFor(ctx *Ctx, hop int) *graph.RelMatcher {
	if s.matchers == nil {
		s.matchers = make([]*graph.RelMatcher, len(s.pattern.Hops))
	}
	if s.matchers[hop] == nil {
		s.matchers[hop] = ctx.G.CompileRelMatcher(s.pattern.Hops[hop].Rel.Types)
	}
	return s.matchers[hop]
}

func (s *subqueryShape) existsReach(ctx *Ctx, from graph.NodeID, rel *ast.RelPat, hop int, dst []graph.NodeID) []graph.NodeID {
	var minHops uint64
	if rel.Length.Min != nil {
		minHops = *rel.Length.Min
	}
	maxHops := uint64(1<<63 - 1)
	if rel.Length.Max != nil {
		maxHops = *rel.Length.Max
	}
	if s.expanded == nil {
		s.expanded = map[graph.NodeID]struct{}{}
		s.emitted = map[graph.NodeID]struct{}{}
	} else {
		clear(s.expanded)
		clear(s.emitted)
	}
	dir := engineDir(rel.Dir)
	s.expanded[from] = struct{}{}
	if minHops == 0 {
		s.emitted[from] = struct{}{}
		dst = append(dst, from)
	}
	frontier := append(s.frontier[:0], from)
	next := s.next[:0]
	for depth := uint64(0); len(frontier) > 0 && depth < maxHops; depth++ {
		d := depth + 1
		next = next[:0]
		for _, u := range frontier {
			s.reach = ctx.G.AppendNeighborsMatched(s.reach[:0], u, dir, s.matcherFor(ctx, hop))
			for _, nb := range s.reach {
				if d >= minHops {
					if _, dup := s.emitted[nb]; !dup {
						s.emitted[nb] = struct{}{}
						dst = append(dst, nb)
					}
				}
				if _, seen := s.expanded[nb]; !seen {
					s.expanded[nb] = struct{}{}
					next = append(next, nb)
				}
			}
		}
		frontier, next = next, frontier
	}
	s.frontier, s.next = frontier, next
	return dst
}
