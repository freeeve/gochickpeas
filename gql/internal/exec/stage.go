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
	hopFilters   []*hopFilter
	semijoins    []semiCache
}

// compileStage pre-resolves the stage's constant names once (labels,
// property keys, rel types, params) so the per-candidate work is bitmap
// contains + column reads.
func compileStage(ctx *eval.Ctx, stage *plan.MatchStage, slots map[string]int, rows [][]value.Value) *stageComp {
	sc := &stageComp{
		matchers:     make([]*graph.NodeMatcher, len(stage.Ops)),
		relMatchers:  make([]*graph.RelMatcher, len(stage.Ops)),
		levelFilters: buildLevelFilters(ctx, stage, slots, rows),
		hopFilters:   buildHopFilters(ctx, stage.Ops),
		semijoins:    buildSemijoins(stage.Ops),
	}
	for i := range stage.Ops {
		op := &stage.Ops[i]
		props := make([]graph.PropSpec, len(op.Props))
		for j, p := range op.Props {
			props[j] = graph.PropSpec{Key: p.Key, Val: eval.LitValue(ctx, p.Val)}
		}
		sc.matchers[i] = ctx.G.CompileNodeMatcher(op.Labels, props)
		sc.relMatchers[i] = ctx.G.CompileRelMatcher(op.Types)
	}
	return sc
}

// runStage walks each input row through the stage's bind chain. An
// optional stage re-emits the input row (new variables left null) when
// nothing matches -- the left-join semantics of OPTIONAL MATCH. opRows is
// PROFILE's per-op counter slice, accumulated across every input row
// (nil when not profiling).
func runStage(ctx *eval.Ctx, stage *plan.MatchStage, slots map[string]int, rows [][]value.Value, opRows []uint64) [][]value.Value {
	sc := compileStage(ctx, stage, slots, rows)
	var out [][]value.Value
	var scratch genScratch
	sink := func(r []value.Value) {
		cp := make([]value.Value, len(r))
		copy(cp, r)
		out = append(out, cp)
	}
	for _, row := range rows {
		if stage.Optional {
			// gen_matches mutates the row it walks, so keep the original
			// for the no-match re-emit.
			orig := make([]value.Value, len(row))
			copy(orig, row)
			before := len(out)
			genMatches(ctx, stage.Ops, row, sc, slots, sink, &scratch, opRows)
			if len(out) == before {
				out = append(out, orig)
			}
			continue
		}
		genMatches(ctx, stage.Ops, row, sc, slots, sink, &scratch, opRows)
	}
	// MATCH p = ...: assemble each row's named path, then run the stage
	// WHERE as a post-path filter so nodes(p)/rels(p)/length(p) resolve.
	if stage.PathBind != nil {
		out = bindPaths(ctx, stage, slots, out)
	}
	return out
}

// buildLevelFilters splits the stage WHERE into top-level AND-conjuncts
// and buckets each at the earliest op level where every slot it reads is
// bound; graph-touching conjuncts keep last-level timing. A conjunct then
// prunes a candidate the moment it can fail instead of after the whole
// pattern binds.
func buildLevelFilters(ctx *eval.Ctx, stage *plan.MatchStage, slots map[string]int, rows [][]value.Value) [][]RowEval {
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
	// A slot is batch-constant when carried in (not bound here) and
	// identical across every input row; a carried slot is loop-invariant
	// per match-call even when it varies across input rows.
	isBound := func(s int) bool { _, ok := slotLevel[s]; return ok }
	isConst := func(s int) bool { return !isBound(s) && slotConstant(s, rows) }
	isCarried := func(s int) bool { return !isBound(s) }
	var sample []value.Value
	if len(rows) > 0 {
		sample = rows[0]
	}
	var conjs []ast.Expr
	splitConjuncts(stage.Where, &conjs)
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

// slotConstant reports whether slot holds an identical value on every row
// of the batch (vacuously true for an empty batch).
func slotConstant(slot int, rows [][]value.Value) bool {
	for i := 1; i < len(rows); i++ {
		if slot >= len(rows[0]) || slot >= len(rows[i]) {
			return false
		}
		if !value.Identical(rows[0][slot], rows[i][slot]) {
			return false
		}
	}
	return true
}

// splitConjuncts flattens a WHERE expression's top-level AND chain.
func splitConjuncts(e ast.Expr, out *[]ast.Expr) {
	if b, ok := e.(*ast.Binary); ok && b.Op == ast.OpAnd {
		splitConjuncts(b.LHS, out)
		splitConjuncts(b.RHS, out)
		return
	}
	*out = append(*out, e)
}
