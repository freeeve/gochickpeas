// Cardinality-aware planning helpers (port of the Rust plan/cost.rs).
// Every count is an exact resident statistic, never an estimate; a probe
// ABSTAINS on Param/NamedParam/Null literals (no value at plan time --
// resolving one would bake a call's value into a shared cached plan), so
// autoparam'd templates plan value-independently.
package plan

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

// anchorCard is the measured leaf cardinality of anchoring on n (lower =
// fewer rows to start from). Mirrors the rank tiers so cardinality never
// inverts a rank decision -- it only refines a same-tier tie: bound -> 0,
// id seek -> 1, concrete property seek -> exact posting length, labelled
// -> label cardinality, else node count.
func anchorCard(n *ast.NodePat, where ast.Expr, slots map[string]int, bound map[int]bool, g graph.Graph) uint64 {
	if n.Var != "" {
		if s, ok := slots[n.Var]; ok && bound[s] {
			return 0
		}
		if idSeekLiteral(where, n.Var) != nil || idSeekVar(where, n.Var, slots, bound) != NoSlot {
			return 1
		}
	}
	if len(n.Labels) > 0 {
		label := n.Labels[0]
		best := int64(-1)
		for i := range n.Props {
			lit := n.Props[i].Val
			if lit.Kind == ast.LitParam || lit.Kind == ast.LitNamedParam || lit.Kind == ast.LitNull {
				continue
			}
			c := int64(setLen(g.NodesWithProperty(label, n.Props[i].Key, semantics.LitValue(lit))))
			if best < 0 || c < best {
				best = c
			}
		}
		if n.Var != "" {
			if ts := textMatchSeek(where, n.Var); ts != nil && ts.needle.Kind == ast.LitStr {
				if cands, ok := g.SubstringCandidates(label, ts.field, ts.needle.S); ok {
					c := int64(setLen(cands))
					if best < 0 || c < best {
						best = c
					}
				}
			}
		}
		if best >= 0 {
			return uint64(best)
		}
		return g.LabelCardinality(label)
	}
	return uint64(g.NodeCount())
}

// resolveAnchorNodes pins a node pattern to the concrete node id(s) it
// anchors on at plan time; ok=false when it can't be pinned (bound, a
// param seek with no plan-time value, or label-only).
func resolveAnchorNodes(n *ast.NodePat, where ast.Expr, slots map[string]int, bound map[int]bool, g graph.Graph) ([]graph.NodeID, bool) {
	if n.Var != "" {
		if s, ok := slots[n.Var]; ok && bound[s] {
			return nil, false
		}
		if lit := idSeekLiteral(where, n.Var); lit != nil && lit.Kind == ast.LitInt {
			return []graph.NodeID{graph.NodeID(lit.I)}, true
		}
	}
	if len(n.Labels) == 0 {
		return nil, false
	}
	for i := range n.Props {
		lit := n.Props[i].Val
		if lit.Kind == ast.LitParam || lit.Kind == ast.LitNamedParam || lit.Kind == ast.LitNull {
			continue
		}
		return setSlice(g.NodesWithProperty(n.Labels[0], n.Props[i].Key, semantics.LitValue(lit))), true
	}
	return nil, false
}

// resolvedFirstHopDegree is the exact fan-out of the FIRST hop when a
// pattern is anchored on its resolved start: the real degree of the
// resolved node(s) over the first hop's types and direction. One hop only,
// an exact resident count, never a multi-hop product. ok=false when the
// start can't be resolved or there is no hop.
func resolvedFirstHopDegree(p *ast.Pattern, where ast.Expr, slots map[string]int, bound map[int]bool, g graph.Graph) (uint64, bool) {
	if len(p.Hops) == 0 {
		return 0, false
	}
	nodes, ok := resolveAnchorNodes(&p.Start, where, slots, bound, g)
	if !ok {
		return 0, false
	}
	rel := &p.Hops[0].Rel
	dir := mapDir(rel.Dir)
	var total uint64
	for _, node := range nodes {
		total += countNeighbors(g, node, dir, rel.Types)
	}
	return total, true
}

// bothEndsUnboundParamSeek reports whether both of a hop pattern's endpoints
// are labelled, unbound, and seeked only by a parameter-valued property --
// the shape whose real anchor degree is unknown at plan time (the param has
// no value yet) but known at execution. A concrete property on an endpoint
// would let the degree probe resolve it, so it is NOT this case; a label-only
// endpoint is a genuine unknown with no value arriving later, so also not.
func bothEndsUnboundParamSeek(p *ast.Pattern, slots map[string]int, bound map[int]bool) bool {
	return isUnboundParamSeek(&p.Start, slots, bound) && isUnboundParamSeek(p.EndNode(), slots, bound)
}

// isUnboundParamSeek reports whether n is a labelled, not-yet-bound node whose
// only concrete seek is a parameter-valued property (no literal property that
// would resolve at plan time).
func isUnboundParamSeek(n *ast.NodePat, slots map[string]int, bound map[int]bool) bool {
	if n.Var != "" {
		if s, ok := slots[n.Var]; ok && bound[s] {
			return false
		}
	}
	if len(n.Labels) == 0 {
		return false
	}
	hasParam := false
	for i := range n.Props {
		switch n.Props[i].Val.Kind {
		case ast.LitParam, ast.LitNamedParam:
			hasParam = true
		case ast.LitNull:
			// structural, ignore
		default:
			// A concrete literal property resolves at plan time -> not this case.
			return false
		}
	}
	return hasParam
}

// countNeighbors counts a node's neighbors over dir restricted to types.
func countNeighbors(g graph.Graph, node graph.NodeID, dir graph.Direction, types []string) uint64 {
	var n uint64
	if len(types) == 0 {
		for range g.Neighbors(node, dir) {
			n++
		}
		return n
	}
	for range g.NeighborsByType(node, dir, types) {
		n++
	}
	return n
}
