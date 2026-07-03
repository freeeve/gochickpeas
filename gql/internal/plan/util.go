// Small shared helpers for the planner.
package plan

import (
	"reflect"
	"sort"
	"strings"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
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
