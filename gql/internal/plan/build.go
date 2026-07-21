// buildSegment: lower one run of stage specs plus its projection boundary
// into a Segment (port of the Rust plan.rs::build_segment, cost branch
// hard-wired as the only strategy).
package plan

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

func buildSegment(specs []stageSpec, projAST ast.Projection, postWhere ast.Expr, inCols []string, g graph.Graph, pc *planCtx) (*Segment, error) {
	// Cost pre-passes, before slot assignment, all result-identical:
	// split a linear pattern at its selective INTERIOR node, reorder the
	// join to start from the most selective pattern, then split again
	// bound-aware -- reordering binds variables that can turn a pattern's
	// interior into its cheapest anchor. Skipped under RETURN */WITH *
	// (and thus under FILTER/LET boundaries), whose column order follows
	// slot order -- reordering would change it visibly.
	if !projAST.Star {
		specs = splitInteriorAnchors(specs, inCols, g)
		specs = reorderJoins(specs, inCols, g)
		specs = splitBoundAnchors(specs, inCols, g)
	}

	if err := checkPatternVarKinds(specs); err != nil {
		return nil, err
	}
	// Group-join detection runs on the final spec list (the cost pre-passes
	// never split or reorder an OPTIONAL spec); the breadth gate and the
	// rewrite fire when the candidate spec is reached below, once the
	// outer stages exist to estimate.
	gjc := detectGroupJoin(specs, &projAST, inCols)
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
			if gjc != nil && si == gjc.specIdx && gjGate(gjc, stages, g) {
				// Unshare the projection items first -- the ast is shared
				// across plannings (plan cache, flip sibling) and the
				// rewrite replaces aggregate items in place.
				projAST.Items = append([]ast.ReturnItem(nil), projAST.Items...)
				if gj, gerr := buildGroupJoinStage(gjc, spec, &projAST, slots, bound, &nextSlot, g); gerr == nil {
					stages = append(stages, gj)
					continue
				}
				// A declined inner plan falls back to the nested execution.
			}
			stage, err := buildMatchStage(spec, slots, bound, &nextSlot, &matchWheres, g, pc)
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

	// When the projection cannot see a repeated row -- a plain DISTINCT, or a
	// grouped aggregation whose every aggregate is multiplicity-insensitive --
	// a bounded var-expand whose relationships/path are unused can bind each
	// distinct endpoint once instead of one row per trail (the pruning
	// var-expand -- duplicate-endpoint rows would collapse anyway).
	if projCannotSeeDuplicateRows(&proj) {
		flagDedupEndpoints(stages, &proj, slots)
	}
	// After flagDedupEndpoints: a contributing var-expand must keep its
	// trails distinct, so the marking pass clears that flag where set.
	markRelUniqueness(stages)
	// After marking: the hash-join extraction reads the flags (its build
	// admission gate declines check-only var-expands), then the re-mark
	// recomputes Check/Contribute over the post-extraction effective
	// execution order -- extraction moves the probe and build ops relative
	// to the ops left in place, and stale flags would let a moved hop skip
	// the used-pair exclusion entirely.
	stages = hashJoinStages(stages, slots, inWidth, g)
	remarkRelUniqueness(stages)

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
// checkPatternVarKinds rejects a variable reused across element kinds
// (node, relationship, path) within a segment's patterns. The planner
// keys slots by name alone, so a cross-kind reuse would silently share
// one slot between two different bindings and the execution paths could
// disagree on which binding survives (fuzz-found, tasks/058); GQL gives
// a variable one element type, so this is a bind error.
func checkPatternVarKinds(specs []stageSpec) error {
	const (
		kNode = iota + 1
		kRel
		kPath
	)
	names := [...]string{kNode: "a node", kRel: "a relationship", kPath: "a path"}
	kinds := map[string]int{}
	claim := func(v string, k int) error {
		if v == "" {
			return nil
		}
		if prev, ok := kinds[v]; ok && prev != k {
			return bindErrf("variable `%s` is already bound as %s and cannot rebind as %s",
				v, names[prev], names[k])
		}
		kinds[v] = k
		return nil
	}
	for i := range specs {
		s := &specs[i]
		if err := claim(s.pathVar, kPath); err != nil {
			return err
		}
		if s.pattern == nil {
			continue
		}
		if err := claim(s.pattern.Start.Var, kNode); err != nil {
			return err
		}
		for h := range s.pattern.Hops {
			if err := claim(s.pattern.Hops[h].Rel.Var, kRel); err != nil {
				return err
			}
			if err := claim(s.pattern.Hops[h].Node.Var, kNode); err != nil {
				return err
			}
		}
	}
	return nil
}

func buildMatchStage(spec *stageSpec, slots map[string]int, bound map[int]bool, nextSlot *int, matchWheres *[]ast.Expr, g graph.Graph, pc *planCtx) (*MatchStage, error) {
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
		if len(n.Labels) > 0 {
			// A value seek -- inline prop OR a WHERE equality (bestPropSeek sees
			// both) -- or a substring seek makes this a tier-2 anchor.
			if _, ok := bestPropSeek(n, where, g); ok {
				return 2
			}
			if n.Var != "" && textMatchSeek(where, n.Var) != nil {
				return 2
			}
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
			// Both ends same rank. If both are unbound PARAM seeks, the entire
			// tie-break below -- exact leaf cardinality, then resolved first-hop
			// degree, then average degree -- is value-BLIND: for a param seek
			// they all fall back to label-wide statistics, yet the real anchor
			// is whichever the bound param resolves to the smaller degree, known
			// only at execution. This is the auto-parameterization anchor
			// hazard; record it so Build produces a flipped sibling and the
			// cached executor decides by the real bound-param degrees. (A
			// label-only tie has no value arriving later, so does not qualify.)
			if bothEndsUnboundParamSeek(pattern, slots, bound) {
				pc.ties = append(pc.ties, spec.pattern)
			}
			// Leaf cardinality first: it is a FILTERED quantity (the seek's
			// posting length, the label's population), where any chain
			// fan-out product is unfiltered -- and for a total chain the
			// honest fan-out estimate counts the same path multiset from
			// either end, so a total-cost comparison decides by estimate
			// noise and overrides the real signal (measured: replacing this
			// ladder with anchorCard x label-conditional chain fan-out
			// regressed Q17 6x and Q16 12x while flipping nothing usefully).
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
		// Sibling pass: flip exactly the qualifying tie's orientation so the
		// two plans seed from opposite ends; the executor chooses per row set.
		if pc.forceReverse == spec.pattern {
			takeReversed = !takeReversed
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
		source = startScanSource(pattern, where, slots, bound, g)
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
		if spec.acyclic && op.Kind == OpVarExpand {
			if op.Min == 0 || op.Max == nil {
				return nil, planErrf("ACYCLIC requires a bounded quantifier with min >= 1 -- a zero-length or unbounded pattern resolves a reachable set, not paths")
			}
			op.Acyclic = true
		}
		// A named path needs each hop's rel positions, but a zero-length or
		// unbounded quantifier resolves a reachable set with no per-path rel
		// lists -- reject like the rel-variable case instead of leaving a
		// rels slot the executor can never fill.
		if bindingPath && op.Kind == OpVarExpand && (op.Min == 0 || op.Max == nil) {
			return nil, planErrf("a named path is not supported over a zero-length or unbounded quantified pattern -- it resolves a reachable set, not paths")
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
	return &MatchStage{Ops: ops, Where: stageWhere, Optional: spec.optional, PathBind: pathBind, Scope: spec.scope, Walk: spec.walk}, nil
}
