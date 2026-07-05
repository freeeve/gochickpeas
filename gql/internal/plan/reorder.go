// Join reordering and interior-anchor splitting (split from cost.go for
// the file-size norm): the cost pre-passes that run before slot
// assignment, plus their name-based selectivity and correlation guards.
package plan

import (
	"fmt"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

// patternVarNames is the variable names a pattern binds (nodes + named
// relationships), for connectivity and WHERE-dependency tests.
func patternVarNames(p *ast.Pattern) []string {
	var vs []string
	if p.Start.Var != "" {
		vs = append(vs, p.Start.Var)
	}
	for i := range p.Hops {
		if v := p.Hops[i].Rel.Var; v != "" {
			vs = append(vs, v)
		}
		if v := p.Hops[i].Node.Var; v != "" {
			vs = append(vs, v)
		}
	}
	return vs
}

// nodeAnchorCostNamed is the name-based analogue of anchorCard for the
// reorder phase (which runs before slot assignment).
func nodeAnchorCostNamed(n *ast.NodePat, where ast.Expr, bound map[string]bool, g graph.Graph) uint64 {
	if n.Var != "" && bound[n.Var] {
		return 0
	}
	if n.Var != "" {
		if lit := idSeekLiteral(where, n.Var); lit != nil && lit.Kind == ast.LitInt {
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
		if best >= 0 {
			return uint64(best)
		}
		return g.LabelCardinality(label)
	}
	return uint64(g.NodeCount())
}

// patternStartCostNamed is the cost of starting a pattern given the bound
// names: the most selective of its two endpoints.
func patternStartCostNamed(p *ast.Pattern, where ast.Expr, bound map[string]bool, g graph.Graph) uint64 {
	s := nodeAnchorCostNamed(&p.Start, where, bound, g)
	e := nodeAnchorCostNamed(p.EndNode(), where, bound, g)
	return min(s, e)
}

// whereSatisfiable reports whether a WHERE is applicable once this pattern
// binds: every reference resolvable, and no correlation on a query
// variable not yet in scope (which would silently flip a correlated
// subquery to uncorrelated).
func whereSatisfiable(where ast.Expr, p *ast.Pattern, bound map[string]bool, allVars map[string]bool) bool {
	if where == nil {
		return true
	}
	scope := make(map[string]int, len(bound)+8)
	for v := range bound {
		scope[v] = 0
	}
	for _, v := range patternVarNames(p) {
		if _, ok := scope[v]; !ok {
			scope[v] = 0
		}
	}
	refs := map[string]bool{}
	collectAllVars(where, refs)
	for v := range refs {
		if allVars[v] {
			if _, ok := scope[v]; !ok {
				return false
			}
		}
	}
	return semantics.CheckRefs(where, scope) == nil
}

// specBinds adds the variables a spec binds to out (conservative).
func specBinds(spec *stageSpec, out map[string]bool) {
	switch spec.kind {
	case specMatch:
		for _, v := range patternVarNames(spec.pattern) {
			out[v] = true
		}
		if spec.pathVar != "" {
			out[spec.pathVar] = true
		}
	case specShortest:
		out[spec.pathVar] = true
		for _, v := range patternVarNames(spec.pattern) {
			out[v] = true
		}
	case specCall:
		for _, y := range spec.yields {
			name := y.Alias
			if name == "" {
				name = y.Field
			}
			out[name] = true
		}
	case specUnwind:
		out[spec.varName] = true
	}
}

// reorderJoins reorders each maximal run of non-OPTIONAL, non-path MATCH
// patterns: greedily place the most selective startable pattern first,
// then prefer patterns connected to the bound set (original order as the
// tie-break), deferring disconnected ones. Everything else is a hard
// boundary kept in place. Result-identical: a pattern moves earlier only
// when its WHERE stays applicable, and the row set is independent of
// binding order.
func reorderJoins(specs []stageSpec, inCols []string, g graph.Graph) []stageSpec {
	bound := make(map[string]bool, len(inCols)+8)
	allVars := make(map[string]bool, len(inCols)+8)
	for _, c := range inCols {
		bound[c] = true
		allVars[c] = true
	}
	for i := range specs {
		specBinds(&specs[i], allVars)
	}
	reorderable := func(s *stageSpec) bool {
		return s.kind == specMatch && !s.optional && s.pathVar == ""
	}
	out := make([]stageSpec, 0, len(specs))
	i := 0
	for i < len(specs) {
		if !reorderable(&specs[i]) {
			specBinds(&specs[i], bound)
			out = append(out, specs[i])
			i++
			continue
		}
		j := i
		for j < len(specs) && reorderable(&specs[j]) {
			j++
		}
		run := make([]stageSpec, j-i)
		copy(run, specs[i:j])
		for len(run) > 0 {
			type key struct {
				disconnected uint8
				cost         uint64
				idx          int
			}
			best := -1
			var bestKey key
			for idx := range run {
				sp := &run[idx]
				if !whereSatisfiable(sp.where, sp.pattern, bound, allVars) {
					continue
				}
				connected := false
				for _, v := range patternVarNames(sp.pattern) {
					if bound[v] {
						connected = true
						break
					}
				}
				k := key{cost: patternStartCostNamed(sp.pattern, sp.where, bound, g), idx: idx}
				if !connected {
					k.disconnected = 1
				}
				if best < 0 || less3(k.disconnected, k.cost, k.idx, bestKey.disconnected, bestKey.cost, bestKey.idx) {
					best = idx
					bestKey = k
				}
			}
			// Nothing WHERE-satisfiable (shouldn't happen for a valid
			// query): keep the original order to stay correct.
			pick := 0
			if best >= 0 {
				pick = best
			}
			spec := run[pick]
			run = append(run[:pick], run[pick+1:]...)
			specBinds(&spec, bound)
			out = append(out, spec)
		}
		i = j
	}
	return out
}

// less3 compares (disconnected, cost, idx) triples lexicographically.
func less3(d1 uint8, c1 uint64, i1 int, d2 uint8, c2 uint64, i2 int) bool {
	if d1 != d2 {
		return d1 < d2
	}
	if c1 != c2 {
		return c1 < c2
	}
	return i1 < i2
}

// trySplitInterior splits a linear pattern at its strictly-most-selective
// INTERIOR node into two arm-patterns rooted there: the left arm anchor ->
// start (relationships flipped) and the right arm anchor -> end (forward).
// The shared anchor is scanned in the first arm and bound-referenced in
// the second; the WHERE rides the second arm so it applies once everything
// binds. Result-identical: identical nodes/rels, re-rooted.
func trySplitInterior(si int, pattern *ast.Pattern, where ast.Expr, bound map[string]bool, g graph.Graph) (*stageSpec, *stageSpec, bool) {
	n := len(pattern.Hops)
	nodeAt := func(i int) *ast.NodePat {
		if i == 0 {
			return &pattern.Start
		}
		return &pattern.Hops[i-1].Node
	}
	costs := make([]uint64, n+1)
	for i := 0; i <= n; i++ {
		costs[i] = nodeAnchorCostNamed(nodeAt(i), where, bound, g)
	}
	endpointBest := min(costs[0], costs[n])
	k, kCost := 0, uint64(0)
	first := true
	for i := 1; i < n; i++ {
		if first || costs[i] < kCost {
			k, kCost = i, costs[i]
			first = false
		}
	}
	if first || kCost >= endpointBest {
		return nil, nil, false
	}
	// The anchor must carry a variable so the second arm can reference it;
	// synthesize one for an anonymous interior node (safe: the caller
	// skips this pass under star projections).
	anchor := *nodeAt(k)
	if anchor.Var == "" {
		anchor.Var = fmt.Sprintf("__ia_%d_%d", si, k)
	}
	left := ast.Pattern{Start: anchor}
	for j := k - 1; j >= 0; j-- {
		rel := pattern.Hops[j].Rel
		rel.Dir = rel.Dir.Flipped()
		left.Hops = append(left.Hops, ast.PatternHop{Rel: rel, Node: *nodeAt(j)})
	}
	right := ast.Pattern{Start: anchor}
	right.Hops = append(right.Hops, pattern.Hops[k:]...)
	return &stageSpec{kind: specMatch, pattern: &left},
		&stageSpec{kind: specMatch, pattern: &right, where: where},
		true
}

// splitInteriorAnchors applies trySplitInterior to each eligible MATCH
// spec (non-OPTIONAL, non-path, all-simple-hop, >=2 hops), threading the
// bound-name set. Composes with reorderJoins (split first, then reorder).
func splitInteriorAnchors(specs []stageSpec, inCols []string, g graph.Graph) []stageSpec {
	bound := make(map[string]bool, len(inCols)+8)
	for _, c := range inCols {
		bound[c] = true
	}
	out := make([]stageSpec, 0, len(specs))
	for si := range specs {
		spec := &specs[si]
		eligible := spec.kind == specMatch && !spec.optional && spec.pathVar == "" && len(spec.pattern.Hops) >= 2
		if eligible {
			for i := range spec.pattern.Hops {
				if spec.pattern.Hops[i].Rel.Length != nil {
					eligible = false
					break
				}
			}
		}
		if eligible {
			if a, b, ok := trySplitInterior(si, spec.pattern, spec.where, bound, g); ok {
				specBinds(a, bound)
				specBinds(b, bound)
				out = append(out, *a, *b)
				continue
			}
		}
		specBinds(spec, bound)
		out = append(out, *spec)
	}
	return out
}
