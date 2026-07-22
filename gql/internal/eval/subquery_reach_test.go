package eval

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
)

// TestExistsReach covers the variable-length reachability walk used by
// EXISTS subqueries: from a start node it collects the nodes reachable over
// hop's rel type within [Min, Max] hops, includes the start itself when
// Min is 0, and dedups. Fixture: a directed chain 0 -R-> 1 -R-> 2 -R-> 3.
func TestExistsReach(t *testing.T) {
	b := chickpeas.NewBuilder(8, 8)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ids := make([]chickpeas.NodeID, 4)
	for i := range ids {
		n, err := b.AddNode("N")
		must(err)
		ids[i] = n
	}
	for i := 0; i < 3; i++ {
		_, err := b.AddRel(ids[i], ids[i+1], "R")
		must(err)
	}
	ctx := &Ctx{G: graph.New(b.Finalize("reach"))}

	// A shape whose only hop matches rel type R.
	shape := &subqueryShape{pattern: &ast.Pattern{
		Hops: []ast.PatternHop{{Rel: ast.RelPat{Types: []string{"R"}}}},
	}}
	u := func(x uint64) *uint64 { return &x }
	set := func(min, max *uint64) map[chickpeas.NodeID]bool {
		rel := &ast.RelPat{Dir: ast.DirOut, Types: []string{"R"}, Length: &ast.VarLength{Min: min, Max: max}}
		got := map[chickpeas.NodeID]bool{}
		for _, id := range shape.existsReach(ctx, ids[0], rel, 0, nil) {
			got[chickpeas.NodeID(id)] = true
		}
		return got
	}
	wants := func(got map[chickpeas.NodeID]bool, want ...int) bool {
		if len(got) != len(want) {
			return false
		}
		for _, i := range want {
			if !got[ids[i]] {
				return false
			}
		}
		return true
	}

	// {1,1}: exactly one hop from 0 -> {1}.
	if got := set(u(1), u(1)); !wants(got, 1) {
		t.Fatalf("{1,1} = %v, want {1}", got)
	}
	// {1,2}: one or two hops -> {1,2}.
	if got := set(u(1), u(2)); !wants(got, 1, 2) {
		t.Fatalf("{1,2} = %v, want {1,2}", got)
	}
	// {0,2}: Min 0 includes the start node -> {0,1,2}.
	if got := set(u(0), u(2)); !wants(got, 0, 1, 2) {
		t.Fatalf("{0,2} = %v, want {0,1,2}", got)
	}
	// {2,3}: at least two hops excludes node 1 -> {2,3}.
	if got := set(u(2), u(3)); !wants(got, 2, 3) {
		t.Fatalf("{2,3} = %v, want {2,3}", got)
	}
	// {2,} (unbounded max): two-plus hops -> {2,3}.
	if got := set(u(2), nil); !wants(got, 2, 3) {
		t.Fatalf("{2,} = %v, want {2,3}", got)
	}
}
