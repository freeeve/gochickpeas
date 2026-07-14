// Decorrelation wiring (task 084): a COUNT{} over a linear chain with both
// endpoints bound to outer variables is answered from a per-anchor side table
// built once, not a DFS per outer row. These white-box tests assert the three
// properties that make that safe and worthwhile: the rewrite is recognized,
// its per-row answer equals the naive per-entity SubqueryCount, and the shared
// anchor's table is built exactly ONCE across many rows (the build-count
// invariant rustychickpeas 091 recommended over a timing assertion).
package compile

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
)

// decorGraph is the BI-Q8 shape in miniature: one hub tag, three persons, and
// Message nodes linking them via (tag)<-[:HAS_TAG]-(m:Message)-[:HAS_CREATOR]->
// (person), plus junk tagged messages so the tag is unambiguously the hub end
// (high HAS_TAG in-degree) and the persons the leaves. p1 authored two in-window
// tagged messages and one out-of-window; p2 one in-window; p3 an untagged one.
func decorGraph(t *testing.T) (*eval.Ctx, *chickpeas.Snapshot, chickpeas.NodeID, [3]chickpeas.NodeID) {
	t.Helper()
	b := chickpeas.NewBuilder(64, 128)
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
	msg(150, true, persons[0])
	msg(160, true, persons[0])
	msg(999, true, persons[0]) // out of window
	msg(170, true, persons[1])
	msg(150, false, persons[2]) // untagged -> absent
	// Junk tagged messages by junk creators: push the tag's HAS_TAG in-degree
	// well above any person's HAS_CREATOR in-degree so the tag is the hub.
	for range 20 {
		j, err := b.AddNode("Person")
		must(err)
		msg(150, true, j)
	}
	snap := b.Finalize("decor")
	return &eval.Ctx{G: graph.New(snap)}, snap, tag, persons
}

func TestDecorCountMatchesNaiveAndBuildsOnce(t *testing.T) {
	ctx, g, tag, persons := decorGraph(t)
	slots := map[string]int{"t": 0, "p": 1}
	src := "COUNT { MATCH (t)<-[:HAS_TAG]-(m:Message)-[:HAS_CREATOR]->(p) WHERE m.ts > 100 AND m.ts < 200 }"

	c := New(ctx, exprOf(t, src), slots, g)
	cs, ok := c.c.(*cSubquery)
	if !ok {
		t.Fatalf("want *cSubquery, got %T", c.c)
	}
	if !cs.decorOK {
		t.Fatal("COUNT{} over a both-endpoints-bound linear chain must be recognized as decorrelatable")
	}

	want := [3]int{2, 1, 0}
	// Eval every person, then re-eval the first: same tag anchor throughout.
	order := []int{0, 1, 2, 0}
	for _, i := range order {
		row := []value.Value{value.Node(tag), value.Node(persons[i])}
		got, _ := c.Eval(ctx, row, slots).AsInt()
		if got != int64(want[i]) {
			t.Fatalf("p%d decor count = %d, want %d", i+1, got, want[i])
		}
		// The decorrelated answer must equal the naive per-entity walk.
		naive := eval.SubqueryCount(ctx, cs.pattern, cs.where, row, slots, false)
		if got != int64(naive) {
			t.Fatalf("p%d decor count %d != naive %d", i+1, got, naive)
		}
	}
	// The hub tag anchors the scan (Start endpoint), so a single table serves
	// every row -- the build-count invariant.
	if cs.decorAnchorIsEnd {
		t.Fatal("anchor should be the hub tag (Start), not the leaf person (End)")
	}
	if cs.decorBuilds != 1 {
		t.Fatalf("side table built %d times, want 1 (one shared tag anchor)", cs.decorBuilds)
	}
}
