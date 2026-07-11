// MATCH stage runner: compile the stage's matchers and WHERE-conjunct
// pushdown buckets once, then walk each input row through the bind chain.
package exec

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// stageComp is a MATCH stage's per-op pre-resolved state: node matchers,
// rel matchers, and the WHERE conjuncts bucketed by pushdown level.
type stageComp struct {
	matchers     []*graph.NodeMatcher
	relMatchers  []*graph.RelMatcher
	levelFilters [][]RowEval
	hopGates     []hopGate
	semijoins    []semiCache
	// seedRel/seedNode are per-op per-chain per-hop matchers for
	// ScanExistsSeed walks, resolved once with the rest of the stage.
	seedRel  [][][]*graph.RelMatcher
	seedNode [][][]*graph.NodeMatcher
}

// compileStage pre-resolves the stage's constant names once (labels,
// property keys, rel types, params) so the per-candidate work is bitmap
// contains + column reads. constIn reports segment-wide hoisting-constant
// slots; sample is a seeded input row carrying their values.
func compileStage(ctx *eval.Ctx, stage *plan.MatchStage, slots map[string]int, constIn func(int) bool, sample []value.Value) *stageComp {
	sc := &stageComp{
		matchers:     make([]*graph.NodeMatcher, len(stage.Ops)),
		relMatchers:  make([]*graph.RelMatcher, len(stage.Ops)),
		levelFilters: buildLevelFilters(ctx, stage, slots, constIn, sample),
		hopGates:     buildHopGates(ctx, stage.Ops),
		semijoins:    buildSemijoins(stage.Ops),
	}
	sc.seedRel = make([][][]*graph.RelMatcher, len(stage.Ops))
	sc.seedNode = make([][][]*graph.NodeMatcher, len(stage.Ops))
	for i := range stage.Ops {
		op := &stage.Ops[i]
		props := make([]graph.PropSpec, len(op.Props))
		for j, p := range op.Props {
			props[j] = graph.PropSpec{Key: p.Key, Val: eval.LitValue(ctx, p.Val)}
		}
		sc.matchers[i] = ctx.G.CompileNodeMatcher(op.Labels, props)
		sc.relMatchers[i] = ctx.G.CompileRelMatcher(op.Types)
		if op.Kind == plan.OpScan && op.Source.Kind == plan.ScanExistsSeed {
			sc.seedRel[i] = make([][]*graph.RelMatcher, len(op.Source.Seeds))
			sc.seedNode[i] = make([][]*graph.NodeMatcher, len(op.Source.Seeds))
			for ci := range op.Source.Seeds {
				hops := op.Source.Seeds[ci].Hops
				sc.seedRel[i][ci] = make([]*graph.RelMatcher, len(hops))
				sc.seedNode[i][ci] = make([]*graph.NodeMatcher, len(hops))
				for hi := range hops {
					sc.seedRel[i][ci][hi] = ctx.G.CompileRelMatcher(hops[hi].Types)
					sc.seedNode[i][ci][hi] = ctx.G.CompileNodeMatcher(hops[hi].Labels, nil)
				}
			}
		}
	}
	return sc
}

// buildLevelFilters splits the stage WHERE into top-level AND-conjuncts
// and buckets each at the earliest op level where every slot it reads is
// bound; graph-touching conjuncts keep last-level timing. A conjunct then
// prunes a candidate the moment it can fail instead of after the whole
// pattern binds.
func buildLevelFilters(ctx *eval.Ctx, stage *plan.MatchStage, slots map[string]int, constIn func(int) bool, sample []value.Value) [][]RowEval {
	n := max(len(stage.Ops), 1)
	buckets := make([][]RowEval, n)
	if stage.Where == nil {
		return buckets
	}
	// A named-path bind assembles p only AFTER the walk, so its WHERE runs
	// as a post-path filter instead (a level filter over the still-null
	// path slot would silently drop every row).
	if stage.PathBind != nil {
		return buckets
	}
	slotLevel := make(map[int]int, len(stage.Ops))
	for i := range stage.Ops {
		op := &stage.Ops[i]
		if _, seen := slotLevel[slotOf(op)]; !seen {
			slotLevel[slotOf(op)] = i
		}
		if rs := relSlotOf(op); rs != plan.NoSlot {
			if _, seen := slotLevel[rs]; !seen {
				slotLevel[rs] = i
			}
		}
	}
	// A slot is batch-constant when the segment never binds it and the
	// seeded inputs agree on its value (constIn); a carried slot is
	// loop-invariant per match-call even when it varies across input rows.
	isBound := func(s int) bool { _, ok := slotLevel[s]; return ok }
	isConst := func(s int) bool { return !isBound(s) && constIn(s) }
	isCarried := func(s int) bool { return !isBound(s) }
	var conjs []ast.Expr
	plan.SplitAnd(stage.Where, &conjs)
	for _, c := range conjs {
		cc := hoistEval(ctx, compileEval(ctx, c, slots), isConst, isCarried, sample, slots)
		var refs []int
		hasSlow := false
		evalPushdown(cc, c, slots, &refs, &hasSlow)
		level := n - 1
		if !hasSlow {
			level = 0
			for _, s := range refs {
				if l, ok := slotLevel[s]; ok && l > level {
					level = l
				}
			}
		}
		buckets[min(level, n-1)] = append(buckets[min(level, n-1)], cc)
	}
	return buckets
}

// slotAgrees reports whether slot holds an identical value on every row of
// the batch (vacuously true for an empty or single-row batch) -- the one
// "is this slot batch-constant" test, with an explicit out-of-range
// policy. padNull treats a position beyond a row's width as Null (the
// seeded-input convention, where narrow inputs pad with nulls); without it
// any out-of-range read across differing rows disqualifies, guarding
// callers that will index rows at the slot directly.
func slotAgrees(slot int, rows [][]value.Value, padNull bool) bool {
	if len(rows) == 0 {
		return true
	}
	var v0 value.Value
	if slot < len(rows[0]) {
		v0 = rows[0][slot]
	} else if !padNull && len(rows) > 1 {
		return false
	}
	for _, r := range rows[1:] {
		var v value.Value
		if slot < len(r) {
			v = r[slot]
		} else if !padNull {
			return false
		}
		if !value.Identical(v0, v) {
			return false
		}
	}
	return true
}
