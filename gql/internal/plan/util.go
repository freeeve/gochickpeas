// Small shared helpers for the planner.
package plan

import (
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
	"github.com/freeeve/gochickpeas/nodeset"
)

// eqFold is a case-insensitive name compare (function/procedure names).
func eqFold(a, b string) bool { return strings.EqualFold(a, b) }

// lower is ASCII lowercasing for name dispatch.
func lower(s string) string { return strings.ToLower(s) }

// sortSlice sorts s by less (a stable order is not required by callers).
func sortSlice[T any](s []T, less func(a, b T) bool) {
	sort.Slice(s, func(i, j int) bool { return less(s[i], s[j]) })
}

// exprEqual is structural expression equality (the Rust derived ==),
// used to match an ORDER BY key against a projected expression.
func exprEqual(a, b ast.Expr) bool { return reflect.DeepEqual(a, b) }

// setLen is a nil-tolerant nodeset length (a nil set is empty).
func setLen(s *nodeset.Set) int {
	if s == nil {
		return 0
	}
	return s.Len()
}

// setSlice is a nil-tolerant nodeset materialization.
func setSlice(s *nodeset.Set) []uint32 {
	if s == nil {
		return nil
	}
	return s.ToSlice()
}

// seekProbes is the set of index keys a single-value property seek must
// probe for v: the value itself plus its numeric twin when one exists
// (the index matches stored values exactly; equality coerces).
func seekProbes(v value.Value) []value.Value {
	if tw, ok := value.NumericTwin(v); ok {
		return []value.Value{v, tw}
	}
	return []value.Value{v}
}

// seekCard is the posting count a single-value property seek yields for v
// across all its probes.
func seekCard(g graph.Graph, label, key string, v value.Value) uint64 {
	var c uint64
	for _, pv := range seekProbes(v) {
		c += uint64(setLen(g.NodesWithProperty(label, key, pv)))
	}
	return c
}

// seekNodes is the sorted, deduplicated id set a single-value property
// seek yields for v across all its probes (the exact set the built scan
// serves).
func seekNodes(g graph.Graph, label, key string, v value.Value) []graph.NodeID {
	var ids []graph.NodeID
	for _, pv := range seekProbes(v) {
		ids = append(ids, setSlice(g.NodesWithProperty(label, key, pv))...)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}
