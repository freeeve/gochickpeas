// A correlated subquery's walk scaffolding (scope layout, matchers, the
// level-0 scan memo, evaluation scratch) is prepared once per execution and
// refreshed per outer row -- never rebuilt per row. Locks the invariant
// behind ctx.subqShapes: a per-row rebuild would cost tens of allocations
// per outer row (the Rust sibling measured 36/person before preparing
// once), so total allocations must not scale with the outer row count.
package gql

import (
	"fmt"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// knowsGraph builds n Persons in a ring where person i KNOWS i+1 and i+2.
func knowsGraph(t *testing.T, n int) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(n, 2*n)
	for i := 0; i < n; i++ {
		id, _ := b.AddNode("Person")
		_ = b.SetProp(id, "name", fmt.Sprintf("p%d", i))
	}
	for i := 0; i < n; i++ {
		for _, d := range []int{1, 2} {
			if _, err := b.AddRel(chickpeas.NodeID(i), chickpeas.NodeID((i+d)%n), "KNOWS"); err != nil {
				t.Fatal(err)
			}
		}
	}
	return b.Finalize("name")
}

// TestSubqueryScaffoldingPreparedOnce runs a correlated COUNT{} over every
// person, collapsing output to one row so the measured allocations are the
// subquery machinery plus fixed parse/plan cost. The bound of one allocation
// per outer row leaves room for incidental costs while failing loudly if the
// walk scaffolding is ever rebuilt per row (tens per row).
func TestSubqueryScaffoldingPreparedOnce(t *testing.T) {
	const n = 2000
	g := knowsGraph(t, n)
	q := "MATCH (p:Person) WHERE COUNT { MATCH (p)-[:KNOWS]->(f) } >= 2 RETURN count(*) AS c"
	// Correctness first: every person has exactly two outgoing KNOWS.
	rows, err := Run(g, q)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := rows.Next()
	v, _ := r.GetAt(0)
	if c, ok := v.AsInt(); !ok || c != n {
		t.Fatalf("count = %v, want %d", v, n)
	}
	allocs := testing.AllocsPerRun(3, func() {
		if _, err := Run(g, q); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > n {
		t.Fatalf("Run allocated %.0f objects for %d outer rows (>%d): correlated-subquery scaffolding is scaling per outer row", allocs, n, n)
	}
}
