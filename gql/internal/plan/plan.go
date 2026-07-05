// Plan entry points: desugar + split a query into UNION branches and
// projection-boundary segments (port of the Rust plan.rs top half). The
// planner is cost-based only: the Rust engine's rank/cost mode switch and
// thread-local override do not exist here -- the cost branch is the one
// and only strategy, layered on the same rank tiers so plan shapes match
// the Rust cost mode exactly.
package plan

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

// Build desugars and plans a query against g's statistics.
func Build(q *ast.Query, g graph.Graph) (*Plan, error) {
	// Normalize before planning: non-literal inline pattern properties
	// lower to WHERE equalities, reaching nested CALL {} bodies, so the
	// recursive BuildWithInCols below need not re-run it.
	if err := semantics.Desugar(q); err != nil {
		return nil, err
	}
	return BuildWithInCols(q, nil, g)
}

// BuildWithInCols plans a query whose branches each begin with inCols
// already bound, carried in from an outer scope (a CALL {} subquery plans
// its body with inCols = its import list).
func BuildWithInCols(q *ast.Query, inCols []string, g graph.Graph) (*Plan, error) {
	branches := make([][]*Segment, 0, len(q.Parts))
	var columns []string
	for pi := range q.Parts {
		segments, cols, err := planPart(&q.Parts[pi], inCols, g)
		if err != nil {
			return nil, err
		}
		// Every UNION branch must project the same column names in order.
		if columns == nil {
			columns = cols
		} else if !stringsEqual(columns, cols) {
			return nil, bindErrf("all UNION branches must return the same columns: [%s] vs [%s]",
				joinComma(columns), joinComma(cols))
		}
		branches = append(branches, segments)
	}
	return &Plan{Branches: branches, Union: q.Union, Columns: columns}, nil
}

// planPart plans one UNION branch into its segment pipeline, returning the
// segments and the branch's output column names.
func planPart(part *ast.QueryPart, initInCols []string, g graph.Graph) ([]*Segment, []string, error) {
	var segments []*Segment
	var cur []stageSpec
	inCols := initInCols

	for _, clause := range fuseProjectionBeforeAggregate(part.Clauses) {
		switch c := clause.(type) {
		case *ast.Match:
			// Comma-separated patterns bind sequentially; the clause WHERE
			// applies once, after all of them (attached to the last).
			last := len(c.Patterns) - 1
			for i := range c.Patterns {
				var w ast.Expr
				if i == last {
					w = c.Where
				}
				cur = append(cur, stageSpec{kind: specMatch, pattern: &c.Patterns[i], where: w, optional: c.Optional, acyclic: c.Acyclic})
			}
		case *ast.PathBind:
			cur = append(cur, stageSpec{kind: specMatch, pattern: &c.Pattern, where: c.Where, optional: c.Optional, pathVar: c.PathVar, acyclic: c.Acyclic})
		case *ast.ShortestPath:
			cur = append(cur, stageSpec{kind: specShortest, pattern: &c.Pattern, where: c.Where, optional: c.Optional, pathVar: c.PathVar, all: c.All, weight: c.Weight})
		case *ast.CallProc:
			cur = append(cur, stageSpec{kind: specCall, proc: c.Proc, args: c.Args, yields: c.Yields})
		case *ast.Unwind:
			cur = append(cur, stageSpec{kind: specUnwind, list: c.Expr, varName: c.Var})
		case *ast.CallSubquery:
			cur = append(cur, stageSpec{kind: specCallSubquery, query: &c.Query, imports: c.Imports})
		case *ast.With:
			seg, err := buildSegment(cur, c.Proj, c.Where, inCols, g)
			if err != nil {
				return nil, nil, err
			}
			cur = nil
			inCols = seg.Proj.Columns
			segments = append(segments, seg)
		}
	}
	seg, err := buildSegment(cur, part.Ret, nil, inCols, g)
	if err != nil {
		return nil, nil, err
	}
	segments = append(segments, seg)
	// Cross-segment monotonic pushdown: a LET-projected rels list and the
	// FILTER constraining it split into adjacent segments, out of reach of
	// the same-segment pushdown in buildSegment.
	pushCrossSegmentMono(segments)
	return segments, seg.Proj.Columns, nil
}

// pathCost estimates the fan-out of traversing p from its start: the
// product of each hop's average degree. Used only to break anchor ties, so
// an unknown/any-type hop counts as a neutral 1.
func pathCost(p *ast.Pattern, g graph.Graph) float64 {
	cost := 1.0
	for i := range p.Hops {
		rel := &p.Hops[i].Rel
		dir := mapDir(rel.Dir)
		deg := 1.0
		if len(rel.Types) > 0 {
			deg = 0.0
			for _, t := range rel.Types {
				deg += g.AvgDegree(t, dir)
			}
		}
		if deg > 0 {
			cost *= deg
		}
	}
	return cost
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func joinComma(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}
