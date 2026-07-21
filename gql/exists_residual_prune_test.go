// Differential guard for the "subquery residual WHERE" trap (task 210,
// ragedb cross-project note): when a correlated EXISTS/COUNT subquery
// carries a WHERE that does NOT reduce to a pushable literal scan filter
// -- an OR, a NOT, or a comparison against an outer variable -- the
// engine must still evaluate that residual against the subquery-bound
// node, not read an unfetched property as null and silently drop every
// row. gochickpeas reads properties on demand (no property-pruning pass),
// and EXISTS/COUNT run as real correlated subqueries, so the bug should
// not reproduce -- these sections pin that by EXECUTION (what the query
// returns), the only thing that catches it, per the note's meta-lesson.
package gql

import (
	"sort"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// residualFixture: four Works, each (but the last) with one Category whose
// uri varies; each Work carries a wuri for the cross-variable case.
//
//	W1 id1 wuri x  -> Category x
//	W2 id2 wuri q  -> Category y
//	W3 id3 wuri z  -> Category z
//	W4 id4 wuri n  -> (no category)
func residualFixture(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(16, 16)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	work := func(id int64, wuri, caturi string) {
		w, err := b.AddNode("Work")
		must(err)
		must(b.SetProp(w, "id", id))
		must(b.SetProp(w, "wuri", wuri))
		if caturi != "" {
			c, err := b.AddNode("Category")
			must(err)
			must(b.SetProp(c, "uri", caturi))
			_, err = b.AddRel(w, c, "CATEGORY")
			must(err)
		}
	}
	work(1, "x", "x")
	work(2, "q", "y")
	work(3, "z", "z")
	work(4, "n", "")
	return b.Finalize("residual")
}

// idsInOrder runs q and returns the int ids of its first column in
// RETURN order -- unsorted, so an ORDER BY under test is observable.
func idsInOrder(t *testing.T, g *chickpeas.Snapshot, q string) []int64 {
	t.Helper()
	rows, err := RunUncached(g, q)
	if err != nil {
		t.Fatalf("run %q: %v", q, err)
	}
	var out []int64
	for r := range rows.All() {
		v, _ := r.Values()[0].AsInt()
		out = append(out, v)
	}
	return out
}

// ids is idsInOrder sorted ascending -- for the EXISTS/COUNT sections,
// which assert set membership (each already carries ORDER BY w.id, but
// the sort makes the comparison order-independent regardless).
func ids(t *testing.T, g *chickpeas.Snapshot, q string) []int64 {
	out := idsInOrder(t, g, q)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func eqIDs(t *testing.T, label string, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %v, want %v", label, got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: got %v, want %v", label, got, want)
			return
		}
	}
}

// TestExistsResidualWhereNotPruned drives the note's exact check: a
// pushable equality (section 2) must NOT be the only spelling that works
// -- the unpushable NOT (3), OR (4), COUNT-with-NOT (5), and cross-var
// (6) sections must each return their correct NON-empty sets. A silent
// empty on 3/4/5/6 while 2 passes is the pruning bug.
func TestExistsResidualWhereNotPruned(t *testing.T) {
	g := residualFixture(t)

	// 1. Data control: every Work is present.
	eqIDs(t, "control", ids(t, g, `MATCH (w:Work) RETURN w.id ORDER BY w.id`),
		[]int64{1, 2, 3, 4})

	// 2. Pushable equality -- the spelling that hides the bug.
	eqIDs(t, "exists-eq", ids(t, g,
		`MATCH (w:Work) WHERE EXISTS { MATCH (w)-[:CATEGORY]->(c) WHERE c.uri = 'x' } RETURN w.id ORDER BY w.id`),
		[]int64{1})

	// 3. Unpushable NOT -- pins pruning specifically (not OR-specific).
	// Works with a category whose uri != 'x': W2(y), W3(z). W1's only
	// category is x (NOT false); W4 has none.
	eqIDs(t, "exists-not", ids(t, g,
		`MATCH (w:Work) WHERE EXISTS { MATCH (w)-[:CATEGORY]->(c) WHERE NOT (c.uri = 'x') } RETURN w.id ORDER BY w.id`),
		[]int64{2, 3})

	// 4. Unpushable OR: W1(x), W2(y).
	eqIDs(t, "exists-or", ids(t, g,
		`MATCH (w:Work) WHERE EXISTS { MATCH (w)-[:CATEGORY]->(c) WHERE c.uri = 'x' OR c.uri = 'y' } RETURN w.id ORDER BY w.id`),
		[]int64{1, 2})

	// 5. COUNT with an unpushable NOT (the decorrelation path): same set
	// as section 3.
	eqIDs(t, "count-not", ids(t, g,
		`MATCH (w:Work) WHERE COUNT { MATCH (w)-[:CATEGORY]->(c) WHERE NOT (c.uri = 'x') } > 0 RETURN w.id ORDER BY w.id`),
		[]int64{2, 3})

	// 6. Cross-variable comparison (subquery reads an OUTER property):
	// c.uri = w.wuri holds for W1(x=x) and W3(z=z).
	eqIDs(t, "exists-crossvar", ids(t, g,
		`MATCH (w:Work) WHERE EXISTS { MATCH (w)-[:CATEGORY]->(c) WHERE c.uri = w.wuri } RETURN w.id ORDER BY w.id`),
		[]int64{1, 3})
}

// TestOrderByNonAnchorVariable guards the sibling trap the same note
// names: ORDER BY a NON-start-node variable's property must sort by that
// property, not silently fall back to start-node-id order. The fixture
// reverses category uri order against work-id order, so the two orderings
// are distinguishable: ordering by c.uri yields works 3,2,1; the bug
// would yield 1,2,3 (the anchor w's id order).
func TestOrderByNonAnchorVariable(t *testing.T) {
	b := chickpeas.NewBuilder(16, 16)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	work := func(id int64, caturi string) {
		w, err := b.AddNode("Work")
		must(err)
		must(b.SetProp(w, "id", id))
		c, err := b.AddNode("Category")
		must(err)
		must(b.SetProp(c, "uri", caturi))
		_, err = b.AddRel(w, c, "CATEGORY")
		must(err)
	}
	work(1, "z")
	work(2, "y")
	work(3, "x")
	g := b.Finalize("orderby")

	// ORDER BY the non-anchor category's uri: x(3), y(2), z(1). Must
	// preserve RETURN order -- do not re-sort.
	got := idsInOrder(t, g, `MATCH (w:Work)-[:CATEGORY]->(c:Category) RETURN w.id, c.uri ORDER BY c.uri`)
	eqIDs(t, "order-by-nonanchor", got, []int64{3, 2, 1})
}
