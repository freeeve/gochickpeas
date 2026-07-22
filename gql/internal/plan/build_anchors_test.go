package plan

import (
	"slices"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// TestSeedChainOf covers the EXISTS-seed chain recognizer: it builds an
// anchor->sv walk when the scan variable sits at one end of a fixed-hop
// pattern and a bound variable anchors the other end (forward when sv is the
// last node, reversed when sv is the start), and declines otherwise.
func TestSeedChainOf(t *testing.T) {
	slots := map[string]int{"b": 3, "u": 5}
	bound := map[int]bool{3: true} // b is bound; u is in scope but not bound
	rel := func(types ...string) ast.RelPat { return ast.RelPat{Dir: ast.DirOut, Types: types} }
	node := func(v string) ast.NodePat { return ast.NodePat{Var: v} }

	// sv at the END, bound anchor at the start -> forward walk.
	fwd := &ast.Pattern{Start: node("b"), Hops: []ast.PatternHop{{Rel: rel("R"), Node: node("a")}}}
	if ch := seedChainOf(fwd, "a", slots, bound); ch == nil || ch.AnchorSlot != 3 ||
		len(ch.Hops) != 1 || !slices.Equal(ch.Hops[0].Types, []string{"R"}) {
		t.Fatalf("forward chain = %+v", ch)
	}
	// Two-hop forward: (b)-[:R1]->(m)-[:R2]->(a).
	fwd2 := &ast.Pattern{Start: node("b"), Hops: []ast.PatternHop{
		{Rel: rel("R1"), Node: node("m")}, {Rel: rel("R2"), Node: node("a")}}}
	if ch := seedChainOf(fwd2, "a", slots, bound); ch == nil || ch.AnchorSlot != 3 ||
		len(ch.Hops) != 2 || !slices.Equal(ch.Hops[0].Types, []string{"R1"}) {
		t.Fatalf("two-hop forward = %+v", ch)
	}

	// sv at the START, bound anchor at the end -> reversed walk.
	rev := &ast.Pattern{Start: node("a"), Hops: []ast.PatternHop{{Rel: rel("R"), Node: node("b")}}}
	if ch := seedChainOf(rev, "a", slots, bound); ch == nil || ch.AnchorSlot != 3 || len(ch.Hops) != 1 {
		t.Fatalf("reversed chain = %+v", ch)
	}
	// Two-hop reversed exercises the land = prior-hop-node branch.
	rev2 := &ast.Pattern{Start: node("a"), Hops: []ast.PatternHop{
		{Rel: rel("R1"), Node: node("m")}, {Rel: rel("R2"), Node: node("b")}}}
	if ch := seedChainOf(rev2, "a", slots, bound); ch == nil || ch.AnchorSlot != 3 || len(ch.Hops) != 2 {
		t.Fatalf("two-hop reversed = %+v", ch)
	}

	// Declines: nil pattern, a single node (no hops), a quantified hop, an
	// unbound anchor at the other end, and sv at neither end.
	if seedChainOf(nil, "a", slots, bound) != nil {
		t.Fatal("nil pattern must decline")
	}
	if seedChainOf(&ast.Pattern{Start: node("a")}, "a", slots, bound) != nil {
		t.Fatal("a single node must decline")
	}
	quant := &ast.Pattern{Start: node("b"), Hops: []ast.PatternHop{
		{Rel: ast.RelPat{Dir: ast.DirOut, Types: []string{"R"}, Length: &ast.VarLength{}}, Node: node("a")}}}
	if seedChainOf(quant, "a", slots, bound) != nil {
		t.Fatal("a quantified hop must decline")
	}
	unbound := &ast.Pattern{Start: node("u"), Hops: []ast.PatternHop{{Rel: rel("R"), Node: node("a")}}}
	if seedChainOf(unbound, "a", slots, bound) != nil {
		t.Fatal("an unbound anchor must decline")
	}
	absent := &ast.Pattern{Start: node("b"), Hops: []ast.PatternHop{{Rel: rel("R"), Node: node("c")}}}
	if seedChainOf(absent, "a", slots, bound) != nil {
		t.Fatal("sv at neither end must decline")
	}
}
