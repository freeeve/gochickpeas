// Subquery patterns must honor node label EXPRESSIONS (task 245): the
// general `:A|B` / `:!C` form is lowered to a HasLabelExpr conjunct for
// MATCH stages by the planner, but subquery patterns compile their node
// matchers directly, so an unlowered expression was silently ignored --
// EXISTS over-matched and COUNT over-counted.
package gql_test

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql"
)

// subqueryLabelGraph: x1(:X) -R-> (:C), x2(:X) -R-> (:A).
func subqueryLabelGraph(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(8, 8)
	x1, _ := b.AddNode("X")
	x2, _ := b.AddNode("X")
	c, _ := b.AddNode("C")
	a, _ := b.AddNode("A")
	_ = b.SetProp(x1, "name", "x1")
	_ = b.SetProp(x2, "name", "x2")
	if _, err := b.AddRel(x1, c, "R"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddRel(x2, a, "R"); err != nil {
		t.Fatal(err)
	}
	return b.Finalize("name")
}

func TestExistsHonorsLabelExpression(t *testing.T) {
	g := subqueryLabelGraph(t)
	// Only x2's neighbor carries A or B; x1's neighbor is a plain C.
	gql.WantStrs(t, gql.StrColOrdered(t, g,
		"MATCH (x:X) WHERE EXISTS { (x)-[:R]->(:A|B) } RETURN x.name AS n ORDER BY n", "n"),
		"x2")
	// Negation form: only x1's neighbor is NOT an A.
	gql.WantStrs(t, gql.StrColOrdered(t, g,
		"MATCH (x:X) WHERE EXISTS { (x)-[:R]->(:!A) } RETURN x.name AS n ORDER BY n", "n"),
		"x1")
}

func TestCountSubHonorsLabelExpression(t *testing.T) {
	g := subqueryLabelGraph(t)
	rows := gql.RunBoth(t, g,
		"MATCH (x:X) RETURN x.name AS n, COUNT { (x)-[:R]->(:A|B) } AS c ORDER BY n")
	want := map[string]int64{"x1": 0, "x2": 1}
	seen := 0
	for r := range rows.All() {
		nv, _ := r.Get("n")
		cv, _ := r.Get("c")
		name, _ := nv.AsStr()
		cnt, _ := cv.AsInt()
		if want[name] != cnt {
			t.Fatalf("%s: COUNT = %d, want %d", name, cnt, want[name])
		}
		seen++
	}
	if seen != 2 {
		t.Fatalf("rows = %d, want 2", seen)
	}
}
