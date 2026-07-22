package eval

import "testing"

// TestIsKnownScalarFunc covers the binder's scalar-function predicate:
// ResolveFuncOp names (case-insensitive), the graph-resolved
// startNode/endNode/type/labels, and rejection of unknown and aggregate
// names.
func TestIsKnownScalarFunc(t *testing.T) {
	for _, name := range []string{"abs", "substring", "toInteger", "coalesce", "trim"} {
		if !IsKnownScalarFunc(name) {
			t.Fatalf("%q should be a known scalar function", name)
		}
	}
	// Case-insensitive.
	if !IsKnownScalarFunc("ABS") || !IsKnownScalarFunc("SubString") {
		t.Fatal("scalar-function names are case-insensitive")
	}
	// Graph-resolved names that are not ResolveFuncOp ops.
	for _, name := range []string{"startNode", "endNode", "type", "labels", "STARTNODE"} {
		if !IsKnownScalarFunc(name) {
			t.Fatalf("%q should be known (graph-resolved)", name)
		}
	}
	// Unknown, and an aggregate (not a scalar function), are rejected.
	for _, name := range []string{"nosuchfn", "count", "collect", ""} {
		if IsKnownScalarFunc(name) {
			t.Fatalf("%q must not be a known scalar function", name)
		}
	}
}
