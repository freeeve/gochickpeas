// Hash-join extraction: when a segment's bind chain re-executes an
// independent branch of the pattern per row of a large intermediate (the
// branch reads nothing the intermediate bound), nesting multiplies the two
// branches' costs. This pass rewrites that shape into a HashJoinStage --
// build the independent branch once, keyed by the variable a connecting
// expand joins it back through, and probe per outer row -- purely on
// estimated cardinality structure, never on query identity. It runs after
// markRelUniqueness so every op keeps the uniqueness flags of the original
// nested order; the executor's capture/replay protocol (see exec) makes
// the reorganized row multiset exactly the nested one.
package plan

import (
	"slices"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
)

// The decision thresholds are variables (not consts) so tests can force
// the rewrite onto small fixture graphs and compare both execution paths.
var (
	// HashJoinMinRows is the smallest estimated intermediate worth
	// protecting from a multiplying re-expansion.
	HashJoinMinRows = 1 << 16
	// HashJoinFanFactor is the minimum estimated output/input multiply of
	// the pivot stage before the rewrite is considered.
	HashJoinFanFactor = 64.0
	// HashJoinExtDivisor bounds the branch's external references: a slot
	// qualifies only when it was bound at a cumulative estimate at most
	// estIn(pivot)/HashJoinExtDivisor, which also bounds the number of
	// distinct memo tuples (re-builds) to the same fraction.
	HashJoinExtDivisor = 256.0
	// HashJoinMaxBuildRows caps the estimated materialized branch (a
	// memory bound; the win condition is the pivot gate's multiply).
	HashJoinMaxBuildRows = 1 << 22
)

// hashJoinStages applies the rewrite repeatedly until no pivot
// qualifies -- a segment can hold several independent multiplying
// branches (each pass consumes its pivot's ops, so the loop terminates).
func hashJoinStages(stages []Stage, slots map[string]int, inWidth int, g graph.Graph) []Stage {
	for range len(stages) + 1 {
		out := hashJoinOnce(stages, slots, inWidth, g)
		if out == nil {
			return stages
		}
		stages = out
	}
	return stages
}

func hashJoinOnce(stages []Stage, slots map[string]int, inWidth int, g graph.Graph) []Stage {
	boundEst := make(map[int]float64, inWidth+8)
	boundStage := make(map[int]int, inWidth+8)
	for s := range inWidth {
		boundEst[s] = 1
		boundStage[s] = -1
	}
	claim := func(slot, si int, est float64) {
		if slot < 0 {
			return
		}
		if _, seen := boundEst[slot]; !seen {
			boundEst[slot] = est
			boundStage[slot] = si
		}
	}
	rows := 1.0
	stageIn := make([]float64, len(stages))
	stageOut := make([]float64, len(stages))
	// Slots proven to hold exactly one concrete node (a resolved seek):
	// later hops from these price at the node's real degree, so the pivot
	// and build gates decide on fact where the type average would lie.
	resolved := make(map[int]graph.NodeID)
	for si, st := range stages {
		stageIn[si] = rows
		switch s := st.(type) {
		case *MatchStage:
			for oi := range s.Ops {
				op := &s.Ops[oi]
				if op.Kind != OpScan || op.Source.Kind == ScanArg {
					continue
				}
				if nodes, ok := resolveScanNodes(&op.Source, op.Labels, op.Props, g); ok && len(nodes) == 1 {
					resolved[op.Slot] = nodes[0]
				}
			}
			ests, out := matchEstAnchored(s, rows, g, resolved)
			for oi := range s.Ops {
				op := &s.Ops[oi]
				if op.Kind == OpScan {
					if op.Source.Kind != ScanArg {
						claim(op.Slot, si, float64(ests[oi]))
					}
				} else if !op.Rebind {
					claim(op.To, si, float64(ests[oi]))
				}
				if op.Kind != OpScan {
					claim(op.RelSlot, si, float64(ests[oi]))
				}
			}
			if s.PathBind != nil {
				claim(s.PathBind.PathSlot, si, out)
			}
			rows = out
		case *UnwindStage:
			rows *= unwindFanout
			claim(s.OutSlot, si, rows)
		case *SpStage:
			claim(s.PathSlot, si, rows)
		case *GateStage:
			claim(s.Sp.PathSlot, si, rows)
			for _, d := range s.Derived {
				claim(d.Slot, si, rows)
			}
		case *CallStage:
			claim(s.NodeSlot, si, rows)
			claim(s.ValueSlot, si, rows)
			claim(s.DepthSlot, si, rows)
		case *CallSubqueryStage:
			for _, o := range s.OutSlots {
				claim(o, si, rows)
			}
		}
		stageOut[si] = rows
	}
	for k := range stages {
		ms, ok := stages[k].(*MatchStage)
		if !ok || ms.Optional || ms.PathBind != nil {
			continue
		}
		if stageIn[k] < float64(HashJoinMinRows) || stageOut[k] < stageIn[k]*HashJoinFanFactor {
			continue
		}
		if out := tryHashJoin(stages, k, stageIn[k], boundEst, boundStage, resolved, slots, g); out != nil {
			return out
		}
	}
	return nil
}

// tryHashJoin attempts the rewrite at pivot stage k; nil when the shape
// does not qualify.
func tryHashJoin(stages []Stage, k int, estIn float64, boundEst map[int]float64, boundStage map[int]int, resolved map[int]graph.NodeID, slots map[string]int, g graph.Graph) []Stage {
	extMax := estIn / HashJoinExtDivisor
	extOK := func(s int) bool {
		e, ok := boundEst[s]
		return ok && boundStage[s] < k && e <= extMax
	}
	preBound := func(s int) bool {
		_, ok := boundEst[s]
		return ok && boundStage[s] < k
	}

	// The collection region: consecutive stages from the pivot; OPTIONAL
	// stages pass through untouched (their bindings and reads never
	// interact with the branch -- enforced below), any other stage kind or
	// a path bind is a hard barrier.
	end := k
	for end < len(stages) {
		ms, ok := stages[end].(*MatchStage)
		if !ok || ms.PathBind != nil {
			break
		}
		end++
	}

	// Op-level dependency-driven collection: an op joins the branch when
	// everything it reads is external-qualified or branch-bound, with
	// var-expand shapes whose uniqueness participation cannot be captured
	// as per-row pairs left post-probe (exact there: the full used-pair
	// stack is live).
	type opRef struct{ si, oi int }
	var collected []opRef
	var isCollected map[opRef]bool
	var bBound, extUsed map[int]bool
	// collect runs the op-level dependency-driven collection over stages
	// [k, lim): an op joins the branch when everything it reads is
	// external-qualified or branch-bound. Callable twice -- the keyless
	// (cartesian) fallback re-collects with lim = k+1 so the build is the
	// pivot stage's own component only, and recursion extracts the rest.
	collect := func(lim int) {
		collected = collected[:0]
		isCollected = make(map[opRef]bool)
		bBound = make(map[int]bool)
		extUsed = make(map[int]bool)
		readable := func(s int) bool {
			if bBound[s] {
				return true
			}
			if extOK(s) {
				extUsed[s] = true
				return true
			}
			return false
		}
		var reads []int
		for si := k; si < lim; si++ {
			ms := stages[si].(*MatchStage)
			if ms.Optional {
				continue
			}
			for oi := range ms.Ops {
				op := &ms.Ops[oi]
				reads = opReads(op, reads[:0])
				ok := true
				for _, r := range reads {
					if !readable(r) {
						ok = false
						break
					}
				}
				// A tracked var-expand joins the build only in its
				// per-trail bounded contributing form: a check-only or
				// dedup'd (reach-shaped) walk's output depends on the
				// row's LIVE uniqueness pairs, which a keyed build cannot
				// see, so those decline. The known mechanism that could
				// admit the check-only reach class -- capture each
				// trail's pair set at build with an empty exclusion, then
				// reconstruct the endpoint set per row by emitting an
				// endpoint iff some captured trail has no scope-live pair
				// -- is deliberately not built: the sibling engine
				// measured ~1.05x on its best case, and no workload here
				// pays for this decline yet.
				if ok && op.Kind == OpVarExpand && op.Uniq != nil &&
					!(op.Max != nil && op.Uniq.Contribute && !op.DedupEndpoints) {
					ok = false
				}
				if !ok {
					continue
				}
				ref := opRef{si, oi}
				collected = append(collected, ref)
				isCollected[ref] = true
				if op.Kind == OpScan {
					if op.Source.Kind != ScanArg {
						bBound[op.Slot] = true
					}
				} else {
					if !op.Rebind {
						bBound[op.To] = true
					}
					if op.RelSlot != NoSlot {
						bBound[op.RelSlot] = true
					}
				}
			}
		}
	}
	collect(end)
	if len(bBound) == 0 {
		return nil
	}

	// Key discovery: the connecting expand is the first non-collected
	// anonymous rebind expand joining a branch-bound endpoint to a
	// pre-pivot outer one. Its B-side endpoint keys the table; the probe
	// runs it from the outer endpoint (direction flipped when the pattern
	// hop ran from the branch side -- uniqPair is reversal-invariant).
	var probe BindOp
	reversed := false
	keySlot := -1
	eRef := opRef{-1, -1}
	for si := k; si < end && keySlot < 0; si++ {
		ms := stages[si].(*MatchStage)
		if ms.Optional {
			continue
		}
		for oi := range ms.Ops {
			op := &ms.Ops[oi]
			if isCollected[opRef{si, oi}] || op.Kind != OpExpand || !op.Rebind || op.RelSlot != NoSlot {
				continue
			}
			fromB, toB := bBound[op.From], bBound[op.To]
			switch {
			case fromB && preBound(op.To):
				probe = *op
				probe.From, probe.To = op.To, op.From
				probe.Dir = revDir(op.Dir)
				reversed = true
				keySlot = op.From
			case toB && preBound(op.From):
				probe = *op
				keySlot = op.To
			default:
				continue
			}
			probe.Rebind = false
			eRef = opRef{si, oi}
			break
		}
	}
	// Value-key fallback (the disconnected-components join): no expand
	// connects the branch, but a region WHERE equality whose sides split
	// cleanly -- one reading only branch/external slots, the other only
	// pre-pivot outer ones -- keys the table by VALUE. The conjunct is
	// consumed (it IS the join), never a stray; nulls never match, exactly
	// the equality's own semantics.
	var keyBuild, keyProbe ast.Expr
	var consumedEq ast.Expr
	if keySlot < 0 {
		for si := k; si < end && consumedEq == nil; si++ {
			ms := stages[si].(*MatchStage)
			if ms.Optional || ms.Where == nil {
				continue
			}
			var conjs []ast.Expr
			SplitAnd(ms.Where, &conjs)
			for _, c := range conjs {
				bin, ok := c.(*ast.Binary)
				if !ok || bin.Op != ast.OpEq {
					continue
				}
				side := func(e ast.Expr) (branch, outer bool) {
					refs := conjSlotRefs(e, slots)
					if len(refs) == 0 {
						return false, false
					}
					branch, outer = true, true
					for _, r := range refs {
						if !bBound[r] && !extOK(r) {
							branch = false
						}
						if !preBound(r) {
							outer = false
						}
					}
					return branch, outer
				}
				lBranch, lOuter := side(bin.LHS)
				rBranch, rOuter := side(bin.RHS)
				switch {
				case lBranch && rOuter:
					keyBuild, keyProbe = bin.LHS, bin.RHS
				case rBranch && lOuter:
					keyBuild, keyProbe = bin.RHS, bin.LHS
				default:
					continue
				}
				// The build side's external reads join the memo tuple like
				// an op's would.
				for _, r := range conjSlotRefs(keyBuild, slots) {
					if !bBound[r] {
						extUsed[r] = true
					}
				}
				consumedEq = c
				break
			}
		}
	}
	// Keyless fallback: nothing relates the branch at all -- a genuine
	// cartesian product. Build once; the probe emits every build row per
	// outer row. Output is the same product the nested loop produced; the
	// win is the branch's scan not re-running per outer row.
	cartesian := false
	if keySlot < 0 && keyBuild == nil {
		// Build-once only pays when there IS an outer to repeat against:
		// a pivot whose input is the single seed row runs its scan once
		// either way, so wrapping it in a join is a no-op with overhead.
		if estIn <= 1 {
			return nil
		}
		cartesian = true
		// Narrow the build to the pivot stage's own component: collecting
		// every satisfiable op would swallow sibling disconnected branches
		// into ONE build whose internal nesting still re-scans -- each
		// component gets its own build-once via the recursion instead.
		collect(k + 1)
		if len(bBound) == 0 {
			return nil
		}
	}

	// The materialized branch must fit the memory bound (the win itself
	// is guaranteed by the pivot gate's multiply).
	buildOps := make([]BindOp, 0, len(collected))
	for _, ref := range collected {
		buildOps = append(buildOps, stages[ref.si].(*MatchStage).Ops[ref.oi])
	}
	buildStage := &MatchStage{Ops: buildOps, Scope: stages[k].(*MatchStage).Scope}
	if _, bRows := matchEstAnchored(buildStage, 1, g, resolved); bRows > float64(HashJoinMaxBuildRows) {
		return nil
	}

	// Split each region stage's WHERE: a conjunct evaluates inside the
	// build only when it reads nothing beyond the branch and the external
	// slots the collected OPS already require -- a conjunct must never
	// admit a new external slot, because every external slot joins the
	// memo tuple and multiplies rebuilds (ops are structural anchors;
	// conjuncts are movable filters that evaluate post-probe for free).
	// The rest ("strays") are re-placed post-probe below.
	type strayConj struct {
		expr ast.Expr
		refs []int
	}
	var strays []strayConj
	var buildConjs []ast.Expr
	for si := k; si < end; si++ {
		ms := stages[si].(*MatchStage)
		if ms.Optional || ms.Where == nil {
			continue
		}
		var conjs []ast.Expr
		SplitAnd(ms.Where, &conjs)
		for _, c := range conjs {
			if c == consumedEq {
				continue // the value join's key equality, not a filter
			}
			refs := conjSlotRefs(c, slots)
			inBranch := true
			for _, r := range refs {
				if !bBound[r] && !extUsed[r] {
					inBranch = false
					break
				}
			}
			if inBranch {
				buildConjs = append(buildConjs, c)
			} else {
				strays = append(strays, strayConj{expr: c, refs: refs})
			}
		}
	}
	buildStage.Where = andJoin(buildConjs)

	payload := make([]int, 0, len(bBound))
	for s := range bBound {
		payload = append(payload, s)
	}
	slices.Sort(payload)
	ext := make([]int, 0, len(extUsed))
	for s := range extUsed {
		ext = append(ext, s)
	}
	slices.Sort(ext)

	hj := &HashJoinStage{
		Build:        []*MatchStage{buildStage},
		ExtSlots:     ext,
		KeySlot:      keySlot,
		PayloadSlots: payload,
		Probe:        probe,
		Reversed:     reversed,
		KeyBuild:     keyBuild,
		KeyProbe:     keyProbe,
		Cartesian:    cartesian,
	}

	// Reassemble: pivot-region stages keep their leftover ops in place
	// (dependencies were never reordered), the consumed connecting expand
	// and collected ops leave, empty stages vanish. newBinder records
	// which surviving region stage binds each remaining fresh slot, for
	// stray placement.
	out := make([]Stage, 0, len(stages)+2)
	out = append(out, stages[:k]...)
	out = append(out, hj)
	newBinder := make(map[int]int)
	optionalAt := make(map[int]bool)
	for si := k; si < end; si++ {
		ms := stages[si].(*MatchStage)
		if ms.Optional {
			for oi := range ms.Ops {
				op := &ms.Ops[oi]
				if s := freshBind(op); s >= 0 {
					newBinder[s] = len(out)
					optionalAt[len(out)] = true
				}
				if op.Kind != OpScan && op.RelSlot != NoSlot {
					newBinder[op.RelSlot] = len(out)
					optionalAt[len(out)] = true
				}
			}
			out = append(out, ms)
			continue
		}
		var ops []BindOp
		for oi := range ms.Ops {
			ref := opRef{si, oi}
			if isCollected[ref] || ref == eRef {
				continue
			}
			op := ms.Ops[oi]
			if s := freshBind(&op); s >= 0 {
				newBinder[s] = len(out)
			}
			if op.Kind != OpScan && op.RelSlot != NoSlot {
				newBinder[op.RelSlot] = len(out)
			}
			ops = append(ops, op)
		}
		if len(ops) == 0 {
			continue
		}
		out = append(out, &MatchStage{Ops: ops, Optional: false, Scope: ms.Scope})
	}
	out = append(out, stages[end:]...)

	// Stray placement: each cross-branch conjunct attaches at its latest
	// reference's binder -- the join stage when every reference is outer-
	// bound or payload-bound, else the surviving region stage that binds
	// the last reference. Attaching to an OPTIONAL stage would change its
	// left-join condition, and a reference bound past the region cannot
	// occur (a stage WHERE's references are bound by its own stage), so
	// either voids the rewrite.
	for _, st := range strays {
		target := -1
		for _, r := range st.refs {
			if bBound[r] || preBound(r) {
				continue
			}
			bi, ok := newBinder[r]
			if !ok {
				return nil
			}
			target = max(target, bi)
		}
		switch {
		case target < 0:
			hj.Where = andWith(hj.Where, st.expr)
		case optionalAt[target]:
			return nil
		default:
			ms := out[target].(*MatchStage)
			ms.Where = andWith(ms.Where, st.expr)
		}
	}
	return out
}

// freshBind is the slot an op newly binds (-1 when it only re-anchors or
// joins into an already-bound slot).
func freshBind(op *BindOp) int {
	if op.Kind == OpScan {
		if op.Source.Kind == ScanArg {
			return -1
		}
		return op.Slot
	}
	if op.Rebind {
		return -1
	}
	return op.To
}

// andWith conjoins extra onto base (base may be nil).
func andWith(base, extra ast.Expr) ast.Expr {
	if base == nil {
		return extra
	}
	return &ast.Binary{Op: ast.OpAnd, LHS: base, RHS: extra}
}

// opReads appends the slots an op reads before binding anything.
func opReads(op *BindOp, out []int) []int {
	switch op.Kind {
	case OpScan:
		switch op.Source.Kind {
		case ScanArg, ScanNodeIDVar:
			out = append(out, op.Source.Slot)
		case ScanExistsSeed:
			for i := range op.Source.Seeds {
				out = append(out, op.Source.Seeds[i].AnchorSlot)
			}
		}
	default:
		out = append(out, op.From)
		if op.Rebind {
			out = append(out, op.To)
		}
	}
	return out
}

// conjSlotRefs is the segment slots a conjunct references (names not in
// the slot map -- e.g. an EXISTS pattern's local variables -- are ignored;
// a collision with an outer name only delays placement, never advances
// it).
func conjSlotRefs(e ast.Expr, slots map[string]int) []int {
	vars := map[string]bool{}
	collectAllVars(e, vars)
	refs := make([]int, 0, len(vars))
	for v := range vars {
		if s, ok := slots[v]; ok {
			refs = append(refs, s)
		}
	}
	slices.Sort(refs)
	return refs
}

// andJoin folds conjuncts back into one AND tree (nil for none).
func andJoin(conjs []ast.Expr) ast.Expr {
	var out ast.Expr
	for _, c := range conjs {
		if out == nil {
			out = c
		} else {
			out = &ast.Binary{Op: ast.OpAnd, LHS: out, RHS: c}
		}
	}
	return out
}
