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

// planCtx carries adaptive-anchor state across a planning pass. On the
// primary pass `ties` collects the patterns whose anchor orientation was a
// both-endpoints-unbound-param-seek tie the planner could only break with
// value-independent average degree -- exactly the auto-parameterization
// hazard. When exactly one such tie exists, a second pass with
// `forceReverse` set to that pattern re-plans it the other way, yielding the
// sibling Plan the cached executor chooses between per execution.
type planCtx struct {
	ties         []*ast.Pattern
	forceReverse *ast.Pattern
}

// Build desugars and plans a query against g's statistics.
func Build(q *ast.Query, g graph.Graph) (*Plan, error) {
	// Normalize before planning: non-literal inline pattern properties
	// lower to WHERE equalities, reaching nested CALL {} bodies, so the
	// recursive BuildWithInCols below need not re-run it.
	if err := semantics.Desugar(q); err != nil {
		return nil, err
	}
	pc := &planCtx{}
	p, err := buildWithInColsCtx(q, nil, g, pc)
	if err != nil {
		return nil, err
	}
	// One or more qualifying auto-param anchor ties -> build the sibling
	// with the FIRST tie's orientation flipped, leaving any further ties
	// static. A full 2^n sibling set is out of the question, but one
	// sibling for the earliest-planned tie strictly dominates giving up:
	// the query with MORE value-blind decisions used to get LESS help.
	// The adaptive chooser scores primary vs sibling by real first-hop
	// fan-out and falls back to the primary whenever it cannot score, so
	// extra unflipped ties cost nothing.
	if len(pc.ties) >= 1 {
		alt, err := buildWithInColsCtx(q, nil, g, &planCtx{forceReverse: pc.ties[0]})
		if err == nil && alt != nil {
			p.Alt = alt
		}
	}
	return p, nil
}

// BuildWithInCols plans a query whose branches each begin with inCols
// already bound, carried in from an outer scope (a CALL {} subquery plans
// its body with inCols = its import list). Adaptive anchoring is scoped to
// the top-level Build pass BY DECISION, not accident: a subquery body
// executes once per outer row, so a per-execution anchor probe there
// would run per row rather than per statement -- the probe's O(1)-ish
// budget assumes statement frequency. Subquery bodies stay on the static
// fallback until a per-row-amortized scoring exists.
func BuildWithInCols(q *ast.Query, inCols []string, g graph.Graph) (*Plan, error) {
	return buildWithInColsCtx(q, inCols, g, &planCtx{})
}

func buildWithInColsCtx(q *ast.Query, inCols []string, g graph.Graph, pc *planCtx) (*Plan, error) {
	branches := make([][]*Segment, 0, len(q.Parts))
	var columns []string
	for pi := range q.Parts {
		segments, cols, err := planPart(&q.Parts[pi], inCols, g, pc)
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
func planPart(part *ast.QueryPart, initInCols []string, g graph.Graph, pc *planCtx) ([]*Segment, []string, error) {
	var segments []*Segment
	var cur []stageSpec
	inCols := initInCols
	// Each MATCH clause is one relationship-uniqueness scope: its comma
	// patterns (and any planner splits of them) must bind pairwise
	// distinct relationships (ISO GQL's DIFFERENT EDGES default).
	scope := uint32(0)

	for _, clause := range fuseProjectionBeforeAggregate(part.Clauses) {
		switch c := clause.(type) {
		case *ast.Match:
			// Comma-separated patterns bind sequentially; the clause WHERE
			// applies once, after all of them (attached to the last).
			scope++
			last := len(c.Patterns) - 1
			for i := range c.Patterns {
				var w ast.Expr
				if i == last {
					w = c.Where
				}
				cur = append(cur, stageSpec{kind: specMatch, pattern: &c.Patterns[i], where: w, optional: c.Optional, acyclic: c.Acyclic, scope: scope})
			}
		case *ast.PathBind:
			scope++
			cur = append(cur, stageSpec{kind: specMatch, pattern: &c.Pattern, where: c.Where, optional: c.Optional, pathVar: c.PathVar, acyclic: c.Acyclic, scope: scope})
		case *ast.ShortestPath:
			cur = append(cur, stageSpec{kind: specShortest, pattern: &c.Pattern, where: c.Where, optional: c.Optional, pathVar: c.PathVar, all: c.All, weight: c.Weight})
		case *ast.CallProc:
			cur = append(cur, stageSpec{kind: specCall, proc: c.Proc, args: c.Args, yields: c.Yields})
		case *ast.Unwind:
			cur = append(cur, stageSpec{kind: specUnwind, list: c.Expr, varName: c.Var})
		case *ast.CallSubquery:
			cur = append(cur, stageSpec{kind: specCallSubquery, query: &c.Query, imports: c.Imports})
		case *ast.With:
			seg, err := buildSegment(cur, c.Proj, c.Where, inCols, g, pc)
			if err != nil {
				return nil, nil, err
			}
			cur = nil
			inCols = seg.Proj.Columns
			segments = append(segments, seg)
		}
	}
	seg, err := buildSegment(cur, part.Ret, nil, inCols, g, pc)
	if err != nil {
		return nil, nil, err
	}
	segments = append(segments, seg)
	// The monotonic pushdown runs once over the fully built segment graph:
	// it sees every stage WHERE (inline form), the same segment's boundary
	// WHERE (derived form), and a LET/FILTER split across segments
	// (cross-segment form) uniformly.
	pushMonoPreds(segments)
	// Early shortest-path row gating runs after every other segment
	// rewrite so it sees the final stage and boundary shapes.
	injectSPGates(segments)
	lowerColumnarAggs(segments)
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
