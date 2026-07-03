// buildSegment: lower one run of stage specs plus its projection boundary
// into a Segment (port of the Rust plan.rs::build_segment, cost branch
// hard-wired as the only strategy).
package plan

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

func buildSegment(specs []stageSpec, projAST ast.Projection, postWhere ast.Expr, inCols []string, g graph.Graph) (*Segment, error) {
	// Cost pre-passes, before slot assignment, both result-identical:
	// split a linear pattern at its selective INTERIOR node, then reorder
	// the join to start from the most selective pattern. Skipped under
	// RETURN */WITH * (and thus under FILTER/LET boundaries), whose column
	// order follows slot order -- reordering would change it visibly.
	if !projAST.Star {
		specs = splitInteriorAnchors(specs, inCols, g)
		specs = reorderJoins(specs, inCols, g)
	}

	inWidth := len(inCols)
	slots := make(map[string]int, inWidth+8)
	bound := make(map[int]bool, inWidth+8)
	for i, c := range inCols {
		slots[c] = i
		bound[i] = true
	}
	nextSlot := inWidth
	var stages []Stage
	var matchWheres []ast.Expr

	for si := range specs {
		spec := &specs[si]
		switch spec.kind {
		case specMatch:
			stage, err := buildMatchStage(spec, slots, bound, &nextSlot, &matchWheres, g)
			if err != nil {
				return nil, err
			}
			stages = append(stages, stage)
		case specShortest:
			sp, err := buildSpStage(spec, slots, bound, &nextSlot)
			if err != nil {
				return nil, err
			}
			stages = append(stages, sp)
		case specCall:
			cs, err := buildCallStage(spec.proc, spec.args, spec.yields, slots, bound, &nextSlot)
			if err != nil {
				return nil, err
			}
			stages = append(stages, cs)
		case specUnwind:
			// The list is evaluated per input row: it may reference only
			// already-bound variables and cannot aggregate. The unwound
			// variable binds a fresh slot, shadowing any same name.
			if semantics.ExprHasAgg(spec.list) {
				return nil, bindErrf("aggregates are not allowed in a FOR list")
			}
			if err := semantics.CheckRefs(spec.list, slots); err != nil {
				return nil, err
			}
			outSlot := nextSlot
			nextSlot++
			slots[spec.varName] = outSlot
			bound[outSlot] = true
			stages = append(stages, &UnwindStage{List: spec.list, OutSlot: outSlot})
		case specCallSubquery:
			// Each imported variable must already be bound in the outer scope.
			importSlots := make([]int, 0, len(spec.imports))
			for _, v := range spec.imports {
				s, ok := slots[v]
				if !ok || !bound[s] {
					return nil, bindErrf("CALL subquery imports unbound variable `%s`", v)
				}
				importSlots = append(importSlots, s)
			}
			sub, err := BuildWithInCols(spec.query, spec.imports, g)
			if err != nil {
				return nil, err
			}
			// Subquery outputs become new outer variables; a collision with
			// an existing outer variable is a bind error (no shadowing).
			outSlots := make([]int, 0, len(sub.Columns))
			for _, c := range sub.Columns {
				if _, exists := slots[c]; exists {
					return nil, bindErrf("CALL subquery output `%s` collides with an outer variable", c)
				}
				s := nextSlot
				nextSlot++
				slots[c] = s
				bound[s] = true
				outSlots = append(outSlots, s)
			}
			stages = append(stages, &CallSubqueryStage{Sub: sub, ImportSlots: importSlots, OutSlots: outSlots})
		}
	}

	rowWidth := nextSlot
	// Validate each MATCH stage's WHERE against the segment's full scope.
	for _, w := range matchWheres {
		if semantics.ExprHasAgg(w) {
			return nil, bindErrf("aggregates are not allowed in WHERE")
		}
		if err := semantics.CheckRefs(w, slots); err != nil {
			return nil, err
		}
	}

	proj, err := bindProjection(projAST, slots)
	if err != nil {
		return nil, err
	}

	// When the segment projects DISTINCT (non-aggregated), a bounded
	// var-expand whose relationships/path are unused can bind each
	// distinct endpoint once instead of one row per trail (the pruning
	// var-expand -- duplicate-endpoint rows would collapse anyway).
	if proj.Distinct && !proj.Aggregated {
		flagDedupEndpoints(stages, &proj, slots)
	}

	if postWhere != nil {
		if semantics.ExprHasAgg(postWhere) {
			return nil, bindErrf("aggregates are not allowed in WHERE")
		}
		outScope := make(map[string]int, len(proj.Columns))
		for i, c := range proj.Columns {
			outScope[c] = i
		}
		if err := semantics.CheckRefs(postWhere, outScope); err != nil {
			return nil, err
		}
	}

	// Recognize the projection-derived form of the monotonic constraint
	// and push it onto the originating bounded var-expand.
	pushDerivedMonoPred(&proj, postWhere, stages, slots)

	return &Segment{
		Stages:    stages,
		RowWidth:  rowWidth,
		Slots:     slots,
		Proj:      proj,
		PostWhere: postWhere,
	}, nil
}

// buildMatchStage lowers one MATCH pattern spec: anchor choice (cost
// tiers), scan + hops, per-hop predicate extraction, label-expression
// lowering, and named-path binding.
func buildMatchStage(spec *stageSpec, slots map[string]int, bound map[int]bool, nextSlot *int, matchWheres *[]ast.Expr, g graph.Graph) (*MatchStage, error) {
	pattern := spec.pattern
	where := spec.where
	// A named-path bind is limited to a single relationship hop (Tier 1)
	// and keeps the written order so nodes(p) reads start->end.
	if spec.pathVar != "" && len(pattern.Hops) != 1 {
		return nil, planErrf("named path `%s` binding supports exactly one relationship hop (Tier 1)", spec.pathVar)
	}

	// Anchor on the most selective endpoint: rank tiers decide; a same-tier
	// tie is refined by exact anchor cardinality, then by the RESOLVED
	// anchor's real first-hop degree (hub-aware), then the average-degree
	// fan-out -- the Rust cost branch verbatim. A ranked decision is never
	// inverted, so plan shapes match the Rust cost mode.
	rank := func(n *ast.NodePat) uint8 {
		if n.Var != "" {
			if s, ok := slots[n.Var]; ok && bound[s] {
				return 4
			}
		}
		if n.Var != "" && (idSeekLiteral(where, n.Var) != nil || idSeekVar(where, n.Var, slots, bound) != NoSlot) {
			return 3
		}
		hasConcrete := false
		for i := range n.Props {
			if n.Props[i].Val.Kind != ast.LitNull {
				hasConcrete = true
				break
			}
		}
		if len(n.Labels) > 0 && (hasConcrete || (n.Var != "" && textMatchSeek(where, n.Var) != nil)) {
			return 2
		}
		if len(n.Labels) > 0 {
			return 1
		}
		return 0
	}
	if len(pattern.Hops) > 0 && spec.pathVar == "" {
		rs := rank(&pattern.Start)
		re := rank(pattern.EndNode())
		rev := pattern.Reversed()
		takeReversed := false
		if re != rs {
			takeReversed = re > rs
		} else {
			cs := anchorCard(&pattern.Start, where, slots, bound, g)
			ce := anchorCard(pattern.EndNode(), where, slots, bound, g)
			if ce != cs {
				takeReversed = ce < cs
			} else {
				// Same leaf cardinality (e.g. two single-node seeks): break
				// the tie on the resolved anchor's real first-hop degree so
				// a hub seek loses to a selective one; fall back to the
				// average-degree fan-out when an end isn't resolvable.
				rdeg, rok := resolvedFirstHopDegree(&rev, where, slots, bound, g)
				fdeg, fok := resolvedFirstHopDegree(pattern, where, slots, bound, g)
				if rok && fok && rdeg != fdeg {
					takeReversed = rdeg < fdeg
				} else {
					takeReversed = pathCost(&rev, g) < pathCost(pattern, g)
				}
			}
		}
		if takeReversed {
			pattern = &rev
		}
	}

	var ops []BindOp
	s0, wasBound := assignSlot(&pattern.Start, slots, bound, nextSlot)
	var source ScanSource
	if wasBound {
		source = ScanSource{Kind: ScanArg, Slot: s0}
	} else {
		bound[s0] = true
		source = startScanSource(pattern, where, slots, bound)
	}
	ops = append(ops, BindOp{Kind: OpScan, Slot: s0, Source: source, Labels: pattern.Start.Labels, Props: pattern.Start.Props, RelSlot: NoSlot})

	bindingPath := spec.pathVar != ""
	// For a named-path bind: the hop's rel positions slot, direction and
	// types, used to reconstruct nodes(p) after matching.
	type pathHop struct {
		relsSlot int
		dir      graph.Direction
		types    []string
	}
	var ph *pathHop
	prev := s0
	for hi := range pattern.Hops {
		rel := &pattern.Hops[hi].Rel
		node := &pattern.Hops[hi].Node
		if len(rel.Props) > 0 || len(rel.PropExprs) > 0 {
			return nil, planErrf("inline relationship properties are not supported (Tier 1)")
		}
		sn, wasBoundN := assignSlot(node, slots, bound, nextSlot)
		if !wasBoundN {
			bound[sn] = true
		}
		// A named rel variable gets a slot; a named-path bind forces a
		// hidden slot even for an anonymous relationship.
		relSlot := NoSlot
		switch {
		case rel.Var != "":
			s, ok := slots[rel.Var]
			if !ok {
				s = *nextSlot
				*nextSlot++
				slots[rel.Var] = s
			}
			bound[s] = true
			relSlot = s
		case bindingPath:
			relSlot = *nextSlot
			*nextSlot++
			bound[relSlot] = true
		}
		if bindingPath {
			ph = &pathHop{relsSlot: relSlot, dir: mapDir(rel.Dir), types: rel.Types}
		}
		op, err := buildHop(rel, prev, sn, relSlot, wasBoundN, node)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
		prev = sn
	}

	// For MATCH p = ..., allocate the path slot and record assembly.
	var pathBind *PathBindSpec
	if spec.pathVar != "" {
		pathSlot, ok := slots[spec.pathVar]
		if !ok {
			pathSlot = *nextSlot
			*nextSlot++
			slots[spec.pathVar] = pathSlot
		}
		bound[pathSlot] = true
		pathBind = &PathBindSpec{PathSlot: pathSlot, FromSlot: s0, RelsSlot: ph.relsSlot, Dir: ph.dir, Types: ph.types}
	}

	// Lift per-hop predicates onto their var-expand ops; the reduced WHERE
	// keeps the rest.
	stageWhere := where
	if err := extractVarlenHopPreds(&stageWhere, ops); err != nil {
		return nil, err
	}
	// Lower a general node label expression to a synthetic HasLabelExpr
	// conjunct on the node's variable, applied after the node binds.
	nodes := make([]*ast.NodePat, 0, len(pattern.Hops)+1)
	nodes = append(nodes, &pattern.Start)
	for hi := range pattern.Hops {
		nodes = append(nodes, &pattern.Hops[hi].Node)
	}
	for _, node := range nodes {
		if node.LabelExpr == nil {
			continue
		}
		if node.Var == "" {
			return nil, planErrf("a label expression (`|`/`!`) requires a variable on the node (Tier 1)")
		}
		conj := &ast.HasLabelExpr{Var: node.Var, Expr: node.LabelExpr}
		if stageWhere == nil {
			stageWhere = conj
		} else {
			stageWhere = &ast.Binary{Op: ast.OpAnd, LHS: stageWhere, RHS: conj}
		}
	}
	if stageWhere != nil {
		*matchWheres = append(*matchWheres, stageWhere)
	}
	return &MatchStage{Ops: ops, Where: stageWhere, Optional: spec.optional, PathBind: pathBind}, nil
}

// startScanSource picks the anchor's scan source: an id seek from a WHERE
// conjunct, a per-row id-variable seek, a substring-index candidate scan
// (labelled start, no inline anchor property), else the label/property/all
// scan.
func startScanSource(pattern *ast.Pattern, where ast.Expr, slots map[string]int, bound map[int]bool) ScanSource {
	sv := pattern.Start.Var
	if sv != "" {
		if lit := idSeekLiteral(where, sv); lit != nil {
			return ScanSource{Kind: ScanNodeID, Value: *lit}
		}
		if s := idSeekVar(where, sv, slots, bound); s != NoSlot {
			return ScanSource{Kind: ScanNodeIDVar, Slot: s}
		}
		hasConcrete := false
		for i := range pattern.Start.Props {
			if pattern.Start.Props[i].Val.Kind != ast.LitNull {
				hasConcrete = true
				break
			}
		}
		if !hasConcrete && len(pattern.Start.Labels) > 0 {
			if ts := textMatchSeek(where, sv); ts != nil {
				return ScanSource{Kind: ScanTextMatch, Label: pattern.Start.Labels[0], Field: ts.field, Mode: ts.mode, Value: ts.needle}
			}
		}
	}
	return scanSource(&pattern.Start)
}

// flagDedupEndpoints marks bounded var-expands whose relationships/path
// are unused so each distinct endpoint binds once under a DISTINCT
// projection (duplicate rows would collapse anyway). Conservative: only
// ops with no rel slot in a stage with no path bind, where no projected
// expression or ORDER BY key references a rel variable (none can -- the
// rel slot is unbound), qualify.
func flagDedupEndpoints(stages []Stage, proj *ProjPlan, slots map[string]int) {
	_ = proj
	_ = slots
	for _, s := range stages {
		ms, ok := s.(*MatchStage)
		if !ok || ms.PathBind != nil {
			continue
		}
		for i := range ms.Ops {
			op := &ms.Ops[i]
			if op.Kind == OpVarExpand && op.Max != nil && op.RelSlot == NoSlot && op.RelPred == nil {
				op.DedupEndpoints = true
			}
		}
	}
}
