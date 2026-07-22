package plan

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
)

// Coverage for two group-join decision helpers reached only through full
// Build runs: the variable-label lookup over stage specs and the
// label-conditional fan-out estimate.

// TestVarLabelIn covers resolving a node variable's first label across a
// spec list, including hop nodes, unlabeled/absent vars, and skipped
// nil-pattern specs.
func TestVarLabelIn(t *testing.T) {
	start := []stageSpec{{pattern: &ast.Pattern{
		Start: ast.NodePat{Var: "x", Labels: []string{"Person", "Actor"}},
	}}}
	if l := varLabelIn(start, "x"); l != "Person" {
		t.Fatalf("start var label = %q, want Person (first label)", l)
	}
	// A hop node's label resolves too.
	hop := []stageSpec{{pattern: &ast.Pattern{
		Start: ast.NodePat{Var: "a"},
		Hops:  []ast.PatternHop{{Node: ast.NodePat{Var: "b", Labels: []string{"Message"}}}},
	}}}
	if l := varLabelIn(hop, "b"); l != "Message" {
		t.Fatalf("hop var label = %q, want Message", l)
	}
	// A var present but unlabeled, and a var absent entirely, both yield "".
	if l := varLabelIn(hop, "a"); l != "" {
		t.Fatalf("unlabeled var = %q, want empty", l)
	}
	if l := varLabelIn(start, "z"); l != "" {
		t.Fatalf("absent var = %q, want empty", l)
	}
	// A nil-pattern spec is skipped, not dereferenced.
	withNil := []stageSpec{{pattern: nil}, start[0]}
	if l := varLabelIn(withNil, "x"); l != "Person" {
		t.Fatalf("nil-pattern spec must be skipped: got %q", l)
	}
}

// TestCondFanout covers the label-conditional fan-out sum: types with a
// conditional statistic add their average degree; unknown types contribute
// nothing; a request with no qualifying type reports the fall-back miss.
func TestCondFanout(t *testing.T) {
	g := buildFixture(t)
	// Message carries one HAS_CREATOR and one HAS_TAG per node, so the
	// conditional fan-out sums both average degrees.
	total, ok := condFanout("Message", []string{"HAS_CREATOR", "HAS_TAG"}, graph.Outgoing, g)
	if !ok || total <= 0 {
		t.Fatalf("Message fan-out = %v,%v, want positive", total, ok)
	}
	// A single known type contributes no more than the pair.
	one, ok := condFanout("Message", []string{"HAS_CREATOR"}, graph.Outgoing, g)
	if !ok || one <= 0 || one > total {
		t.Fatalf("single-type fan-out = %v,%v (pair %v)", one, ok, total)
	}
	// An unknown type on a known label carries a zero conditional statistic,
	// so the mix still resolves (from the known type's degree alone).
	mix, ok := condFanout("Message", []string{"HAS_CREATOR", "NOPE"}, graph.Outgoing, g)
	if !ok || mix != one {
		t.Fatalf("mixed fan-out = %v,%v, want %v", mix, ok, one)
	}
	// An unknown label has no conditional statistic at all -> ok=false, so
	// the caller falls back to the global fan-out.
	if _, ok := condFanout("NopeLabel", []string{"HAS_CREATOR"}, graph.Outgoing, g); ok {
		t.Fatal("unknown label must report ok=false")
	}
	// No types to sum -> ok=false as well.
	if _, ok := condFanout("Message", nil, graph.Outgoing, g); ok {
		t.Fatal("no types must report ok=false")
	}
}
