// Correlated subquery matching: EXISTS { MATCH ... }, COUNT { MATCH ... },
// and pattern comprehensions, all sharing one anchored DFS over the
// pattern. NodeMatches lives here (the Rust engine kept it in exec; Go
// moves it to break the eval -> exec module cycle).
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

// subqueryFrame is the shared setup of the subquery DFS: the pattern
// (reversed when only its far end is anchored to an outer variable), the
// per-level node patterns, their row slots, and which are outer-anchored.
type subqueryFrame struct {
	pattern   *ast.Pattern
	nodes     []*ast.NodePat
	nodeSlots []int
	anchored  []bool
	slots     map[string]int
	row       []value.Value
}

// newSubqueryFrame anchors the DFS on a bound endpoint: if the start node
// isn't an outer variable but the end node is, the pattern reverses so
// expansion starts from the (few) bound node's neighbors rather than a
// label scan. Outer variables keep their slots; inner-only pattern
// variables extend the row.
func newSubqueryFrame(outer map[string]int, outerRow []value.Value, pattern *ast.Pattern) *subqueryFrame {
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
	f := &subqueryFrame{
		pattern: pattern,
		slots:   make(map[string]int, len(outer)+len(pattern.Hops)+1),
		row:     append([]value.Value(nil), outerRow...),
	}
	maps.Copy(f.slots, outer)
	f.nodes = append(f.nodes, &pattern.Start)
	for i := range pattern.Hops {
		f.nodes = append(f.nodes, &pattern.Hops[i].Node)
	}
	for _, node := range f.nodes {
		switch {
		case node.Var != "" && hasKey(f.slots, node.Var):
			f.nodeSlots = append(f.nodeSlots, f.slots[node.Var])
			f.anchored = append(f.anchored, true)
		case node.Var != "":
			s := len(f.row)
			f.row = append(f.row, value.Null())
			f.slots[node.Var] = s
			f.nodeSlots = append(f.nodeSlots, s)
			f.anchored = append(f.anchored, false)
		default:
			f.nodeSlots = append(f.nodeSlots, len(f.row))
			f.row = append(f.row, value.Null())
			f.anchored = append(f.anchored, false)
		}
	}
	return f
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
	f := newSubqueryFrame(outerSlots, outerRow, pattern)
	total := 0
	f.dfs(ctx, 0, func() bool {
		ok := where == nil || Eval(ctx, where, f.row, f.slots).IsTruthy()
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
	f := newSubqueryFrame(slots, row, e.Pattern)
	out := []value.Value{}
	f.dfs(ctx, 0, func() bool {
		if e.Where == nil || Eval(ctx, e.Where, f.row, f.slots).IsTruthy() {
			out = append(out, Eval(ctx, e.Proj, f.row, f.slots))
		}
		return false
	})
	return value.List(out)
}

// dfs enumerates the pattern's matches level by level, invoking onMatch at
// each full assignment; onMatch returning true stops the search (the
// EXISTS fast path).
func (f *subqueryFrame) dfs(ctx *Ctx, level int, onMatch func() bool) bool {
	if level == len(f.nodes) {
		return onMatch()
	}
	node := f.nodes[level]
	slot := f.nodeSlots[level]
	var candidates []graph.NodeID
	if level == 0 {
		if f.anchored[0] {
			if id, ok := f.row[slot].AsNode(); ok && NodeMatches(ctx, id, node.Labels, node.Props) {
				candidates = []graph.NodeID{id}
			}
		} else {
			candidates = existsScan(ctx, node)
		}
	} else {
		rel := &f.pattern.Hops[level-1].Rel
		fromID, ok := f.row[f.nodeSlots[level-1]].AsNode()
		if !ok {
			return false
		}
		var bound graph.NodeID
		isBound := false
		if f.anchored[level] {
			b, ok := f.row[slot].AsNode()
			if !ok {
				// An outer variable bound to null (e.g. an unmatched
				// OPTIONAL MATCH variable) cannot match any node.
				return false
			}
			bound, isBound = b, true
		}
		keep := func(nid graph.NodeID) {
			if (!isBound || bound == nid) && NodeMatches(ctx, nid, node.Labels, node.Props) {
				candidates = append(candidates, nid)
			}
		}
		if rel.Length != nil {
			for _, nid := range existsReach(ctx, fromID, rel) {
				keep(nid)
			}
		} else {
			for nid := range existsNeighbors(ctx, fromID, rel) {
				keep(nid)
			}
		}
	}
	for _, c := range candidates {
		f.row[slot] = value.Node(c)
		if f.dfs(ctx, level+1, onMatch) {
			return true
		}
	}
	return false
}

// existsScan is the level-0 candidate source for an unanchored start
// node: the first label's index set (or the whole id space with no
// label), filtered by the full pattern-node match.
func existsScan(ctx *Ctx, node *ast.NodePat) []graph.NodeID {
	var out []graph.NodeID
	if len(node.Labels) > 0 {
		set := ctx.G.NodesWithLabel(node.Labels[0])
		if set == nil {
			return nil
		}
		for id := range set.Iter() {
			if NodeMatches(ctx, id, node.Labels, node.Props) {
				out = append(out, id)
			}
		}
		return out
	}
	for id := graph.NodeID(0); id < ctx.G.IDSpace(); id++ {
		if NodeMatches(ctx, id, node.Labels, node.Props) {
			out = append(out, id)
		}
	}
	return out
}

// existsNeighbors iterates the neighbors reachable over one pattern hop.
func existsNeighbors(ctx *Ctx, from graph.NodeID, rel *ast.RelPat) func(func(graph.NodeID) bool) {
	dir := engineDir(rel.Dir)
	if len(rel.Types) == 0 {
		return ctx.G.Neighbors(from, dir)
	}
	return ctx.G.NeighborsByType(from, dir, rel.Types)
}

// existsReach is a quantified hop's candidate set inside a subquery: the
// distinct nodes reachable in min..=max hops (dedup'd BFS; min 0 includes
// the start, a nil max is unbounded). An existence test needs the
// reachable set, not per-path enumeration -- the same collapse varReach
// applies in the main pipeline.
func existsReach(ctx *Ctx, from graph.NodeID, rel *ast.RelPat) []graph.NodeID {
	var minHops uint64
	if rel.Length.Min != nil {
		minHops = *rel.Length.Min
	}
	maxHops := uint64(1<<63 - 1)
	if rel.Length.Max != nil {
		maxHops = *rel.Length.Max
	}
	var out []graph.NodeID
	expanded := map[graph.NodeID]struct{}{from: {}}
	emitted := map[graph.NodeID]struct{}{}
	if minHops == 0 {
		emitted[from] = struct{}{}
		out = append(out, from)
	}
	frontier := []graph.NodeID{from}
	for depth := uint64(0); len(frontier) > 0 && depth < maxHops; depth++ {
		d := depth + 1
		var next []graph.NodeID
		for _, u := range frontier {
			for nb := range existsNeighbors(ctx, u, rel) {
				if d >= minHops {
					if _, dup := emitted[nb]; !dup {
						emitted[nb] = struct{}{}
						out = append(out, nb)
					}
				}
				if _, seen := expanded[nb]; !seen {
					expanded[nb] = struct{}{}
					next = append(next, nb)
				}
			}
		}
		frontier = next
	}
	return out
}
