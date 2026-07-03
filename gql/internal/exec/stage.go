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
}

// compileStage pre-resolves the stage's constant names once (labels,
// property keys, rel types, params) so the per-candidate work is bitmap
// contains + column reads.
func compileStage(ctx *eval.Ctx, stage *plan.MatchStage, slots map[string]int) *stageComp {
	sc := &stageComp{
		matchers:     make([]*graph.NodeMatcher, len(stage.Ops)),
		relMatchers:  make([]*graph.RelMatcher, len(stage.Ops)),
		levelFilters: buildLevelFilters(stage, slots),
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

// runStage walks each input row through the stage's bind chain. A
// required stage consumes its input rows.
func runStage(ctx *eval.Ctx, stage *plan.MatchStage, slots map[string]int, rows [][]value.Value) [][]value.Value {
	sc := compileStage(ctx, stage, slots)
	var out [][]value.Value
	var scratch genScratch
	for _, row := range rows {
		genMatches(ctx, stage.Ops, row, sc, slots, func(r []value.Value) {
			cp := make([]value.Value, len(r))
			copy(cp, r)
			out = append(out, cp)
		}, &scratch)
	}
	return out
}

// buildLevelFilters splits the stage WHERE into top-level AND-conjuncts
// and buckets each at the earliest op level where every slot it reads is
// bound; graph-touching conjuncts keep last-level timing. A conjunct then
// prunes a candidate the moment it can fail instead of after the whole
// pattern binds.
func buildLevelFilters(stage *plan.MatchStage, slots map[string]int) [][]RowEval {
	n := max(len(stage.Ops), 1)
	buckets := make([][]RowEval, n)
	if stage.Where == nil {
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
	var conjs []ast.Expr
	splitConjuncts(stage.Where, &conjs)
	for _, c := range conjs {
		var refs []int
		hasSlow := false
		evalPushdown(c, slots, &refs, &hasSlow)
		level := n - 1
		if !hasSlow {
			level = 0
			for _, s := range refs {
				if l, ok := slotLevel[s]; ok && l > level {
					level = l
				}
			}
		}
		buckets[min(level, n-1)] = append(buckets[min(level, n-1)], compileEval(c))
	}
	return buckets
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
