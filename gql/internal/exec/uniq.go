// MATCH-scope used-relationship state (port of the Rust exec.rs UniqEnv):
// one in-flight row's bound relationship-uniqueness keys, tagged by scope
// (= source MATCH clause). Owned by the segment chain and threaded through
// genMatches and its sinks so a scope spanning chained stages (comma
// patterns, planner splits) sees one stack per row; the DFS's push/pop
// discipline empties it between rows automatically. Tiny in practice
// (<= the number of tracked rel ops on the current partial row), so a
// linear slice beats hashing.
package exec

import (
	"github.com/freeeve/gochickpeas/gql/internal/graph"
)

// uniqKey is one used relationship: its canonical endpoint pair, tagged by
// uniqueness scope.
type uniqKey struct {
	scope uint32
	a, b  graph.NodeID
}

// uniqEnv is the used-pair stack one in-flight row carries.
type uniqEnv struct {
	stack []uniqKey
}

// used reports whether the scope already bound the pair.
func (u *uniqEnv) used(scope uint32, a, b graph.NodeID) bool {
	for _, k := range u.stack {
		if k.scope == scope && k.a == a && k.b == b {
			return true
		}
	}
	return false
}

// uniqPair is the edge-canonical relationship-uniqueness key for a hop
// from `from` to `to` over dir: the ORDERED pair in the relationship's own
// source->target orientation ((from, to) for an outgoing hop, (to, from)
// for an incoming one), or the UNORDERED pair for an undirected hop.
//
//   - A directed hop keys on the edge's (source, target), so a
//     there-and-back cycle's two distinct edges a->b and b->a get distinct
//     keys and both may be used in one clause -- while the SAME physical
//     edge traversed from either endpoint (outgoing a->b vs incoming b<-a)
//     normalizes to one key and is never used twice.
//   - An undirected hop keys on the unordered pair, so the double-stored
//     undirected convention (a->b + b->a for one logical edge) collapses
//     to one edge. Genuine parallel rels between one pair also collapse --
//     the documented multigraph deviation, matching the trail walk's key.
func uniqPair(dir graph.Direction, from, to graph.NodeID) (graph.NodeID, graph.NodeID) {
	switch dir {
	case graph.Outgoing:
		return from, to
	case graph.Incoming:
		return to, from
	}
	return min(from, to), max(from, to)
}
