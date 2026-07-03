// Named-path assembly (MATCH p = ...): after the pattern binds, each row's
// path value is reconstructed from its start node and the hop's bound
// relationship position(s); the stage WHERE then runs as a post-path
// filter so predicates over nodes(p)/rels(p)/length(p) resolve.
package exec

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// bindPaths assembles each output row's named path and applies the
// post-path WHERE conjuncts.
func bindPaths(ctx *eval.Ctx, stage *plan.MatchStage, slots map[string]int, out [][]value.Value) [][]value.Value {
	pb := stage.PathBind
	for _, row := range out {
		rels := pathRelPositionsOf(row[pb.RelsSlot])
		var nodes []graph.NodeID
		if start, ok := row[pb.FromSlot].AsNode(); ok {
			nodes = reconstructPathNodes(ctx, start, rels, pb.Dir, pb.Types)
		}
		row[pb.PathSlot] = value.Path(nodes, rels)
	}
	if stage.Where == nil {
		return out
	}
	var conjs []ast.Expr
	splitConjuncts(stage.Where, &conjs)
	filters := make([]RowEval, len(conjs))
	for i, c := range conjs {
		filters[i] = compileEval(ctx, c, slots)
	}
	kept := out[:0]
	for _, row := range out {
		ok := true
		for _, f := range filters {
			if !f.Eval(ctx, row, slots).IsTruthy() {
				ok = false
				break
			}
		}
		if ok {
			kept = append(kept, row)
		}
	}
	return kept
}

// pathRelPositionsOf reads a path hop's bound relationship value: a single
// rel (fixed hop) or a list of them (quantified hop).
func pathRelPositionsOf(v value.Value) []uint32 {
	if p, ok := v.AsRel(); ok {
		return []uint32{p}
	}
	if xs, ok := v.AsList(); ok {
		out := make([]uint32, 0, len(xs))
		for _, x := range xs {
			if p, ok := x.AsRel(); ok {
				out = append(out, p)
			}
		}
		return out
	}
	return nil
}

// reconstructPathNodes walks dir/types from start through the given
// relationship positions -- each physical relationship has one position
// whichever endpoint it is walked from, so the matched hop's positions
// name a valid walk.
func reconstructPathNodes(ctx *eval.Ctx, start graph.NodeID, rels []uint32, dir graph.Direction, types []string) []graph.NodeID {
	nodes := []graph.NodeID{start}
	cur := start
	for _, pos := range rels {
		found := false
		for nb, p := range ctx.G.Relationships(cur, dir, types) {
			if p == pos {
				nodes = append(nodes, nb)
				cur = nb
				found = true
				break
			}
		}
		if !found {
			break
		}
	}
	return nodes
}
