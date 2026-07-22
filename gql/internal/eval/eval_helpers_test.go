package eval

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/value"
)

// Direct-contract coverage for eval helpers the package's own tests only
// reach through the interpreter/compiled integration paths: the || concat
// operator, the duration-component accessor, and the known-scalar-function
// predicate.

// TestConcatOperator covers the || operator: string and list concatenation
// only, Null for every other operand pairing (unlike +, it never adds
// numbers).
func TestConcatOperator(t *testing.T) {
	if got := Concat(value.Str("ab"), value.Str("cd")); !value.Equal(got, value.Str("abcd")) {
		t.Fatalf("string || string = %v, want abcd", got)
	}
	if got := Concat(value.Str(""), value.Str("x")); !value.Equal(got, value.Str("x")) {
		t.Fatalf("empty || x = %v, want x", got)
	}
	// list || list appends.
	l := value.List([]value.Value{value.Int(1)})
	r := value.List([]value.Value{value.Int(2), value.Int(3)})
	got, ok := Concat(l, r).AsList()
	if !ok || len(got) != 3 || !value.Equal(got[0], value.Int(1)) || !value.Equal(got[2], value.Int(3)) {
		t.Fatalf("list || list = %v,%v", got, ok)
	}
	// Mismatched or non-concatenable pairings are Null.
	if got := Concat(value.Str("a"), value.Int(1)); !got.IsNull() {
		t.Fatalf("string || int = %v, want null", got)
	}
	if got := Concat(value.List(nil), value.Str("a")); !got.IsNull() {
		t.Fatalf("list || string = %v, want null", got)
	}
	if got := Concat(value.Int(1), value.Int(2)); !got.IsNull() {
		t.Fatalf("int || int = %v, want null (|| never adds numbers)", got)
	}
}

// TestDurationComponent covers the (months, days, millis) duration accessor,
// including the independent-group totals from the doc (a years-only duration
// reads .months as the month total; a PT2H duration reads .minutes as 120).
func TestDurationComponent(t *testing.T) {
	// duration({years: 1}) -> 12 months: the months group answers
	// years/quarters/months off the same total.
	for key, want := range map[string]int64{"years": 1, "quarters": 4, "months": 12} {
		if got, ok := DurationComponent(12, 0, 0, key); !ok || got != want {
			t.Fatalf("{months:12}.%s = (%d,%v), want %d", key, got, ok, want)
		}
	}
	// duration({weeks: 2}) -> 14 days.
	for key, want := range map[string]int64{"weeks": 2, "days": 14} {
		if got, ok := DurationComponent(0, 14, 0, key); !ok || got != want {
			t.Fatalf("{days:14}.%s = (%d,%v), want %d", key, got, ok, want)
		}
	}
	// PT2H -> 7_200_000 ms: the millis group totals at each unit.
	for key, want := range map[string]int64{
		"hours": 2, "minutes": 120, "seconds": 7200, "milliseconds": 7_200_000,
	} {
		if got, ok := DurationComponent(0, 0, 7_200_000, key); !ok || got != want {
			t.Fatalf("PT2H.%s = (%d,%v), want %d", key, got, ok, want)
		}
	}
	if _, ok := DurationComponent(1, 1, 1, "fortnights"); ok {
		t.Fatal("unknown duration component should not resolve")
	}
}

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
