// Decorrelation primitive parity: SubqueryGroupCount evaluates a correlated
// COUNT{} once with the group endpoint free and buckets the match count by
// the node it binds to. The invariant that makes the decorrelated form safe
// (task 084) is that, for every entity, the bucketed count equals the naive
// per-entity SubqueryCount over the same data -- asserted here directly, the
// decor-vs-per-row parity pattern rustychickpeas 088 recommended.
package eval

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
)

// decorFixture is the BI-Q8 shape in miniature: one tag, several persons, and
// Message nodes linking them via (tag)<-[:HAS_TAG]-(m:Message)-[:HAS_CREATOR]->
// (person). p1 authored two in-window tagged messages plus one out-of-window;
// p2 one in-window; p3 a message that is NOT tagged (must never appear). Each
// message carries a `ts` for the inner-WHERE window test.
func decorFixture(t *testing.T) (*Ctx, chickpeas.NodeID, [3]chickpeas.NodeID) {
	t.Helper()
	b := chickpeas.NewBuilder(32, 32)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	tag, err := b.AddNode("Tag")
	must(err)
	var persons [3]chickpeas.NodeID
	for i := range persons {
		persons[i], err = b.AddNode("Person")
		must(err)
	}
	// msg adds a Message with ts, tags it to `tag` when tagged, and credits it
	// to creator.
	msg := func(ts int64, tagged bool, creator chickpeas.NodeID) {
		m, err := b.AddNode("Message")
		must(err)
		must(b.SetProp(m, "ts", ts))
		if tagged {
			_, err = b.AddRel(m, tag, "HAS_TAG")
			must(err)
		}
		_, err = b.AddRel(m, creator, "HAS_CREATOR")
		must(err)
	}
	msg(150, true, persons[0])  // p1 in-window
	msg(160, true, persons[0])  // p1 in-window
	msg(999, true, persons[0])  // p1 out-of-window
	msg(170, true, persons[1])  // p2 in-window
	msg(150, false, persons[2]) // p3 untagged -> absent
	return &Ctx{G: graph.New(b.Finalize())}, tag, persons
}

// countSub parses the COUNT{} and returns its pattern and inner WHERE.
func countSub(t *testing.T, src string) (*ast.Pattern, ast.Expr) {
	t.Helper()
	e := exprOf(t, src)
	cs, ok := e.(*ast.CountSub)
	if !ok {
		t.Fatalf("want *ast.CountSub, got %T", e)
	}
	return cs.Pattern, cs.Where
}

func TestSubqueryGroupCountMatchesPerEntity(t *testing.T) {
	ctx, tag, persons := decorFixture(t)
	slots := map[string]int{"t": 0, "p": 1}

	for _, tc := range []struct {
		name string
		src  string
		want [3]int // expected count per person p1,p2,p3
	}{
		{
			name: "no inner where",
			src:  "COUNT { MATCH (t)<-[:HAS_TAG]-(m:Message)-[:HAS_CREATOR]->(p) }",
			want: [3]int{3, 1, 0},
		},
		{
			name: "inner where window",
			src:  "COUNT { MATCH (t)<-[:HAS_TAG]-(m:Message)-[:HAS_CREATOR]->(p) WHERE m.ts > 100 AND m.ts < 200 }",
			want: [3]int{2, 1, 0},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pat, where := countSub(t, tc.src)

			// Decorrelated: one pass anchored on the shared tag, bucketed by p.
			groupRow := []value.Value{value.Node(tag), value.Null()}
			table := SubqueryGroupCount(ctx, pat, where, groupRow, slots, "t", "p")

			for i, p := range persons {
				// Naive per-entity: SubqueryCount with both t and p bound.
				perRow := []value.Value{value.Node(tag), value.Node(p)}
				naive := SubqueryCount(ctx, pat, where, perRow, slots, false)
				if naive != tc.want[i] {
					t.Fatalf("p%d naive count = %d, want %d", i+1, naive, tc.want[i])
				}
				// The decorrelation invariant: bucketed count == per-entity count.
				if got := table[graph.NodeID(p)]; got != naive {
					t.Fatalf("p%d decorrelated count = %d, per-entity = %d (must match)", i+1, got, naive)
				}
			}
			// No spurious buckets (p3 authored an untagged message).
			if got := table[graph.NodeID(persons[2])]; got != 0 {
				t.Fatalf("p3 must not appear in the table, got %d", got)
			}
		})
	}
}
