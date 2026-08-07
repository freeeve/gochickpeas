// Grouping-key rename regressions: expressions in an aggregating
// projection's wrapper must follow a key projected under a different name.
package gql_test

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql"
)

// TestGroupKeyRenameFollowsLabelTest pins the rename walk re-pointing a
// label test on a renamed grouping key (task 230): the wrapper's c:Company
// must follow `c AS comp` through aggregation instead of referencing a
// variable no post-grouping binder provides.
func TestGroupKeyRenameFollowsLabelTest(t *testing.T) {
	g := gql.SocialGraph(t)
	rows := gql.RunBoth(t, g,
		"MATCH (p:Person)-[:WORKS_AT]->(c) "+
			"RETURN c AS comp, count(*) + (CASE WHEN c:Company THEN 100 ELSE 0 END) AS x ORDER BY x")
	var got []int64
	for r := range rows.All() {
		v, _ := r.Get("x")
		i, ok := v.AsInt()
		if !ok {
			t.Fatalf("x not an int: %v", v)
		}
		got = append(got, i)
	}
	// Globex has one worker, Acme two; both carry the Company label.
	if len(got) != 2 || got[0] != 101 || got[1] != 102 {
		t.Fatalf("x column = %v, want [101 102]", got)
	}
}

// TestGroupKeyRenameReachesComprehensionBody pins that the rename walk
// descends into comprehension bodies (task 232): the filter's reference to
// the key follows `c AS comp` through aggregation even though the
// comprehension binds its own variable. Before the descent this was a
// spurious bind error.
func TestGroupKeyRenameReachesComprehensionBody(t *testing.T) {
	g := gql.SocialGraph(t)
	rows := gql.RunBoth(t, g,
		"MATCH (p:Person)-[:WORKS_AT]->(c) "+
			"RETURN c AS comp, count(*) + size([x IN [1,2] WHERE c.name = 'Acme' | x]) AS x ORDER BY x")
	var got []int64
	for r := range rows.All() {
		v, _ := r.Get("x")
		i, ok := v.AsInt()
		if !ok {
			t.Fatalf("x not an int: %v", v)
		}
		got = append(got, i)
	}
	// Globex: 1 worker + empty list; Acme: 2 workers + both elements.
	if len(got) != 2 || got[0] != 1 || got[1] != 4 {
		t.Fatalf("x column = %v, want [1 4]", got)
	}
}

// TestGroupKeyRenameAlphaRenamesCollidingLocal pins the capture guard
// (task 233): the comprehension local is named `comp` -- the key's output
// name -- while the body also references the key by its source name. The
// planner alpha-renames the local so the key substitution cannot be
// captured; the local still iterates the list elements.
func TestGroupKeyRenameAlphaRenamesCollidingLocal(t *testing.T) {
	g := gql.SocialGraph(t)
	rows := gql.RunBoth(t, g,
		"MATCH (p:Person)-[:WORKS_AT]->(c) "+
			"RETURN c AS comp, count(*) + size([comp IN [1,2] WHERE c.name = 'Acme' | comp]) AS x ORDER BY x")
	var got []int64
	for r := range rows.All() {
		v, _ := r.Get("x")
		i, ok := v.AsInt()
		if !ok {
			t.Fatalf("x not an int: %v", v)
		}
		got = append(got, i)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 4 {
		t.Fatalf("x column = %v, want [1 4]", got)
	}
}
