package eval

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/value"
)

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

// TestFuncOpPurityRollCall enforces the FuncOp purity contract
// behaviorally (task 299, rustychickpeas 7798b68's lesson): constant
// folding, IN-list baking, carried-list hoisting, and pushdown placement
// all evaluate a resolved function fewer times than once-per-row, which
// is only sound while every registered op is deterministic over its
// argument values. Each op runs twice over a spread of argument shapes
// and must produce identical results -- a future rand() or statement
// clock registered as a FuncOp fails here instead of folding to one draw
// for a whole batch (the exact regression the Rust sibling shipped).
func TestFuncOpPurityRollCall(t *testing.T) {
	shapes := [][]value.Value{
		nil,
		{value.Null()},
		{value.Int(7)},
		{value.Int(-3)},
		{value.Float(0.5)},
		{value.Str("Ab cD")},
		{value.Str("2012-09-16")},
		{value.List([]value.Value{value.Int(1), value.Int(2), value.Int(3)})},
		{value.Int(1), value.Int(5)},
		{value.Str("substring"), value.Int(2), value.Int(4)},
		{value.Null(), value.Int(1), value.Str("x")},
	}
	for op := range funcOpCount {
		for si, argv := range shapes {
			first := ApplyFunc(op, argv)
			second := ApplyFunc(op, argv)
			if !value.Identical(first, second) {
				t.Fatalf("FuncOp %d is nondeterministic on shape %d: %v vs %v -- volatile functions must not be registered as FuncOps", op, si, first, second)
			}
		}
	}
}
