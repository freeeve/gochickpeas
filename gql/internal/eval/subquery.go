// Correlated subquery matching: EXISTS { MATCH ... }, COUNT { MATCH ... },
// and pattern comprehensions, all sharing one anchored DFS over the
// pattern. The DFS's evaluation-invariant setup (anchor-reversal, level
// slots, the extended scope, compiled node and rel matchers) is shaped
// once per pattern and cached on the Ctx with its evaluation scratch, so
// the per-row cost is a row refresh instead of map/slice/matcher rebuilds.
package eval

import (
	"maps"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
)

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
	// relSlots is each hop's rel-variable slot (-1 when the hop binds no
	// fresh fixed-hop rel var); candRel holds the per-level rel position
	// parallel to cand at levels whose incoming hop has a slot.
	relSlots []int
	candRel  [][]uint32
	// scan0 memoizes the unanchored level-0 scan (evaluation-invariant:
	// labels and literal props only); scan0Done marks it filled.
	scan0     []graph.NodeID
	scan0Done bool
	// whereFn is the compiled per-row WHERE evaluator (Ctx.CompileWhere),
	// cached per shape; whereFor guards against a shape being reused with
	// a different WHERE expression.
	whereFn  func(*Ctx, []value.Value) value.Value
	whereFor ast.Expr
	// matchers lazily holds each hop's pre-resolved rel matcher, so the
	// per-candidate seam calls skip name resolution.
	matchers []*graph.RelMatcher
	// nodeMatchers lazily holds each level's compiled node matcher (labels
	// resolved to membership tests, inline-prop params resolved to values),
	// so the per-candidate accept is a bitmap probe plus a column read
	// instead of a string-keyed label lookup -- the same hoist the main
	// expand path gets from CompileNodeMatcher, mirrored for the subquery
	// walk. Params are fixed per Ctx, so the resolved values stay valid for
	// the shape's lifetime (the invariant scan0 already relies on).
	nodeMatchers []*graph.NodeMatcher
	// BFS scratch for quantified hops.
	expanded map[graph.NodeID]struct{}
	emitted  map[graph.NodeID]struct{}
	frontier []graph.NodeID
	next     []graph.NodeID
	reach    []graph.NodeID
	// rev is the reversed-pattern shape, built when BOTH endpoints anchor
	// to outer variables: the probe then starts per row from whichever
	// bound endpoint has the smaller first-hop fan-out (a hot hub on one
	// side -- e.g. a popular tag -- otherwise costs its whole neighbor
	// list on every row).
	rev *subqueryShape
}

// subqueryShapeFor returns the pattern's cached shape with its row buffer
// refreshed from the outer row, building (and caching) it on first use.
// With both endpoints anchored, the forward or reversed shape is chosen
// per row by the bound endpoints' actual first-hop degree (O(1) reads).
func subqueryShapeFor(ctx *Ctx, outer map[string]int, outerRow []value.Value, pattern *ast.Pattern, allowSeedReverse bool) *subqueryShape {
	if ctx.subqShapes == nil {
		ctx.subqShapes = map[*ast.Pattern]*subqueryShape{}
	}
	s := ctx.subqShapes[pattern]
	if s == nil || !s.validFor(outer, outerRow) {
		s = buildSubqueryShape(outer, outerRow, pattern, allowSeedReverse)
		ctx.subqShapes[pattern] = s
	}
	if s.rev != nil {
		if a, ok := outerRow[s.nodeSlots[0]].AsNode(); ok {
			if b, ok2 := outerRow[s.rev.nodeSlots[0]].AsNode(); ok2 {
				da := ctx.G.Degree(a, engineDir(s.pattern.Hops[0].Rel.Dir))
				db := ctx.G.Degree(b, engineDir(s.rev.pattern.Hops[0].Rel.Dir))
				if db < da {
					s = s.rev
				}
			}
		}
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
// label scan. When BOTH endpoints are anchored, a reversed twin shape is
// attached and subqueryShapeFor picks a side per row by actual degree.
// When NEITHER endpoint is anchored, the walk seeds from the statically
// stronger endpoint (property literal beats bare label beats unlabeled)
// when the caller allows reversal -- an unlabeled written start would
// otherwise seed the level-0 scan with the whole id space to reach a
// seek-sized end. Reversal enumerates the same match set, so counting
// callers are safe; a COLLECTING caller (pattern comprehension) must pass
// allowSeedReverse=false, because reversing its walk reverses the list's
// element order -- a visible output change. Outer variables keep their
// slots; inner-only pattern variables extend the row.
func buildSubqueryShape(outer map[string]int, outerRow []value.Value, pattern *ast.Pattern, allowSeedReverse bool) *subqueryShape {
	isAnchored := func(n *ast.NodePat) bool {
		if n.Var == "" {
			return false
		}
		_, ok := outer[n.Var]
		return ok
	}
	startAnchored := isAnchored(&pattern.Start)
	endAnchored := isAnchored(pattern.EndNode())
	switch {
	case !startAnchored && endAnchored:
		rev := pattern.Reversed()
		pattern = &rev
	case allowSeedReverse && !startAnchored && !endAnchored && len(pattern.Hops) > 0 &&
		seedStrength(pattern.EndNode()) > seedStrength(&pattern.Start):
		rev := pattern.Reversed()
		pattern = &rev
	}
	s := buildOneSubqueryShape(outer, outerRow, pattern)
	if startAnchored && endAnchored && len(pattern.Hops) > 0 {
		rev := pattern.Reversed()
		s.rev = buildOneSubqueryShape(outer, outerRow, &rev)
	}
	return s
}

// seedStrength ranks a node pattern's static selectivity as a level-0
// seed: an inline property ({name: 'x'} or a param) beats a bare label
// beats an unlabeled node.
func seedStrength(n *ast.NodePat) int {
	if len(n.Props) > 0 || len(n.PropExprs) > 0 {
		return 2
	}
	if len(n.Labels) > 0 || n.LabelExpr != nil {
		return 1
	}
	return 0
}

// buildOneSubqueryShape builds a single direction's shape.
func buildOneSubqueryShape(outer map[string]int, outerRow []value.Value, pattern *ast.Pattern) *subqueryShape {
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
	// A fixed hop's fresh rel variable gets a slot too, bound per
	// traversed relationship in the walk -- a subquery WHERE reading it
	// (k.w > 1) otherwise evaluated against null and silently rejected
	// every row. An outer-bound name keeps its outer slot (reads work;
	// the walk does not re-check identity, matching prior behavior), and
	// a quantified hop's var stays unbound (the reachable-set collapse
	// has no single relationship to bind).
	s.relSlots = make([]int, len(pattern.Hops))
	for i := range pattern.Hops {
		s.relSlots[i] = -1
		rv := pattern.Hops[i].Rel.Var
		if rv != "" && pattern.Hops[i].Rel.Length == nil && !hasKey(s.slots, rv) {
			s.slots[rv] = w
			s.relSlots[i] = w
			w++
		}
	}
	s.row = make([]value.Value, w)
	s.cand = make([][]graph.NodeID, len(s.nodes))
	s.candRel = make([][]uint32, len(s.nodes))
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
	s := subqueryShapeFor(ctx, outerSlots, outerRow, pattern, true)
	wf := s.whereEval(ctx, where)
	total := 0
	s.dfs(ctx, 0, func() bool {
		if wf == nil || wf(ctx, s.row).IsTruthy() {
			total++
		}
		return stopAtFirst && total > 0
	})
	return total
}

// whereEval resolves the walk's WHERE evaluator: compiled once per shape
// through Ctx.CompileWhere when the executor installed it, else the
// per-row interpreter. subqWhereCompiles is the engagement oracle.
func (s *subqueryShape) whereEval(ctx *Ctx, where ast.Expr) func(*Ctx, []value.Value) value.Value {
	if where == nil {
		return nil
	}
	if ctx.CompileWhere == nil || ctx.ForceInterp {
		return func(c *Ctx, row []value.Value) value.Value { return Eval(c, where, row, s.slots) }
	}
	if s.whereFn == nil || s.whereFor != where {
		s.whereFn = ctx.CompileWhere(where, s.slots)
		s.whereFor = where
		subqWhereCompiles++
	}
	return s.whereFn
}

// subqWhereCompiles counts compiled subquery WHERE builds -- the
// engagement oracle: a walk over N candidates must compile its WHERE
// exactly once per shape, not per row; 0 across a filtered subquery
// means every candidate paid the interpreter.
var subqWhereCompiles int

// SubqueryGroupCount evaluates a correlated subquery ONCE with the group
// variable left free, returning the match count bucketed by the node the
// group variable binds to. It is the decorrelated form of SubqueryCount:
// where SubqueryCount re-walks the pattern once per outer entity bound to
// the group variable, a single SubqueryGroupCount call -- anchored on the
// SHARED endpoint (anchorVar, still bound from the outer row) -- yields every
// entity's count in one pass over the anchored set, so the per-entity cost
// collapses from a walk to an O(1) map read.
//
// anchorVar must name an outer variable bound in outerRow; groupVar names the
// pattern endpoint to bucket by, left UNBOUND here even when it is itself an
// outer variable (its outer binding is ignored -- enumeration rebinds it per
// match). The returned map is keyed only by node id, so it is independent of
// what the outer query calls the group variable: two subqueries with the same
// pattern, WHERE, and anchor share one table (see task 084 / rustychickpeas
// 091). The result for a given entity equals SubqueryCount with that entity
// bound -- the invariant the decor parity test asserts.
func SubqueryGroupCount(ctx *Ctx, pattern *ast.Pattern, where ast.Expr, outerRow []value.Value, outerSlots map[string]int, anchorVar, groupVar string) map[graph.NodeID]int {
	// Anchor only anchorVar: a reduced outer scope that drops groupVar so the
	// group endpoint enumerates through the DFS instead of binding.
	outer := make(map[string]int, len(outerSlots))
	for k, v := range outerSlots {
		if k != groupVar {
			outer[k] = v
		}
	}
	s := buildSubqueryShape(outer, outerRow, pattern, true)
	copy(s.row, outerRow)
	for i := len(outerRow); i < len(s.row); i++ {
		s.row[i] = value.Null()
	}
	out := map[graph.NodeID]int{}
	gslot, ok := s.slots[groupVar]
	if !ok {
		return out
	}
	wf := s.whereEval(ctx, where)
	s.dfs(ctx, 0, func() bool {
		if wf == nil || wf(ctx, s.row).IsTruthy() {
			if gid, ok := s.row[gslot].AsNode(); ok {
				out[gid]++
			}
		}
		return false
	})
	return out
}

// evalPatternComp collects a projection over each correlated match of a
// pattern: [ (pattern) [WHERE filter] | proj ].
func evalPatternComp(ctx *Ctx, e *ast.PatternComp, row []value.Value, slots map[string]int) value.Value {
	// Collecting walk: list element order is enumeration order, so the
	// unanchored seed reversal must not fire here.
	s := subqueryShapeFor(ctx, slots, row, e.Pattern, false)
	wf := s.whereEval(ctx, e.Where)
	out := []value.Value{}
	s.dfs(ctx, 0, func() bool {
		if wf == nil || wf(ctx, s.row).IsTruthy() {
			out = append(out, Eval(ctx, e.Proj, s.row, s.slots))
		}
		return false
	})
	return value.List(out)
}
