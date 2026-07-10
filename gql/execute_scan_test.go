// End-to-end execute tests for the M15 core: scans, WHERE pushdown,
// projection, DISTINCT, ORDER BY/OFFSET/LIMIT, UNION, params, and the GQL
// projection-boundary forms. Ports the single-node subset of the Rust
// engine's execute.rs (same fixture, same expected rows); expansion tests
// arrive with M17.
package gql

import (
	"errors"
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// socialGraph is the Rust execute.rs fixture: four Persons (name, age,
// joined YYYYMMDD, optional city), two Companies, KNOWS diamond + WORKS_AT.
func socialGraph(t testing.TB) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(8, 16)
	people := []struct {
		name   string
		age    int64
		joined int64
		city   string
	}{
		{"Alice", 30, 20100101, "NYC"},
		{"Bob", 35, 20120101, ""},
		{"Carol", 40, 20110615, "LA"},
		{"Dave", 25, 20130320, ""},
	}
	for _, p := range people {
		id, _ := b.AddNode("Person")
		_ = b.SetProp(id, "name", p.name)
		_ = b.SetProp(id, "age", p.age)
		_ = b.SetProp(id, "joined", p.joined)
		if p.city != "" {
			_ = b.SetProp(id, "city", p.city)
		}
	}
	for _, name := range []string{"Acme", "Globex"} {
		id, _ := b.AddNode("Company")
		_ = b.SetProp(id, "name", name)
	}
	for _, e := range [][2]chickpeas.NodeID{{0, 1}, {0, 2}, {1, 2}, {1, 3}, {2, 3}, {2, 1}, {3, 0}} {
		if _, err := b.AddRel(e[0], e[1], "KNOWS"); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range [][2]chickpeas.NodeID{{0, 4}, {1, 4}, {2, 5}} {
		if _, err := b.AddRel(e[0], e[1], "WORKS_AT"); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize("name", "age")
}

// runBoth executes q under the compiled and interpreted eval paths,
// asserts identical rows, and returns the compiled result -- the dual-path
// differential harness every end-to-end test runs through.
func runBoth(t *testing.T, g *chickpeas.Snapshot, q string) *Rows {
	t.Helper()
	compiled, err := Run(g, q)
	if err != nil {
		t.Fatalf("query failed: %s\n%v", q, err)
	}
	forceInterp = true
	interp, ierr := Run(g, q)
	forceInterp = false
	if ierr != nil {
		t.Fatalf("interpreted path failed where compiled succeeded: %s\n%v", q, ierr)
	}
	ci := *compiled
	for {
		cr, cok := ci.Next()
		ir, iok := interp.Next()
		if cok != iok {
			t.Fatalf("dual-path row-count divergence: %s", q)
		}
		if !cok {
			break
		}
		for i, cv := range cr.Values() {
			iv, _ := ir.GetAt(i)
			if value.Key(cv) != value.Key(iv) {
				t.Fatalf("dual-path divergence at %s col %d: compiled %v vs interpreted %v", q, i, cv, iv)
			}
		}
	}
	return compiled
}

// strCol collects a string column, sorted.
func strCol(t *testing.T, g *chickpeas.Snapshot, q, col string) []string {
	t.Helper()
	rows := runBoth(t, g, q)
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

// strColOrdered collects a string column preserving result order.
func strColOrdered(t *testing.T, g *chickpeas.Snapshot, q, col string) []string {
	t.Helper()
	rows := runBoth(t, g, q)
	var out []string
	for r := range rows.All() {
		v, _ := r.Get(col)
		s, _ := v.AsStr()
		out = append(out, s)
	}
	return out
}

func wantStrs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestScanFilterProject(t *testing.T) {
	g := socialGraph(t)
	wantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE p.age > 30 RETURN p.name AS name", "name"),
		"Bob", "Carol")
}

func TestIndexedPropertyAnchor(t *testing.T) {
	g := socialGraph(t)
	rows, err := Run(g, "MATCH (p:Person {name: 'Alice'}) RETURN p.age AS age")
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
	g := socialGraph(t)
	wantStrs(t, strCol(t, g, "MATCH (n) WHERE n.name = 'Acme' RETURN n.name AS name", "name"), "Acme")
	wantStrs(t, strCol(t, g, "MATCH (n {name: 'Globex'}) RETURN n.name AS name", "name"), "Globex")
}

func TestIDSeekAndFunction(t *testing.T) {
	g := socialGraph(t)
	wantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE id(p) = 2 RETURN p.name AS name", "name"), "Carol")
	// Out-of-space id matches nothing.
	if got := strCol(t, g, "MATCH (p:Person) WHERE id(p) = 99 RETURN p.name AS name", "name"); len(got) != 0 {
		t.Fatalf("id 99 = %v", got)
	}
}

func TestTextPredicateScan(t *testing.T) {
	g := socialGraph(t)
	wantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE p.name STARTS WITH 'A' RETURN p.name AS name", "name"), "Alice")
	wantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE p.name CONTAINS 'aro' RETURN p.name AS name", "name"), "Carol")
	wantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE p.name ENDS WITH 'e' RETURN p.name AS name", "name"), "Alice", "Dave")
}

func TestWhereInListAndNullSemantics(t *testing.T) {
	g := socialGraph(t)
	wantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE p.name IN ['Alice', 'Dave', 'Zed'] RETURN p.name AS name", "name"),
		"Alice", "Dave")
	// A miss over a null-element list is null -> the row filters out.
	if got := strCol(t, g, "MATCH (p:Person) WHERE p.age IN [null] RETURN p.name AS name", "name"); len(got) != 0 {
		t.Fatalf("null-list IN = %v", got)
	}
	// Mixed int/float membership coerces.
	wantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE p.age IN [30.0, 35] RETURN p.name AS name", "name"),
		"Alice", "Bob")
}

func TestNotPrecedenceBelowComparison(t *testing.T) {
	g := socialGraph(t)
	// NOT binds looser than the comparison: NOT age > 30 == NOT (age > 30).
	wantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE NOT p.age > 30 RETURN p.name AS name", "name"),
		"Alice", "Dave")
}

func TestIsNullOverOptionalProperty(t *testing.T) {
	g := socialGraph(t)
	wantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE p.city IS NULL RETURN p.name AS name", "name"),
		"Bob", "Dave")
	wantStrs(t, strCol(t, g, "MATCH (p:Person) WHERE p.city IS NOT NULL RETURN p.name AS name", "name"),
		"Alice", "Carol")
}

func TestDistinctProjection(t *testing.T) {
	g := socialGraph(t)
	// Two people share no city value; joined years collapse via DISTINCT on
	// a computed key.
	got := strColOrdered(t, g, "MATCH (p:Person) RETURN DISTINCT toString(p.joined / 10000) AS y ORDER BY y", "y")
	wantStrs(t, got, "2010", "2011", "2012", "2013")
	// DISTINCT over a repeated constant yields one row.
	rows, err := Run(g, "MATCH (p:Person) RETURN DISTINCT 1 AS one")
	if err != nil {
		t.Fatal(err)
	}
	if got := rows.NextBatch(10); len(got) != 1 {
		t.Fatalf("distinct constant rows = %d", len(got))
	}
}

func TestOrderByOffsetLimit(t *testing.T) {
	g := socialGraph(t)
	wantStrs(t, strColOrdered(t, g, "MATCH (p:Person) RETURN p.name AS name ORDER BY p.age", "name"),
		"Dave", "Alice", "Bob", "Carol")
	wantStrs(t, strColOrdered(t, g, "MATCH (p:Person) RETURN p.name AS name ORDER BY p.age DESC", "name"),
		"Carol", "Bob", "Alice", "Dave")
	wantStrs(t, strColOrdered(t, g, "MATCH (p:Person) RETURN p.name AS name ORDER BY p.age DESC LIMIT 2", "name"),
		"Carol", "Bob")
	wantStrs(t, strColOrdered(t, g, "MATCH (p:Person) RETURN p.name AS name ORDER BY p.age OFFSET 1 LIMIT 2", "name"),
		"Alice", "Bob")
	// SKIP is accepted as a synonym.
	wantStrs(t, strColOrdered(t, g, "MATCH (p:Person) RETURN p.name AS name ORDER BY p.age SKIP 3", "name"),
		"Carol")
	// Nulls sort last: city is absent for Bob and Dave.
	got := strColOrdered(t, g, "MATCH (p:Person) RETURN p.name AS name ORDER BY p.city, p.name", "name")
	wantStrs(t, got, "Carol", "Alice", "Bob", "Dave")
	// Multi-key with a composite second key over an alias: age/10 tiers are
	// 2 (Dave), 3 (Alice, Bob -> name DESC), 4 (Carol).
	wantStrs(t, strColOrdered(t, g,
		"MATCH (p:Person) RETURN p.name AS name, p.age AS age ORDER BY age / 10, name DESC", "name"),
		"Dave", "Bob", "Alice", "Carol")
}

func TestCrossProductAndCarriedScan(t *testing.T) {
	g := socialGraph(t)
	// Two scan ops in one MATCH: a filtered cross product.
	got := strCol(t, g,
		"MATCH (a:Person {name: 'Alice'}), (c:Company) RETURN c.name AS cn", "cn")
	wantStrs(t, got, "Acme", "Globex")
	// A variable carried across a projection boundary re-binds via ScanArg.
	wantStrs(t, strCol(t, g,
		"MATCH (p:Person) WHERE p.age > 30 RETURN p AS p NEXT MATCH (p) RETURN p.name AS name", "name"),
		"Bob", "Carol")
	// AND-conjuncts split and push to their earliest bound level: the a.age
	// conjunct prunes at level 0, the c.name conjunct at level 1, and the
	// cross-level comparison at the deepest slot it reads.
	wantStrs(t, strCol(t, g,
		"MATCH (a:Person), (c:Company) WHERE a.age > 30 AND c.name STARTS WITH 'A' AND a.age > size(c.name) RETURN a.name AS an", "an"),
		"Bob", "Carol")
}

func TestGQLBoundaryForms(t *testing.T) {
	g := socialGraph(t)
	// LET + FILTER between MATCH and RETURN.
	wantStrs(t, strCol(t, g,
		"MATCH (p:Person) LET a = p.age FILTER a > 30 RETURN p.name AS name", "name"),
		"Bob", "Carol")
	// RETURN ... NEXT projection boundary with a post-filter on an output
	// column.
	wantStrs(t, strCol(t, g,
		"MATCH (p:Person) RETURN p.name AS name, p.age AS age NEXT FILTER age >= 35 RETURN name", "name"),
		"Bob", "Carol")
	// FOR expands a list into rows.
	rows := runBoth(t, g, "FOR x IN [1, 2, 3] RETURN x AS x ORDER BY x DESC")
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
	g := socialGraph(t)
	// EXISTS runs through the eval-side DFS, so it works before M17's
	// expand. Who KNOWS someone over 30: Alice->Bob/Carol, Bob->Carol,
	// Carol->Bob; Dave only knows Alice (30, not >30).
	wantStrs(t, strCol(t, g,
		"MATCH (p:Person) WHERE EXISTS { MATCH (p)-[:KNOWS]->(q:Person) WHERE q.age > 30 } RETURN p.name AS name", "name"),
		"Alice", "Bob", "Carol")
	wantStrs(t, strCol(t, g,
		"MATCH (p:Person) WHERE COUNT { MATCH (p)-[:KNOWS]->(q) } >= 2 RETURN p.name AS name", "name"),
		"Alice", "Bob", "Carol")
}

func TestUnionAndUnionAll(t *testing.T) {
	g := socialGraph(t)
	q := "MATCH (p:Person {name: 'Alice'}) RETURN p.name AS n UNION ALL MATCH (p:Person {name: 'Alice'}) RETURN p.name AS n"
	wantStrs(t, strColOrdered(t, g, q, "n"), "Alice", "Alice")
	q = "MATCH (p:Person {name: 'Alice'}) RETURN p.name AS n UNION MATCH (p:Person {name: 'Alice'}) RETURN p.name AS n"
	wantStrs(t, strColOrdered(t, g, q, "n"), "Alice")
	q = "MATCH (p:Person {name: 'Alice'}) RETURN p.name AS n UNION MATCH (c:Company) RETURN c.name AS n"
	wantStrs(t, strCol(t, g, q, "n"), "Acme", "Alice", "Globex")
}

func TestNamedParams(t *testing.T) {
	g := socialGraph(t)
	rows, err := RunWithParams(g, "MATCH (p:Person {name: $who}) RETURN p.age AS age",
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
	rows, err = Run(g, "MATCH (p:Person) WHERE p.name = $who RETURN p.name AS name")
	if err != nil {
		t.Fatal(err)
	}
	if got := rows.NextBatch(10); len(got) != 0 {
		t.Fatalf("unsupplied param matched %d rows", len(got))
	}
}

func TestStarProjection(t *testing.T) {
	g := socialGraph(t)
	rows, err := Run(g, "MATCH (p:Person {name: 'Alice'}) RETURN *")
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
	g := socialGraph(t)
	if _, err := Run(g, "MATCH (p:Person RETURN p"); !errors.Is(err, ErrParse) {
		t.Fatalf("parse error kind: %v", err)
	}
	if _, err := Run(g, "MATCH (p:Person) RETURN q.name AS n"); !errors.Is(err, ErrBind) {
		t.Fatalf("bind error kind: %v", err)
	}
	if _, err := Run(g, "MATCH (p:Person) RETURN nosuchfn(p) AS n"); !errors.Is(err, ErrBind) {
		t.Fatalf("unknown function kind: %v", err)
	}
	// A variable has one element kind: cross-kind reuse within a
	// segment's patterns is a bind error in every direction (node->rel,
	// rel->node, node->path); same-kind reuse stays legal (tasks/058).
	if _, err := Run(g, "MATCH (A:!A)-[A]-() RETURN 0"); !errors.Is(err, ErrBind) {
		t.Fatalf("node var reused as rel: %v", err)
	}
	if _, err := Run(g, "MATCH ()-[r]->() MATCH (r:Person) RETURN r"); !errors.Is(err, ErrBind) {
		t.Fatalf("rel var reused as node: %v", err)
	}
	if _, err := Run(g, "MATCH p = (a)-->() MATCH (p:Person) RETURN 0"); !errors.Is(err, ErrBind) {
		t.Fatalf("path var reused as node: %v", err)
	}
	if _, err := Run(g, "MATCH (a:Person)-->() MATCH (a)-->(b) RETURN b"); err != nil {
		t.Fatalf("same-kind node reuse should stay legal: %v", err)
	}
	// An unknown YIELD column is a typed plan error (algo.* yields
	// node/value, not score).
	if _, err := Run(g, "CALL algo.pagerank() YIELD node, score RETURN score"); !errors.Is(err, ErrPlan) {
		t.Fatalf("unknown YIELD column: %v", err)
	}
	// PROFILE executes and returns the annotated plan (M20).
	rows, err := Run(g, "PROFILE MATCH (p:Person) RETURN p.name AS n")
	if err != nil {
		t.Fatalf("profile mode: %v", err)
	}
	if cols := rows.Columns(); len(cols) != 1 || cols[0] != "plan" {
		t.Fatalf("profile columns = %v", cols)
	}
}

func TestExplainModes(t *testing.T) {
	g := socialGraph(t)
	text, err := Explain(g, "MATCH (p:Person {name: 'Alice'}) RETURN p.age AS age")
	if err != nil {
		t.Fatal(err)
	}
	if text == "" {
		t.Fatal("empty explain")
	}
	rows, err := Run(g, "EXPLAIN MATCH (p:Person {name: 'Alice'}) RETURN p.age AS age")
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
	g := socialGraph(t)
	// The ORDER BY key is the projection expression itself (no alias use).
	wantStrs(t, strColOrdered(t, g,
		"MATCH (p:Person) RETURN p.name AS name ORDER BY p.joined DESC LIMIT 1", "name"),
		"Dave")
}
