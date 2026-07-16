// COLLECT { [MATCH] pattern [WHERE] RETURN proj } -- the list-valued
// subquery, sibling of COUNT { } (integer) and EXISTS { } (boolean). It
// parses into the engine's pattern-comprehension node, so it gathers proj
// over every correlated match into a list in match order, an empty list when
// nothing matches (task 182).
package gql

import (
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// collectGraph: Persons a,b,c,d with KNOWS a->b, a->c, b->c; c and d know
// nobody (so their COLLECT yields an empty list, and the collect() aggregate
// drops them entirely).
func collectGraph(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(4, 8)
	for _, n := range []string{"a", "b", "c", "d"} {
		id, _ := b.AddNode("Person")
		_ = b.SetProp(id, "name", n)
	}
	for _, e := range [][2]chickpeas.NodeID{{0, 1}, {0, 2}, {1, 2}} {
		if _, err := b.AddRel(e[0], e[1], "KNOWS"); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize("name")
}

// nameToFriends runs q (which must RETURN a string column "n" and a list
// column "friends") and returns name -> sorted friend names.
func nameToFriends(t *testing.T, g *chickpeas.Snapshot, q string) map[string][]string {
	t.Helper()
	rows := runBoth(t, g, q)
	out := map[string][]string{}
	for r := range rows.All() {
		nv, _ := r.Get("n")
		n, ok := nv.AsStr()
		if !ok {
			t.Fatalf("column n not a string: %v", nv)
		}
		fv, _ := r.Get("friends")
		list, ok := fv.AsList()
		if !ok {
			t.Fatalf("column friends not a list: %v", fv)
		}
		fs := []string{}
		for _, e := range list {
			s, ok := e.AsStr()
			if !ok {
				t.Fatalf("friend element not a string: %v", e)
			}
			fs = append(fs, s)
		}
		slices.Sort(fs)
		out[n] = fs
	}
	return out
}

// TestCollectSubqueryGathers pins the core behavior: a per-row list of the
// correlated projection, an empty list (not absent, not null) when the inner
// pattern has no match.
func TestCollectSubqueryGathers(t *testing.T) {
	g := collectGraph(t)
	got := nameToFriends(t, g,
		"MATCH (p:Person) RETURN p.name AS n, COLLECT { MATCH (p)-[:KNOWS]->(f) RETURN f.name } AS friends")
	want := map[string][]string{
		"a": {"b", "c"},
		"b": {"c"},
		"c": {},
		"d": {},
	}
	if len(got) != len(want) {
		t.Fatalf("row count = %d, want %d (%v)", len(got), len(want), got)
	}
	for n, w := range want {
		if !slices.Equal(got[n], w) {
			t.Fatalf("friends[%s] = %v, want %v", n, got[n], w)
		}
	}
}

// TestCollectSubqueryMatchesAggregate pins that COLLECT { } equals the
// collect() aggregate over the same pattern for every row that has a match
// (the aggregate simply drops the no-match rows COLLECT keeps as empty).
func TestCollectSubqueryMatchesAggregate(t *testing.T) {
	g := collectGraph(t)
	sub := nameToFriends(t, g,
		"MATCH (p:Person) RETURN p.name AS n, COLLECT { MATCH (p)-[:KNOWS]->(f) RETURN f.name } AS friends")
	agg := nameToFriends(t, g,
		"MATCH (p:Person)-[:KNOWS]->(f) RETURN p.name AS n, collect(f.name) AS friends")
	for n, a := range agg {
		if !slices.Equal(sub[n], a) {
			t.Fatalf("COLLECT{}[%s] = %v, collect() = %v", n, sub[n], a)
		}
	}
	// The aggregate drops c and d; COLLECT keeps them as empty lists.
	if _, ok := agg["c"]; ok {
		t.Fatal("aggregate unexpectedly kept c")
	}
	if got := sub["c"]; len(got) != 0 {
		t.Fatalf("COLLECT{}[c] = %v, want empty list", got)
	}
}

// TestCollectSubqueryInnerWhereAndOptionalMatch pins that an inner WHERE
// filters the gathered values and that the MATCH keyword is optional.
func TestCollectSubqueryInnerWhereAndOptionalMatch(t *testing.T) {
	g := collectGraph(t)
	withWhere := nameToFriends(t, g,
		"MATCH (p:Person) RETURN p.name AS n, COLLECT { MATCH (p)-[:KNOWS]->(f) WHERE f.name <> 'c' RETURN f.name } AS friends")
	if !slices.Equal(withWhere["a"], []string{"b"}) {
		t.Fatalf("inner WHERE friends[a] = %v, want [b]", withWhere["a"])
	}
	if len(withWhere["b"]) != 0 {
		t.Fatalf("inner WHERE friends[b] = %v, want empty", withWhere["b"])
	}
	// The MATCH keyword is optional (bare braced pattern) and an AS alias on
	// the projection is accepted and ignored.
	bare := nameToFriends(t, g,
		"MATCH (p:Person) RETURN p.name AS n, COLLECT { (p)-[:KNOWS]->(f) RETURN f.name AS x } AS friends")
	if !slices.Equal(bare["a"], []string{"b", "c"}) {
		t.Fatalf("bare-pattern friends[a] = %v, want [b c]", bare["a"])
	}
}

// TestCollectSubqueryRejects pins the documented limitations as clean parse
// errors, not silent mishandling: RETURN is required, and a nested body clause
// (WITH / ORDER BY) is rejected.
func TestCollectSubqueryRejects(t *testing.T) {
	g := collectGraph(t)
	rejects := []string{
		"MATCH (p:Person) RETURN COLLECT { MATCH (p)-[:KNOWS]->(f) } AS x",
		"MATCH (p:Person) RETURN COLLECT { MATCH (p)-[:KNOWS]->(f) RETURN f.name ORDER BY f.name } AS x",
		"MATCH (p:Person) RETURN COLLECT { MATCH (p)-[:KNOWS]->(f) WITH f RETURN f.name } AS x",
	}
	for _, q := range rejects {
		if _, err := Run(g, q); err == nil {
			t.Fatalf("expected a parse error, got success: %s", q)
		}
	}
}

// TestCollectAggregateStillParses pins that collect(...) the aggregate is
// unaffected by the COLLECT { } subquery form (the brace vs paren
// disambiguation).
func TestCollectAggregateStillParses(t *testing.T) {
	g := collectGraph(t)
	agg := nameToFriends(t, g,
		"MATCH (p:Person)-[:KNOWS]->(f) RETURN p.name AS n, collect(f.name) AS friends")
	if !slices.Equal(agg["a"], []string{"b", "c"}) {
		t.Fatalf("collect() aggregate friends[a] = %v, want [b c]", agg["a"])
	}
}
