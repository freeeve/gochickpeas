// OPTIONAL-MATCH group-join decorrelation (port of the Rust sibling's
// technique): when an OPTIONAL MATCH's bindings are consumed ONLY through
// bare null-skipping duplicate-decomposable aggregates grouped by outer
// variables, the clause is semantically a grouped left join whose inner
// pattern can be planned as a standalone query -- free to take its own
// selective anchor, where the nested walk is chained to the outer rows'
// bindings. The inner executes once into a key -> aggregate-values table,
// each outer row binds synthetic columns by correlation-key lookup with
// the aggregates' empty-group identities as the fill, and the segment
// projection re-aggregates those columns (count -> sum), which reproduces
// the nested left-join answer exactly, including over duplicate outer
// rows.
//
// The rewrite's dual: a SELECTIVE outer's nested walk is cheap while the
// standalone inner enumerates globally, so it is gated on the estimated
// outer breadth -- below the floor the nested execution stays. An
// inner-side estimate gate was tried and rejected on the Rust side: the
// estimator prices the inner at raw average fan, blind to the selective
// anchor that actually runs.
package plan

import (
	"fmt"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

// GroupJoinMinOuterRows is the estimated-outer-breadth floor below which
// the nested OPTIONAL execution stays (a variable so tests can force the
// rewrite onto fixture-scale graphs).
var GroupJoinMinOuterRows = 1024.0

// GroupJoinMinCoverage is the minimum fraction of the correlation
// variable's population the outer must reach. The standalone inner
// enumerates its pattern for EVERY correlation node while the nested walk
// only expands the outer's slice, so their cost ratio IS the coverage: a
// broad-but-narrow outer (thousands of rows over millions of candidates)
// keeps the cheap nested walk (measured: a tag-seek outer of ~4k messages
// against 3M messages regressed 65x under a breadth-only gate).
var GroupJoinMinCoverage = 0.5

// gjAgg is one consumed aggregate: the projection item it rewrites, the
// aggregate function, and the output column name it must keep.
type gjAgg struct {
	item int
	fn   string
	name string
}

// gjCandidate is a detected group-join shape: the optional spec's index,
// the correlation variables (outer vars the pattern reads, in first-
// appearance order), their labels as written in the outer patterns
// (first labeled occurrence; "" when never labeled -- the coverage gate
// then prices the population at the whole node count), and the consumed
// aggregates.
type gjCandidate struct {
	specIdx    int
	corr       []string
	corrLabels []string
	aggs       []gjAgg
}

// exprVars collects every variable an expression reads (bare references
// and property bases, subquery interiors included via the shared walker).
func exprVars(e ast.Expr) []string {
	var out []string
	ast.Walk(e, func(x ast.Expr) bool {
		switch n := x.(type) {
		case *ast.Var:
			out = append(out, n.Name)
		case *ast.Prop:
			out = append(out, n.Var)
		}
		return true
	})
	return out
}

// patternVars lists a pattern's variable names in appearance order
// (duplicates kept; callers dedup).
func patternVars(p *ast.Pattern) []string {
	var out []string
	if p.Start.Var != "" {
		out = append(out, p.Start.Var)
	}
	for i := range p.Hops {
		if v := p.Hops[i].Rel.Var; v != "" {
			out = append(out, v)
		}
		if v := p.Hops[i].Node.Var; v != "" {
			out = append(out, v)
		}
	}
	return out
}

// detectGroupJoin recognizes the decorrelatable shape over the final spec
// list and the projection: the LAST spec is a single-pattern OPTIONAL
// MATCH whose fresh bindings feed only qualifying aggregates. Everything
// else declines -- count(*) (it counts the null fill row), DISTINCT or
// unsupported aggregates, aggregates over outer-only arguments (they
// count join multiplicity the standalone inner cannot see), nested
// aggregates, inner variables escaping through group keys or ORDER BY,
// an uncorrelated optional, and WHERE reads beyond the pattern's own
// variables.
func detectGroupJoin(specs []stageSpec, projAST *ast.Projection, inCols []string) *gjCandidate {
	n := len(specs) - 1
	if n < 1 {
		return nil
	}
	sp := &specs[n]
	if sp.kind != specMatch || !sp.optional || sp.pathVar != "" || sp.walk || sp.pattern == nil {
		return nil
	}
	outer := map[string]bool{}
	for _, c := range inCols {
		outer[c] = true
	}
	for i := 0; i < n; i++ {
		s := &specs[i]
		switch s.kind {
		case specMatch, specShortest:
			for _, v := range patternVars(s.pattern) {
				outer[v] = true
			}
			if s.pathVar != "" {
				outer[s.pathVar] = true
			}
		case specUnwind:
			outer[s.varName] = true
		default:
			// CALL yields / subquery outputs would need their own var
			// accounting; decline rather than guess.
			return nil
		}
	}
	pvSet := map[string]bool{}
	inner := map[string]bool{}
	var corr []string
	for _, v := range patternVars(sp.pattern) {
		if pvSet[v] {
			continue
		}
		pvSet[v] = true
		if outer[v] {
			corr = append(corr, v)
		} else {
			inner[v] = true
		}
	}
	if len(corr) == 0 || len(inner) == 0 {
		return nil
	}
	// The optional's WHERE moves into the standalone inner, so it may read
	// only the pattern's own variables.
	if sp.where != nil {
		for _, v := range exprVars(sp.where) {
			if !pvSet[v] {
				return nil
			}
		}
	}
	if projAST.Star || projAST.Distinct {
		return nil
	}
	readsInner := func(e ast.Expr) bool {
		for _, v := range exprVars(e) {
			if inner[v] {
				return true
			}
		}
		return false
	}
	var aggs []gjAgg
	for i := range projAST.Items {
		it := &projAST.Items[i]
		if f, ok := it.Expr.(*ast.Func); ok && semantics.IsAggName(f.Name) {
			switch f.Name {
			case "count", "sum", "min", "max":
			default:
				return nil
			}
			if f.Distinct || f.Star || len(f.Args) != 1 || semantics.ExprHasAgg(f.Args[0]) {
				return nil
			}
			hasInner := false
			for _, v := range exprVars(f.Args[0]) {
				if !pvSet[v] {
					return nil
				}
				if inner[v] {
					hasInner = true
				}
			}
			if !hasInner {
				return nil
			}
			name := it.Alias
			if name == "" {
				name = semantics.DerivedName(it.Expr)
			}
			aggs = append(aggs, gjAgg{item: i, fn: f.Name, name: name})
			continue
		}
		if readsInner(it.Expr) {
			return nil
		}
	}
	if len(aggs) == 0 {
		return nil
	}
	for _, si := range projAST.OrderBy {
		if readsInner(si.Expr) {
			return nil
		}
	}
	labels := make([]string, len(corr))
	for i, v := range corr {
		labels[i] = varLabelIn(specs[:n+1], v)
	}
	return &gjCandidate{specIdx: n, corr: corr, corrLabels: labels, aggs: aggs}
}

// varLabelIn finds v's first written label across the specs' patterns
// ("" when v is never labeled).
func varLabelIn(specs []stageSpec, v string) string {
	nodeLabel := func(np *ast.NodePat) string {
		if np.Var == v && len(np.Labels) > 0 {
			return np.Labels[0]
		}
		return ""
	}
	for i := range specs {
		p := specs[i].pattern
		if p == nil {
			continue
		}
		if l := nodeLabel(&p.Start); l != "" {
			return l
		}
		for h := range p.Hops {
			if l := nodeLabel(&p.Hops[h].Node); l != "" {
				return l
			}
		}
	}
	return ""
}

// gjGate is the rewrite's economics check over the built outer prefix:
// the estimated breadth must clear the absolute floor AND cover enough of
// every correlation variable's population that running the standalone
// inner over ALL of it beats re-walking the nested pattern per outer row.
func gjGate(c *gjCandidate, stages []Stage, g graph.Graph) bool {
	breadth := gjOuterBreadth(stages, g)
	if breadth < GroupJoinMinOuterRows {
		return false
	}
	for _, l := range c.corrLabels {
		pop := float64(g.NodeCount())
		if l != "" {
			pop = float64(g.LabelCardinality(l))
		}
		if breadth < GroupJoinMinCoverage*pop {
			return false
		}
	}
	return true
}

// gjOuterBreadth is the estimated row count leaving the already-built
// stage prefix -- the group-join gate's outer breadth. Hop fan-outs use
// the LABEL-CONDITIONAL average degree whenever the hop's source label is
// known, falling back to the global average: the global statistic
// inflates for every label merely touching a hub-heavy type, and an
// inflated breadth fires the rewrite over a selective outer whose nested
// walk was cheap (measured: two tag/country-seek chains estimated past
// the floor globally regressed 65x and 210x before this refinement while
// their conditional estimates sit two decades under it).
func gjOuterBreadth(stages []Stage, g graph.Graph) float64 {
	rows := 1.0
	labelOf := map[int]string{}
	for _, st := range stages {
		ms, ok := st.(*MatchStage)
		if !ok {
			continue
		}
		anchorDeg, anchorDegOK := anchorFirstHopDegree(ms, g)
		for i := range ms.Ops {
			op := &ms.Ops[i]
			if op.Kind == OpScan {
				if len(op.Labels) > 0 {
					labelOf[op.Slot] = op.Labels[0]
				}
				if op.Source.Kind != ScanArg {
					rows *= float64(scanCard(&op.Source, op.Props, g))
				}
				continue
			}
			// The anchor's resolved degree beats any average; a
			// label-conditional average beats the global one.
			var deg *float64
			if i == 1 && anchorDegOK {
				deg = &anchorDeg
			} else if lbl, ok := labelOf[op.From]; ok {
				if d, ok2 := condFanout(lbl, op.Types, op.Dir, g); ok2 {
					deg = &d
				}
			}
			rows = opEst(op, rows, deg, g)
			if len(op.Labels) > 0 {
				labelOf[op.To] = op.Labels[0]
			}
		}
		if ms.Where != nil {
			rows *= whereSel
		}
	}
	return rows
}

// condFanout sums the label-conditional average degree over types;
// ok=false when no type has a conditional statistic (the caller falls
// back to the global fan-out).
func condFanout(label string, types []string, dir graph.Direction, g graph.Graph) (float64, bool) {
	total, any := 0.0, false
	for _, t := range types {
		if d, ok := g.AvgDegreeByLabel(label, t, dir); ok {
			total += d
			any = true
		}
	}
	return total, any
}

// buildGroupJoinStage synthesizes the standalone inner query (the optional
// pattern and WHERE, projecting the correlation keys then the aggregates),
// plans it, binds the synthetic output slots, and rewrites the caller's
// projection items to re-aggregate them. All caller-visible mutation
// happens only after the inner plan built successfully, so a decline falls
// back to the nested execution untouched. The caller must have unshared
// projAST.Items before calling (the ast is shared across plannings).
func buildGroupJoinStage(c *gjCandidate, spec *stageSpec, projAST *ast.Projection, slots map[string]int, bound map[int]bool, nextSlot *int, g graph.Graph) (*GroupJoinStage, error) {
	items := make([]ast.ReturnItem, 0, len(c.corr)+len(c.aggs))
	for _, v := range c.corr {
		items = append(items, ast.ReturnItem{Expr: &ast.Var{Name: v}, Alias: v})
	}
	for i, a := range c.aggs {
		items = append(items, ast.ReturnItem{Expr: projAST.Items[a.item].Expr, Alias: fmt.Sprintf("$gj%d", i)})
	}
	innerQ := &ast.Query{Parts: []ast.QueryPart{{
		Clauses: []ast.Clause{&ast.Match{Patterns: []ast.Pattern{*spec.pattern}, Where: spec.where, Acyclic: spec.acyclic}},
		Ret:     ast.Projection{Items: items},
	}}}
	sub, err := Build(innerQ, g)
	if err != nil {
		return nil, err
	}
	gj := &GroupJoinStage{Sub: sub}
	for _, v := range c.corr {
		gj.KeySlots = append(gj.KeySlots, slots[v])
	}
	for i, a := range c.aggs {
		syn := fmt.Sprintf("$gj%d", i)
		s := *nextSlot
		*nextSlot++
		slots[syn] = s
		bound[s] = true
		gj.OutSlots = append(gj.OutSlots, s)
		fill, mapped := FillNull, a.fn
		if a.fn == "count" {
			fill, mapped = FillZero, "sum"
		}
		gj.Fills = append(gj.Fills, fill)
		projAST.Items[a.item] = ast.ReturnItem{
			Expr:  &ast.Func{Name: mapped, Args: []ast.Expr{&ast.Var{Name: syn}}},
			Alias: a.name,
		}
	}
	return gj, nil
}
