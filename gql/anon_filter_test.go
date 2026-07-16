// Regression for the anonymous-destination inline-property-filter prune bug
// reported cross-project from ragedb (gochickpeas task 174): a pattern whose
// destination is anonymous but carries an inline `{prop: v}` filter --
// `(a)-[:R]->(:Label {prop: v})` -- must return the same rows as the named
// form `(a)-[:R]->(x:Label {prop: v})`. If a property pruner keyed on query
// variables strips the destination's properties before the node's own inline
// filter runs (because the anonymous element has only an internal name), the
// filter matches nothing and the result is a silent empty set.
package gql

import (
	"sort"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// namesParam runs a one-parameter query and returns its sorted first column as
// strings -- the parameterized-filter analogue of strCol.
func namesParam(t *testing.T, g *chickpeas.Snapshot, q, p string) []string {
	t.Helper()
	rows, err := RunWithParams(g, q, map[string]value.Value{"p": value.Str(p)})
	if err != nil {
		t.Fatalf("param query failed: %s\n%v", q, err)
	}
	var out []string
	for {
		r, ok := rows.Next()
		if !ok {
			break
		}
		v, _ := r.GetAt(0)
		if s, ok := v.AsStr(); ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func TestAnonDestInlinePropFilter(t *testing.T) {
	g := socialGraph(t)
	const named = "MATCH (p:Person)-[:WORKS_AT]->(c:Company {name: 'Acme'}) RETURN p.name AS name"
	const anon = "MATCH (p:Person)-[:WORKS_AT]->(:Company {name: 'Acme'}) RETURN p.name AS name"
	// Control: named destination keeps its props, filter runs, {Alice, Bob}.
	wantStrs(t, strCol(t, g, named, "name"), "Alice", "Bob")
	// The bug shape: anonymous destination must match identically, not 0 rows.
	wantStrs(t, strCol(t, g, anon, "name"), "Alice", "Bob")

	// Non-indexed, filter-only property on the anonymous element -- the case
	// most likely to trip a pruner, since `city` is neither indexed nor
	// projected, so nothing but the inline filter needs it. Carol is the only
	// LA resident; Alice and Bob KNOW her.
	const namedCity = "MATCH (p:Person)-[:KNOWS]->(c:Person {city: 'LA'}) RETURN p.name AS name"
	const anonCity = "MATCH (p:Person)-[:KNOWS]->(:Person {city: 'LA'}) RETURN p.name AS name"
	wantStrs(t, strCol(t, g, namedCity, "name"), "Alice", "Bob")
	wantStrs(t, strCol(t, g, anonCity, "name"), "Alice", "Bob")

	// Parameterized (non-literal) inline filter on the anonymous destination --
	// the actual LDBC IC6 shape (post)-[:HAS_TAG]->(:Tag {name: $p}). This is
	// the form that desugars to a synthetic-named filter conjunct, the variant
	// closest to the reported bug's mechanism (ragedb 054 / rustychickpeas 363).
	const anonParam = "MATCH (p:Person)-[:WORKS_AT]->(:Company {name: $p}) RETURN p.name AS name"
	wantStrs(t, namesParam(t, g, anonParam, "Acme"), "Alice", "Bob")
	// Discriminating: a different param value selects a different subset, so the
	// filter genuinely reads and applies the anonymous destination's property
	// rather than passing everything through.
	wantStrs(t, namesParam(t, g, anonParam, "Globex"), "Carol")
}
