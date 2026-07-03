// Correlated subquery matching: EXISTS { MATCH ... }, COUNT { MATCH ... },
// and pattern comprehensions, all sharing one anchored DFS over the
// pattern. The DFS's evaluation-invariant setup (anchor-reversal, level
// slots, the extended scope) is shaped once per pattern and cached on the
// Ctx with its evaluation scratch, so the per-row cost is a row refresh
// instead of map/slice rebuilds. NodeMatches lives here (the Rust engine
// kept it in exec; Go moves it to break the eval -> exec module cycle).
package eval

import (
	"maps"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
)

// NodeMatches reports whether a node satisfies a pattern node's labels and
// literal inline properties: every label present and every property equal
// (params resolved through the context).
func NodeMatches(ctx *Ctx, nid graph.NodeID, labels []string, props []ast.PropEntry) bool {
	for _, l := range labels {
		if !ctx.G.HasLabel(nid, l) {
			return false
		}
	}
	for _, p := range props {
		if !ctx.G.NodePropEq(nid, p.Key, LitValue(ctx, p.Val)) {
			return false
		}
	}
	return true
}

// engineDir maps a pattern direction to the engine's traversal direction.
func engineDir(d ast.Dir) graph.Direction {
	switch d {
	case ast.DirOut:
		return graph.Outgoing
	case ast.DirIn:
		return graph.Incoming
	}
	return graph.Both
}

// evalExists is EXISTS { MATCH pattern [WHERE ...] } -- true if the
// correlated subquery matches at least once.
func evalExists(ctx *Ctx, pattern *ast.Pattern, where ast.Expr, row []value.Value, slots map[string]int) bool {
	return SubqueryCount(ctx, pattern, where, row, slots, true) > 0
}

// evalCountSub is COUNT { MATCH pattern [WHERE ...] } -- how many times
// the correlated subquery matches the current outer row.
func evalCountSub(ctx *Ctx, pattern *ast.Pattern, where ast.Expr, row []value.Value, slots map[string]int) int64 {
	return int64(SubqueryCount(ctx, pattern, where, row, slots, false))
}

// subqueryShape is the shared setup of the subquery DFS -- the pattern
// (reversed when only its far end is anchored to an outer variable), the
// per-level node patterns, their row slots, and which are outer-anchored
// -- plus the reusable evaluation scratch (row buffer, per-level
// candidates, BFS sets). Shapes cache on the Ctx keyed by pattern; validFor
// re-checks the outer scope on every hit, so a pattern evaluated under a
// different scope rebuilds instead of misbinding.
type subqueryShape struct {
	pattern   *ast.Pattern
	nodes     []*ast.NodePat
	nodeSlots []int
	anchored  []bool
	outer     map[string]int
	outerRowW int
	slots     map[string]int
	row       []value.Value
	cand      [][]graph.NodeID
	// scan0 memoizes the unanchored level-0 scan (evaluation-invariant:
	// labels and literal props only); scan0Done marks it filled.
	scan0     []graph.NodeID
	scan0Done bool
	// BFS scratch for quantified hops.
	expanded map[graph.NodeID]struct{}
	emitted  map[graph.NodeID]struct{}
	frontier []graph.NodeID
	next     []graph.NodeID
	reach    []graph.NodeID
}

// subqueryShapeFor returns the pattern's cached shape with its row buffer
// refreshed from the outer row, building (and caching) it on first use.
func subqueryShapeFor(ctx *Ctx, outer map[string]int, outerRow []value.Value, pattern *ast.Pattern) *subqueryShape {
	if ctx.subqShapes == nil {
		ctx.subqShapes = map[*ast.Pattern]*subqueryShape{}
	}
	s := ctx.subqShapes[pattern]
	if s == nil || !s.validFor(outer, outerRow) {
		s = buildSubqueryShape(outer, outerRow, pattern)
		ctx.subqShapes[pattern] = s
	}
	copy(s.row, outerRow)
	for i := s.outerRowW; i < len(s.row); i++ {
		s.row[i] = value.Null()
	}
	return s
}

// validFor reports whether the shape was built against an identical outer
// scope and row width.
func (s *subqueryShape) validFor(outer map[string]int, outerRow []value.Value) bool {
	return len(outerRow) == s.outerRowW && maps.Equal(outer, s.outer)
}

// buildSubqueryShape anchors the DFS on a bound endpoint: if the start
// node isn't an outer variable but the end node is, the pattern reverses
// so expansion starts from the (few) bound node's neighbors rather than a
// label scan. Outer variables keep their slots; inner-only pattern
// variables extend the row.
func buildSubqueryShape(outer map[string]int, outerRow []value.Value, pattern *ast.Pattern) *subqueryShape {
	isAnchored := func(n *ast.NodePat) bool {
		if n.Var == "" {
			return false
		}
		_, ok := outer[n.Var]
		return ok
	}
	if !isAnchored(&pattern.Start) && isAnchored(pattern.EndNode()) {
		rev := pattern.Reversed()
		pattern = &rev
	}
	s := &subqueryShape{
		pattern:   pattern,
		outer:     outer,
		outerRowW: len(outerRow),
		slots:     make(map[string]int, len(outer)+len(pattern.Hops)+1),
	}
	maps.Copy(s.slots, outer)
	s.nodes = append(s.nodes, &pattern.Start)
	for i := range pattern.Hops {
		s.nodes = append(s.nodes, &pattern.Hops[i].Node)
	}
	w := len(outerRow)
	for _, node := range s.nodes {
		switch {
		case node.Var != "" && hasKey(s.slots, node.Var):
			s.nodeSlots = append(s.nodeSlots, s.slots[node.Var])
			s.anchored = append(s.anchored, true)
		case node.Var != "":
			s.slots[node.Var] = w
			s.nodeSlots = append(s.nodeSlots, w)
			s.anchored = append(s.anchored, false)
			w++
		default:
			s.nodeSlots = append(s.nodeSlots, w)
			s.anchored = append(s.anchored, false)
			w++
		}
	}
	s.row = make([]value.Value, w)
	s.cand = make([][]graph.NodeID, len(s.nodes))
	return s
}

func hasKey(m map[string]int, k string) bool {
	_, ok := m[k]
	return ok
}

// SubqueryCount counts matches of a correlated subquery against the
// current outer row, anchoring pattern nodes that name an outer variable
// to their bound value. stopAtFirst short-circuits for EXISTS. A single
// fixed-length pattern (quantifiers are treated as one hop).
func SubqueryCount(ctx *Ctx, pattern *ast.Pattern, where ast.Expr, outerRow []value.Value, outerSlots map[string]int, stopAtFirst bool) int {
	s := subqueryShapeFor(ctx, outerSlots, outerRow, pattern)
	total := 0
	s.dfs(ctx, 0, func() bool {
		ok := where == nil || Eval(ctx, where, s.row, s.slots).IsTruthy()
		if ok {
			total++
		}
		return stopAtFirst && total > 0
	})
	return total
}

// evalPatternComp collects a projection over each correlated match of a
// pattern: [ (pattern) [WHERE filter] | proj ].
func evalPatternComp(ctx *Ctx, e *ast.PatternComp, row []value.Value, slots map[string]int) value.Value {
	s := subqueryShapeFor(ctx, slots, row, e.Pattern)
	out := []value.Value{}
	s.dfs(ctx, 0, func() bool {
		if e.Where == nil || Eval(ctx, e.Where, s.row, s.slots).IsTruthy() {
			out = append(out, Eval(ctx, e.Proj, s.row, s.slots))
		}
		return false
	})
	return value.List(out)
}

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
			if id, ok := s.row[slot].AsNode(); ok && NodeMatches(ctx, id, node.Labels, node.Props) {
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
		if rel.Length != nil {
			candidates = s.existsReach(ctx, fromID, rel, candidates)
		} else {
			candidates = ctx.G.AppendNeighborsByType(candidates, fromID, engineDir(rel.Dir), rel.Types)
		}
		// Filter the appended tail in place: endpoint binding and the
		// pattern node's own constraints.
		kept := candidates[:0]
		for _, nid := range candidates {
			if (!isBound || bound == nid) && NodeMatches(ctx, nid, node.Labels, node.Props) {
				kept = append(kept, nid)
			}
		}
		candidates = kept
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
	if len(node.Labels) > 0 {
		if set := ctx.G.NodesWithLabel(node.Labels[0]); set != nil {
			for id := range set.Iter() {
				if NodeMatches(ctx, id, node.Labels, node.Props) {
					s.scan0 = append(s.scan0, id)
				}
			}
		}
	} else {
		for id := graph.NodeID(0); id < ctx.G.IDSpace(); id++ {
			if NodeMatches(ctx, id, node.Labels, node.Props) {
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
func (s *subqueryShape) existsReach(ctx *Ctx, from graph.NodeID, rel *ast.RelPat, dst []graph.NodeID) []graph.NodeID {
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
			s.reach = ctx.G.AppendNeighborsByType(s.reach[:0], u, dir, rel.Types)
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
