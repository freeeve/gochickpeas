// Named-path assembly (MATCH p = ...): after the pattern binds, each row's
// path value is reconstructed from its start node and the hop's bound
// relationship position(s); the matchSink then runs the stage WHERE as a
// post-path filter so predicates over nodes(p)/rels(p)/length(p) resolve.
package exec

import (
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
)

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

// reconstructPathNodes walks from start through the given relationship
// positions -- each physical relationship has one position whichever
// endpoint it is walked from, so the matched hop's positions name a valid
// walk and each hop resolves O(1) by its endpoints. The result slice is
// fresh: it escapes into the row's path value.
func reconstructPathNodes(ctx *eval.Ctx, start graph.NodeID, rels []uint32) []graph.NodeID {
	nodes := make([]graph.NodeID, 0, len(rels)+1)
	nodes = append(nodes, start)
	cur := start
	for _, pos := range rels {
		src, dst, ok := ctx.G.RelEndpoints(pos)
		if !ok {
			break
		}
		var nb graph.NodeID
		switch cur {
		case src:
			nb = dst
		case dst:
			nb = src
		default:
			return nodes
		}
		nodes = append(nodes, nb)
		cur = nb
	}
	return nodes
}
