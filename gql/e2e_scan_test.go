// End-to-end execute tests for the M15 core: scans, WHERE pushdown,
// projection, DISTINCT, ORDER BY/OFFSET/LIMIT, UNION, params, and the GQL
// projection-boundary forms. Ports the single-node subset of the Rust
// engine's execute.rs (same fixture, same expected rows); expansion tests
// arrive with M17.
package gql_test

import (
	"errors"
	"github.com/freeeve/gochickpeas/gql"
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// strCol collects a string column, sorted.
func strCol(t *testing.T, g *chickpeas.Snapshot, q, col string) []string {
	t.Helper()
	rows := gql.RunBoth(t, g, q)
	var out []string
	for r := range rows.All() {
		v, ok := r.Get(col)
		if !ok {
			t.Fatalf("no column %q in %s", col, q)
		}
		s, ok := v.AsStr()
		if !ok {
			t.Fatalf("column %q not a string in %s: %v", col, q, v)
		}
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func TestScanFilterProject(t *testing.T) {
	g := gql.SocialGraph(t)
	gql.WantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE p.age > 30 RETURN p.name AS name", "name"),
		"Bob", "Carol")
}

func TestIndexedPropertyAnchor(t *testing.T) {
	g := gql.SocialGraph(t)
	rows, err := gql.Run(g, "MATCH (p:Person {name: 'Alice'}) RETURN p.age AS age")
	if err != nil {
		t.Fatal(err)
	}
	r, ok := rows.Next()
	if !ok {
		t.Fatal("no rows")
	}
	if v, _ := r.Get("age"); !value.Equal(v, value.Int(30)) {
		t.Fatalf("age = %v", v)
	}
	if _, more := rows.Next(); more {
		t.Fatal("expected a single row")
	}
}

func TestScanAllAndUnlabeled(t *testing.T) {
	g := gql.SocialGraph(t)
	gql.WantStrs(t, strCol(t, g, "MATCH (n) WHERE n.name = 'Acme' RETURN n.name AS name", "name"), "Acme")
	gql.WantStrs(t, strCol(t, g, "MATCH (n {name: 'Globex'}) RETURN n.name AS name", "name"), "Globex")
}

func TestIDSeekAndFunction(t *testing.T) {
	g := gql.SocialGraph(t)
	gql.WantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE id(p) = 2 RETURN p.name AS name", "name"), "Carol")
	// Out-of-space id matches nothing.
	if got := strCol(t, g, "MATCH (p:Person) WHERE id(p) = 99 RETURN p.name AS name", "name"); len(got) != 0 {
		t.Fatalf("id 99 = %v", got)
	}
}

func TestTextPredicateScan(t *testing.T) {
	g := gql.SocialGraph(t)
	gql.WantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE p.name STARTS WITH 'A' RETURN p.name AS name", "name"), "Alice")
	gql.WantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE p.name CONTAINS 'aro' RETURN p.name AS name", "name"), "Carol")
	gql.WantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE p.name ENDS WITH 'e' RETURN p.name AS name", "name"), "Alice", "Dave")
}

func TestWhereInListAndNullSemantics(t *testing.T) {
	g := gql.SocialGraph(t)
	gql.WantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE p.name IN ['Alice', 'Dave', 'Zed'] RETURN p.name AS name", "name"),
		"Alice", "Dave")
	// A miss over a null-element list is null -> the row filters out.
	if got := strCol(t, g, "MATCH (p:Person) WHERE p.age IN [null] RETURN p.name AS name", "name"); len(got) != 0 {
		t.Fatalf("null-list IN = %v", got)
	}
	// Mixed int/float membership coerces.
	gql.WantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE p.age IN [30.0, 35] RETURN p.name AS name", "name"),
		"Alice", "Bob")
}

func TestNotPrecedenceBelowComparison(t *testing.T) {
	g := gql.SocialGraph(t)
	// NOT binds looser than the comparison: NOT age > 30 == NOT (age > 30).
	gql.WantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE NOT p.age > 30 RETURN p.name AS name", "name"),
		"Alice", "Dave")
}

func TestIsNullOverOptionalProperty(t *testing.T) {
	g := gql.SocialGraph(t)
	gql.WantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE p.city IS NULL RETURN p.name AS name", "name"),
		"Bob", "Dave")
	gql.WantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE p.city IS NOT NULL RETURN p.name AS name", "name"),
		"Alice", "Carol")
}

func TestDistinctProjection(t *testing.T) {
	g := gql.SocialGraph(t)
	// Two people share no city value; joined years collapse via DISTINCT on
	// a computed key.
	got := gql.StrColOrdered(t, g, "MATCH (p:Person) RETURN DISTINCT toString(p.joined / 10000) AS y ORDER BY y", "y")
	gql.WantStrs(t, got, "2010", "2011", "2012", "2013")
	// DISTINCT over a repeated constant yields one row.
	rows, err := gql.Run(g, "MATCH (p:Person) RETURN DISTINCT 1 AS one")
	if err != nil {
		t.Fatal(err)
	}
	if got := rows.NextBatch(10); len(got) != 1 {
		t.Fatalf("distinct constant rows = %d", len(got))
	}
}

func TestOrderByOffsetLimit(t *testing.T) {
	g := gql.SocialGraph(t)
	gql.WantStrs(t, gql.StrColOrdered(t, g, "MATCH (p:Person) RETURN p.name AS name ORDER BY p.age", "name"),
		"Dave", "Alice", "Bob", "Carol")
	gql.WantStrs(t, gql.StrColOrdered(t, g, "MATCH (p:Person) RETURN p.name AS name ORDER BY p.age DESC", "name"),
		"Carol", "Bob", "Alice", "Dave")
	gql.WantStrs(t, gql.StrColOrdered(t, g, "MATCH (p:Person) RETURN p.name AS name ORDER BY p.age DESC LIMIT 2", "name"),
		"Carol", "Bob")
	gql.WantStrs(t, gql.StrColOrdered(t, g, "MATCH (p:Person) RETURN p.name AS name ORDER BY p.age OFFSET 1 LIMIT 2", "name"),
		"Alice", "Bob")
	// SKIP is accepted as a synonym.
	gql.WantStrs(t, gql.StrColOrdered(t, g, "MATCH (p:Person) RETURN p.name AS name ORDER BY p.age SKIP 3", "name"),
		"Carol")
	// Nulls sort last: city is absent for Bob and Dave.
	got := gql.StrColOrdered(t, g, "MATCH (p:Person) RETURN p.name AS name ORDER BY p.city, p.name", "name")
	gql.WantStrs(t, got, "Carol", "Alice", "Bob", "Dave")
	// Multi-key with a composite second key over an alias: age/10 tiers are
	// 2 (Dave), 3 (Alice, Bob -> name DESC), 4 (Carol).
	gql.WantStrs(t, gql.StrColOrdered(t, g,
		"MATCH (p:Person) RETURN p.name AS name, p.age AS age ORDER BY age / 10, name DESC", "name"),
		"Dave", "Bob", "Alice", "Carol")
}

func TestCrossProductAndCarriedScan(t *testing.T) {
	g := gql.SocialGraph(t)
	// Two scan ops in one MATCH: a filtered cross product.
	got := strCol(t, g,
		"MATCH (a:Person {name: 'Alice'}), (c:Company) RETURN c.name AS cn", "cn")
	gql.WantStrs(t, got, "Acme", "Globex")
	// A variable carried across a projection boundary re-binds via ScanArg.
	gql.WantStrs(t, strCol(t, g,
		"MATCH (p:Person) WHERE p.age > 30 RETURN p AS p NEXT MATCH (p) RETURN p.name AS name", "name"),
		"Bob", "Carol")
	// AND-conjuncts split and push to their earliest bound level: the a.age
	// conjunct prunes at level 0, the c.name conjunct at level 1, and the
	// cross-level comparison at the deepest slot it reads.
	gql.WantStrs(t, strCol(t, g,
		"MATCH (a:Person), (c:Company) WHERE a.age > 30 AND c.name STARTS WITH 'A' AND a.age > size(c.name) RETURN a.name AS an", "an"),
		"Bob", "Carol")
}

func TestGQLBoundaryForms(t *testing.T) {
	g := gql.SocialGraph(t)
	// LET + FILTER between MATCH and RETURN.
	gql.WantStrs(t, strCol(t, g,
		"MATCH (p:Person) LET a = p.age FILTER a > 30 RETURN p.name AS name", "name"),
		"Bob", "Carol")
	// RETURN ... NEXT projection boundary with a post-filter on an output
	// column.
	gql.WantStrs(t, strCol(t, g,
		"MATCH (p:Person) RETURN p.name AS name, p.age AS age NEXT FILTER age >= 35 RETURN name", "name"),
		"Bob", "Carol")
	// FOR expands a list into rows.
	rows := gql.RunBoth(t, g, "FOR x IN [1, 2, 3] RETURN x AS x ORDER BY x DESC")
	var xs []int64
	for r := range rows.All() {
		v, _ := r.Get("x")
		i, _ := v.AsInt()
		xs = append(xs, i)
	}
	if len(xs) != 3 || xs[0] != 3 || xs[2] != 1 {
		t.Fatalf("FOR rows = %v", xs)
	}
}

func TestExistsSubqueryInWhere(t *testing.T) {
	g := gql.SocialGraph(t)
	// EXISTS runs through the eval-side DFS, so it works before M17's
	// expand. Who KNOWS someone over 30: Alice->Bob/Carol, Bob->Carol,
	// Carol->Bob; Dave only knows Alice (30, not >30).
	gql.WantStrs(t, strCol(t, g,
		"MATCH (p:Person) WHERE EXISTS { MATCH (p)-[:KNOWS]->(q:Person) WHERE q.age > 30 } RETURN p.name AS name", "name"),
		"Alice", "Bob", "Carol")
	gql.WantStrs(t, strCol(t, g,
		"MATCH (p:Person) WHERE COUNT { MATCH (p)-[:KNOWS]->(q) } >= 2 RETURN p.name AS name", "name"),
		"Alice", "Bob", "Carol")
}

func TestUnionAndUnionAll(t *testing.T) {
	g := gql.SocialGraph(t)
	q := "MATCH (p:Person {name: 'Alice'}) RETURN p.name AS n UNION ALL MATCH (p:Person {name: 'Alice'}) RETURN p.name AS n"
	gql.WantStrs(t, gql.StrColOrdered(t, g, q, "n"), "Alice", "Alice")
	q = "MATCH (p:Person {name: 'Alice'}) RETURN p.name AS n UNION MATCH (p:Person {name: 'Alice'}) RETURN p.name AS n"
	gql.WantStrs(t, gql.StrColOrdered(t, g, q, "n"), "Alice")
	q = "MATCH (p:Person {name: 'Alice'}) RETURN p.name AS n UNION MATCH (c:Company) RETURN c.name AS n"
	gql.WantStrs(t, strCol(t, g, q, "n"), "Acme", "Alice", "Globex")
}

func TestNamedParams(t *testing.T) {
	g := gql.SocialGraph(t)
	rows, err := gql.RunWithParams(g, "MATCH (p:Person {name: $who}) RETURN p.age AS age",
		map[string]value.Value{"who": value.Str("Carol")})
	if err != nil {
		t.Fatal(err)
	}
	r, ok := rows.Next()
	if !ok {
		t.Fatal("no rows")
	}
	if v, _ := r.Get("age"); !value.Equal(v, value.Int(40)) {
		t.Fatalf("age = %v", v)
	}
	// An unsupplied parameter reads as null -> matches nothing.
	rows, err = gql.Run(g, "MATCH (p:Person) WHERE p.name = $who RETURN p.name AS name")
	if err != nil {
		t.Fatal(err)
	}
	if got := rows.NextBatch(10); len(got) != 0 {
		t.Fatalf("unsupplied param matched %d rows", len(got))
	}
}

func TestStarProjection(t *testing.T) {
	g := gql.SocialGraph(t)
	rows, err := gql.Run(g, "MATCH (p:Person {name: 'Alice'}) RETURN *")
	if err != nil {
		t.Fatal(err)
	}
	r, ok := rows.Next()
	if !ok {
		t.Fatal("no rows")
	}
	v, ok := r.Get("p")
	if !ok {
		t.Fatal("star projects p")
	}
	if n, _ := v.AsNode(); n != 0 {
		t.Fatalf("p = %v", v)
	}
}

func TestErrorKinds(t *testing.T) {
	g := gql.SocialGraph(t)
	if _, err := gql.Run(g, "MATCH (p:Person RETURN p"); !errors.Is(err, gql.ErrParse) {
		t.Fatalf("parse error kind: %v", err)
	}
	if _, err := gql.Run(g, "MATCH (p:Person) RETURN q.name AS n"); !errors.Is(err, gql.ErrBind) {
		t.Fatalf("bind error kind: %v", err)
	}
	if _, err := gql.Run(g, "MATCH (p:Person) RETURN nosuchfn(p) AS n"); !errors.Is(err, gql.ErrBind) {
		t.Fatalf("unknown function kind: %v", err)
	}
	// A variable has one element kind: cross-kind reuse within a
	// segment's patterns is a bind error in every direction (node->rel,
	// rel->node, node->path); same-kind reuse stays legal (tasks/058).
	if _, err := gql.Run(g, "MATCH (A:!A)-[A]-() RETURN 0"); !errors.Is(err, gql.ErrBind) {
		t.Fatalf("node var reused as rel: %v", err)
	}
	if _, err := gql.Run(g, "MATCH ()-[r]->() MATCH (r:Person) RETURN r"); !errors.Is(err, gql.ErrBind) {
		t.Fatalf("rel var reused as node: %v", err)
	}
	if _, err := gql.Run(g, "MATCH p = (a)-->() MATCH (p:Person) RETURN 0"); !errors.Is(err, gql.ErrBind) {
		t.Fatalf("path var reused as node: %v", err)
	}
	if _, err := gql.Run(g, "MATCH (a:Person)-->() MATCH (a)-->(b) RETURN b"); err != nil {
		t.Fatalf("same-kind node reuse should stay legal: %v", err)
	}
}

// TestExistsSeededScan pins the EXISTS-driven candidate scan (065): a
// broad scan whose WHERE is EXISTS (or an OR of EXISTSes) anchored at a
// bound variable must return exactly the scan-and-filter rows -- across
// forward and reversed pattern orientations, multi-hop chains, the OR
// union, a disqualifying mixed OR, and rows reachable only through
// nodes that fail interior labels (the superset must not lose them
// because the walk applies pattern labels the witness path satisfies).
func TestExistsSeededScan(t *testing.T) {
	b := chickpeas.NewBuilder(32, 64)
	tag, _ := b.AddNode("Tag")
	_ = b.SetProp(tag, "name", "t")
	mkPerson := func(name string) chickpeas.NodeID {
		p, _ := b.AddNode("Person")
		_ = b.SetProp(p, "name", name)
		return p
	}
	mkMsg := func(creator chickpeas.NodeID, tagged bool) chickpeas.NodeID {
		m, _ := b.AddNode("Message")
		b.AddRel(m, creator, "HAS_CREATOR")
		if tagged {
			b.AddRel(m, tag, "HAS_TAG")
		}
		return m
	}
	alice := mkPerson("alice") // authors a tagged message
	bob := mkPerson("bob")     // replies to a tagged message
	carol := mkPerson("carol") // untagged activity only
	dave := mkPerson("dave")   // no activity
	_ = dave
	mt := mkMsg(alice, true)
	mu := mkMsg(carol, false)
	reply := mkMsg(bob, false)
	b.AddRel(reply, mt, "REPLY_OF")
	_ = mu
	g := b.Finalize()

	// Forward orientation: person at the pattern start.
	gql.WantStrs(t, gql.StrColOrdered(t, g,
		"MATCH (t:Tag {name: 't'}) MATCH (p:Person) WHERE EXISTS { MATCH (p)<-[:HAS_CREATOR]-(m:Message)-[:HAS_TAG]->(t) } RETURN p.name AS n ORDER BY n", "n"),
		"alice")
	// Reversed orientation: person at the pattern end.
	gql.WantStrs(t, gql.StrColOrdered(t, g,
		"MATCH (t:Tag {name: 't'}) MATCH (p:Person) WHERE EXISTS { MATCH (t)<-[:HAS_TAG]-(m:Message)-[:HAS_CREATOR]->(p) } RETURN p.name AS n ORDER BY n", "n"),
		"alice")
	// OR of EXISTSes: author or replier.
	gql.WantStrs(t, gql.StrColOrdered(t, g,
		"MATCH (t:Tag {name: 't'}) MATCH (p:Person) WHERE EXISTS { MATCH (p)<-[:HAS_CREATOR]-(m:Message)-[:HAS_TAG]->(t) } OR EXISTS { MATCH (p)<-[:HAS_CREATOR]-(r:Message)-[:REPLY_OF]->(m2:Message)-[:HAS_TAG]->(t) } RETURN p.name AS n ORDER BY n", "n"),
		"alice", "bob")
	// Mixed OR (non-EXISTS leaf) must not lose the property-only row.
	gql.WantStrs(t, gql.StrColOrdered(t, g,
		"MATCH (t:Tag {name: 't'}) MATCH (p:Person) WHERE EXISTS { MATCH (p)<-[:HAS_CREATOR]-(m:Message)-[:HAS_TAG]->(t) } OR p.name = 'carol' RETURN p.name AS n ORDER BY n", "n"),
		"alice", "carol")
	// An unknown YIELD column is a typed plan error (algo.* yields
	// node/value, not score).
	if _, err := gql.Run(g, "CALL algo.pagerank() YIELD node, score RETURN score"); !errors.Is(err, gql.ErrPlan) {
		t.Fatalf("unknown YIELD column: %v", err)
	}
	// PROFILE executes and returns the annotated plan (M20).
	rows, err := gql.Run(g, "PROFILE MATCH (p:Person) RETURN p.name AS n")
	if err != nil {
		t.Fatalf("profile mode: %v", err)
	}
	if cols := rows.Columns(); len(cols) != 1 || cols[0] != "plan" {
		t.Fatalf("profile columns = %v", cols)
	}
}

func TestExplainModes(t *testing.T) {
	g := gql.SocialGraph(t)
	text, err := gql.Explain(g, "MATCH (p:Person {name: 'Alice'}) RETURN p.age AS age")
	if err != nil {
		t.Fatal(err)
	}
	if text == "" {
		t.Fatal("empty explain")
	}
	rows, err := gql.Run(g, "EXPLAIN MATCH (p:Person {name: 'Alice'}) RETURN p.age AS age")
	if err != nil {
		t.Fatal(err)
	}
	if got := rows.Columns(); len(got) != 1 || got[0] != "plan" {
		t.Fatalf("explain columns = %v", got)
	}
	if batch := rows.NextBatch(100); len(batch) == 0 {
		t.Fatal("explain emitted no plan rows")
	}
}

func TestOrderByProjectedExpressionKey(t *testing.T) {
	g := gql.SocialGraph(t)
	// The ORDER BY key is the projection expression itself (no alias use).
	gql.WantStrs(t, gql.StrColOrdered(t, g,
		"MATCH (p:Person) RETURN p.name AS name ORDER BY p.joined DESC LIMIT 1", "name"),
		"Dave")
}
