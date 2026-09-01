// Distinct-aggregate factorization through a functional chain: a final
// OPTIONAL MATCH suffix reached only through one bound variable (the
// partition variable), feeding a projection whose single aggregate is
// count(DISTINCT r) over the suffix's last relationship, factors into
//
//	... RETURN DISTINCT groupKeys, pv
//	NEXT LET rl = COUNT { suffix }
//	NEXT RETURN groupKeys, sum(rl)
//
// -- the per-pv suffix count is computed once per distinct partition
// value (the decorrelated COUNT{} side table) instead of re-enumerating
// the suffix per duplicated prefix row and deduplicating a giant edge
// set per group. Soundness: every interior suffix hop must be
// functional toward pv (each far node reaches exactly one pv-side
// node), so a distinct r determines its pv and the per-pv edge sets are
// pairwise disjoint -- summing their sizes IS the distinct count. The
// detection is shape-generic: label/type structure, variable usage, and
// the graph's functionality facts, never query identity.
package plan

import (
	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

// DisableFactorDistinct pins differential tests to the unfactored plan;
// factorDistinctFired counts rewrites so tests cannot pass vacuously.
var (
	DisableFactorDistinct bool
	factorDistinctFired   int
)

// factorRLName is the synthetic per-partition count column; a user
// column of the same name declines the rewrite rather than colliding.
const factorRLName = "__factor_rl"

// factorDistinctAgg rewrites (clauses, ret) when the shape qualifies,
// returning the inputs unchanged otherwise. It never mutates the input
// nodes -- shared subtrees are referenced, all containers are fresh.
func factorDistinctAgg(clauses []ast.Clause, ret ast.Projection, g graph.Graph) ([]ast.Clause, ast.Projection) {
	if DisableFactorDistinct || len(clauses) == 0 {
		return clauses, ret
	}
	suffix, ok := clauses[len(clauses)-1].(*ast.Match)
	if !ok || !suffix.Optional || suffix.Where != nil || suffix.Acyclic ||
		suffix.Repeatable || len(suffix.Patterns) != 1 {
		return clauses, ret
	}
	pat := &suffix.Patterns[0]
	if len(pat.Hops) == 0 {
		return clauses, ret
	}
	pv := pat.Start.Var
	if pv == "" || len(pat.Start.Props) > 0 || len(pat.Start.PropExprs) > 0 || pat.Start.Where != nil {
		return clauses, ret
	}
	// Suffix hops: fixed single directed hops, no inline predicates or
	// property filters (they could reference outer rows), every interior
	// hop functional toward pv, the last hop's relationship named.
	suffixVars := map[string]bool{}
	last := len(pat.Hops) - 1
	for i := range pat.Hops {
		h := &pat.Hops[i]
		if h.Rel.Length != nil || h.Rel.Where != nil || h.Node.Where != nil ||
			len(h.Rel.Props) > 0 || len(h.Rel.PropExprs) > 0 || len(h.Node.PropExprs) > 0 ||
			h.Rel.Dir == ast.DirBoth {
			return clauses, ret
		}
		if i < last && !functionalTowardStart(&h.Rel, g) {
			return clauses, ret
		}
		if h.Rel.Var != "" {
			suffixVars[h.Rel.Var] = true
		}
		if h.Node.Var != "" {
			suffixVars[h.Node.Var] = true
		}
	}
	distinctVar := pat.Hops[last].Rel.Var
	if distinctVar == "" {
		return clauses, ret
	}

	// The projection: exactly one aggregate, count(DISTINCT <lastRel>),
	// aliased; every other item a group key with a referenceable name
	// that reads nothing from the suffix.
	if ret.Star {
		return clauses, ret
	}
	aggIdx := -1
	type gk struct {
		item ast.ReturnItem
		name string
	}
	var gks []gk
	var aggAlias string
	for idx, it := range ret.Items {
		if semantics.ExprHasAgg(it.Expr) {
			f, isF := it.Expr.(*ast.Func)
			if !isF || aggIdx != -1 || !eqFold(f.Name, "count") || !f.Distinct ||
				f.Star || len(f.Args) != 1 || it.Alias == "" {
				return clauses, ret
			}
			v, isVar := f.Args[0].(*ast.Var)
			if !isVar || v.Name != distinctVar {
				return clauses, ret
			}
			aggIdx, aggAlias = idx, it.Alias
			continue
		}
		name := it.Alias
		if name == "" {
			v, isVar := it.Expr.(*ast.Var)
			if !isVar {
				return clauses, ret
			}
			name = v.Name
		}
		if exprReadsAny(it.Expr, suffixVars) {
			return clauses, ret
		}
		gks = append(gks, gk{item: it, name: name})
	}
	if aggIdx == -1 {
		return clauses, ret
	}
	// ORDER BY runs on the rewritten final projection, so its
	// expressions must read only output columns (and no suffix vars).
	outCols := map[string]bool{aggAlias: true}
	for _, k := range gks {
		if k.name == pv || k.name == factorRLName || outCols[k.name] {
			return clauses, ret
		}
		outCols[k.name] = true
	}
	for _, s := range ret.OrderBy {
		vars := map[string]bool{}
		collectAllVars(s.Expr, vars)
		for v := range vars {
			if !outCols[v] {
				return clauses, ret
			}
		}
	}

	// Scope facts: pv must be bound by the prefix; every suffix var must
	// be fresh. An unrecognized prefix clause declines conservatively.
	bound := map[string]bool{}
	if !prefixBoundVars(clauses[:len(clauses)-1], bound) {
		return clauses, ret
	}
	if !bound[pv] || pv == factorRLName {
		return clauses, ret
	}
	for v := range suffixVars {
		if bound[v] {
			return clauses, ret
		}
	}

	// Construction. Phase A dedups to the group granularity plus pv;
	// phase B computes the per-pv suffix count with pv still in scope.
	phaseA := ast.Projection{Distinct: true}
	for _, k := range gks {
		phaseA.Items = append(phaseA.Items, ast.ReturnItem{Expr: k.item.Expr, Alias: k.name})
	}
	phaseA.Items = append(phaseA.Items, ast.ReturnItem{Expr: &ast.Var{Name: pv}, Alias: pv})
	phaseB := ast.Projection{}
	for _, k := range gks {
		phaseB.Items = append(phaseB.Items, ast.ReturnItem{Expr: &ast.Var{Name: k.name}, Alias: k.name})
	}
	phaseB.Items = append(phaseB.Items, ast.ReturnItem{Expr: &ast.CountSub{Pattern: pat}, Alias: factorRLName})

	final := ast.Projection{Distinct: ret.Distinct, OrderBy: ret.OrderBy, Skip: ret.Skip, Limit: ret.Limit}
	ki := 0
	for idx := range ret.Items {
		if idx == aggIdx {
			final.Items = append(final.Items, ast.ReturnItem{
				Expr:  &ast.Func{Name: "sum", Args: []ast.Expr{&ast.Var{Name: factorRLName}}},
				Alias: aggAlias,
			})
			continue
		}
		name := gks[ki].name
		ki++
		final.Items = append(final.Items, ast.ReturnItem{Expr: &ast.Var{Name: name}, Alias: name})
	}

	out := make([]ast.Clause, 0, len(clauses)+1)
	out = append(out, clauses[:len(clauses)-1]...)
	out = append(out, &ast.With{Proj: phaseA}, &ast.With{Proj: phaseB})
	factorDistinctFired++
	return out, final
}

// functionalTowardStart reports whether every far-side node of the hop
// reaches at most one near-side (pv-side) node: for (near)-[:T]->(far)
// the far node's T-edges are incoming, for (near)<-[:T]-(far) outgoing.
func functionalTowardStart(rel *ast.RelPat, g graph.Graph) bool {
	switch rel.Dir {
	case ast.DirOut:
		return g.FunctionalVia(rel.Types, chickpeas.Incoming)
	case ast.DirIn:
		return g.FunctionalVia(rel.Types, chickpeas.Outgoing)
	}
	return false
}

// exprReadsAny reports whether e references any variable in names.
func exprReadsAny(e ast.Expr, names map[string]bool) bool {
	vars := map[string]bool{}
	collectAllVars(e, vars)
	for v := range vars {
		if names[v] {
			return true
		}
	}
	return false
}

// prefixBoundVars over-approximates the variables the prefix clauses
// bind (a projection boundary's dropped columns stay in the set, which
// only ever declines more). ok=false for a clause kind this pass does
// not model.
func prefixBoundVars(cs []ast.Clause, out map[string]bool) bool {
	for _, c := range cs {
		switch n := c.(type) {
		case *ast.Match:
			for i := range n.Patterns {
				patternBoundVars(&n.Patterns[i], out)
			}
		case *ast.With:
			for _, it := range n.Proj.Items {
				if it.Alias != "" {
					out[it.Alias] = true
				} else if v, ok := it.Expr.(*ast.Var); ok {
					out[v.Name] = true
				}
			}
		case *ast.Unwind:
			out[n.Var] = true
		default:
			return false
		}
	}
	return true
}

func patternBoundVars(p *ast.Pattern, out map[string]bool) {
	if p.Start.Var != "" {
		out[p.Start.Var] = true
	}
	for i := range p.Hops {
		if p.Hops[i].Rel.Var != "" {
			out[p.Hops[i].Rel.Var] = true
		}
		if p.Hops[i].Node.Var != "" {
			out[p.Hops[i].Node.Var] = true
		}
	}
}
