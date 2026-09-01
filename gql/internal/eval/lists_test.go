// List-scope source virtualization: the nodes(p)/rels(p) lazy views
// must answer every quantifier/comprehension/reduce exactly as the
// eager list evaluation does, fall back for non-path arguments, and
// allocate no intermediate list (the CR1-class per-path predicate
// cost: three boxed lists per path, one of which this removes).
package eval

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
)

func pathFixtureCtx(t *testing.T) (*Ctx, []value.Value, map[string]int) {
	t.Helper()
	b := chickpeas.NewBuilder(8, 8)
	var ids []chickpeas.NodeID
	for i := range 4 {
		n, _ := b.AddNode("P")
		_ = b.SetProp(n, "v", int64(10-i*3)) // 10, 7, 4, 1 -- strictly descending
		ids = append(ids, n)
	}
	var rels []uint32
	for i := 0; i < 3; i++ {
		r, err := b.AddRel(ids[i], ids[i+1], "K")
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SetRelPropAt(r, "w", int64(100-i)); err != nil { // 100, 99, 98
			t.Fatal(err)
		}
		rels = append(rels, uint32(r))
	}
	g := b.Finalize("paths")
	ctx := &Ctx{G: graph.New(g)}
	row := []value.Value{value.Path(ids, rels)}
	return ctx, row, map[string]int{"p": 0}
}

func TestPathListSourceVirtualization(t *testing.T) {
	ctx, row, slots := pathFixtureCtx(t)
	for _, tc := range []struct {
		src  string
		want value.Value
	}{
		// Quantifiers over rels(p)/nodes(p).
		{"all(r IN rels(p) WHERE r.w > 90)", value.Bool(true)},
		{"any(r IN rels(p) WHERE r.w = 99)", value.Bool(true)},
		{"none(r IN rels(p) WHERE r.w < 90)", value.Bool(true)},
		{"single(r IN rels(p) WHERE r.w = 100)", value.Bool(true)},
		{"all(n IN nodes(p) WHERE n.v >= 1)", value.Bool(true)},
		{"any(n IN nodes(p) WHERE n.v > 10)", value.Bool(false)},
		// Comprehensions read through the lazy view (reduce shares listSource).
		{"size([r IN rels(p) | r.w]) = 3", value.Bool(true)},
		{"[n IN nodes(p) WHERE n.v > 5 | n.v][1] = 7", value.Bool(true)},
		// The CR1 idiom end-to-end: pairwise-descending check over the
		// comprehension of rel timestamps.
		{"all(i IN range(0, size([r IN rels(p) | r.w]) - 2) WHERE [r IN rels(p) | r.w][i] > [r IN rels(p) | r.w][i + 1])", value.Bool(true)},
		// Non-path fallbacks keep the eager semantics.
		{"all(x IN [1, 2, 3] WHERE x > 0)", value.Bool(true)},
		{"size(rels(p)) = 3", value.Bool(true)},
	} {
		got := Eval(ctx, exprOf(t, tc.src), row, slots)
		if !value.Identical(got, tc.want) {
			t.Fatalf("%s = %v, want %v", tc.src, got, tc.want)
		}
	}
	// Null argument: quantifier over rels(null) is null, matching the
	// eager path's non-list decline.
	nullRow := []value.Value{value.Null()}
	if got := Eval(ctx, exprOf(t, "all(r IN rels(p) WHERE r.w > 0)"), nullRow, slots); !got.IsNull() {
		t.Fatalf("rels(null) quantifier = %v, want null", got)
	}
}

// TestPathListSourceAllocs pins the virtualization's point: a warm
// quantifier over rels(p) allocates nothing per evaluation (scope
// scratch is Ctx-cached; the lazy view boxes elements into a reused
// slot). The eager form allocated the rels list per call.
func TestPathListSourceAllocs(t *testing.T) {
	ctx, row, slots := pathFixtureCtx(t)
	e := exprOf(t, "all(r IN rels(p) WHERE r.w > 90)")
	Eval(ctx, e, row, slots) // warm the scope cache
	allocs := testing.AllocsPerRun(200, func() {
		if got := Eval(ctx, e, row, slots); !got.IsTruthy() {
			t.Fatal("predicate flipped")
		}
	})
	if allocs > 0 {
		t.Fatalf("warm rels(p) quantifier allocates %.1f/op, want 0 (the lazy view exists to remove the per-call list)", allocs)
	}
}
