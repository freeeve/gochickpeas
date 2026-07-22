package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
)

// TestNodeIDSeekValue covers the id-seek value resolution: a non-negative
// integer within the CSR id space resolves (including a sparse high id with
// no node present -- the id space, not the node count, is the bound), while
// a negative id, an out-of-space id, and a non-integer all decline.
func TestNodeIDSeekValue(t *testing.T) {
	// One node at id 10 -> the id space spans 0..10 with a single node, so
	// the space exceeds the node count.
	b := chickpeas.NewBuilder(16, 0)
	if _, err := b.AddNodeWithID(10, "N"); err != nil {
		t.Fatal(err)
	}
	sg := graph.New(b.Finalize("idseek"))
	ctx := &eval.Ctx{G: sg}
	space := int64(sg.IDSpace())
	if space <= 10 {
		t.Fatalf("id space = %d, want > 10", space)
	}

	// In-space ids resolve, including a sparse id with no node behind it
	// (existence is the caller's job; the seek only bounds-checks).
	for _, id := range []int64{0, 5, 10, space - 1} {
		got, ok := nodeIDSeekValue(ctx, value.Int(id))
		if !ok || got != graph.NodeID(id) {
			t.Fatalf("id %d = %d,%v, want %d,true", id, got, ok, id)
		}
	}

	// A negative id, an id at/beyond the space, and a non-integer decline.
	for _, v := range []value.Value{
		value.Int(-1),
		value.Int(space),
		value.Int(space + 1000),
		value.Str("7"),
		value.Float(3.0),
		value.Null(),
	} {
		if _, ok := nodeIDSeekValue(ctx, v); ok {
			t.Fatalf("%+v should not resolve to an id seek", v)
		}
	}
}
