// M18 tests: aggregation (implicit grouping, DISTINCT, the zeroed keyless
// row, nested-aggregate wrappers), FOR row expansion, and CALL { }
// subqueries -- the Rust execute.rs aggregation subset translated to GQL
// under the dual-path harness.
package gql

import (
	"errors"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// intCol collects an integer column preserving result order.
func intCol(t *testing.T, g *chickpeas.Snapshot, q, col string) []int64 {
	t.Helper()
	rows := runBoth(t, g, q)
	var out []int64
	for r := range rows.All() {
		v, ok := r.Get(col)
		if !ok {
			t.Fatalf("no column %q in %s", col, q)
		}
		i, ok := v.AsInt()
		if !ok {
			t.Fatalf("column %q not an int in %s: %v", col, q, v)
		}
		out = append(out, i)
	}
	return out
}

func TestCountStarAndGrouping(t *testing.T) {
	g := socialGraph(t)
	if got := intCol(t, g, "MATCH (p:Person) RETURN count(*) AS n", "n"); len(got) != 1 || got[0] != 4 {
		t.Fatalf("count(*) = %v", got)
	}
	// Implicit group by the non-aggregate key, ordered by the count.
	rows := runBoth(t, g,
		"MATCH (c:Company)<-[:WORKS_AT]-(p:Person) RETURN c.name AS company, count(*) AS n ORDER BY n DESC")
	type pair struct {
		c string
		n int64
	}
	var got []pair
	for r := range rows.All() {
		cv, _ := r.Get("company")
		c, _ := cv.AsStr()
		nv, _ := r.Get("n")
		n, _ := nv.AsInt()
		got = append(got, pair{c, n})
	}
	if len(got) != 2 || got[0] != (pair{"Acme", 2}) || got[1] != (pair{"Globex", 1}) {
		t.Fatalf("grouped counts = %v", got)
	}
}

func TestCountDistinctVsCountStar(t *testing.T) {
	g := socialGraph(t)
	// Two hops from Alice reach Carol,Dave (via Bob) and Dave,Bob (via
	// Carol): 4 rows, 3 distinct endpoints.
	if got := intCol(t, g,
		"MATCH (a:Person {name: 'Alice'})-[:KNOWS]->()-[:KNOWS]->(f) RETURN count(*) AS n", "n"); got[0] != 4 {
		t.Fatalf("count(*) = %v", got)
	}
	if got := intCol(t, g,
		"MATCH (a:Person {name: 'Alice'})-[:KNOWS]->()-[:KNOWS]->(f) RETURN count(DISTINCT f) AS n", "n"); got[0] != 3 {
		t.Fatalf("count(DISTINCT f) = %v", got)
	}
}

func TestNumericAggregatesAndCollect(t *testing.T) {
	g := socialGraph(t)
	rows := runBoth(t, g,
		"MATCH (p:Person) RETURN sum(p.age) AS s, avg(p.age) AS a, min(p.age) AS lo, max(p.age) AS hi, count(p.city) AS cities")
	r, _ := rows.Next()
	if v, _ := r.Get("s"); !value.Equal(v, value.Int(130)) {
		t.Fatalf("sum = %v", v)
	}
	if v, _ := r.Get("a"); !value.Equal(v, value.Float(32.5)) {
		t.Fatalf("avg = %v", v)
	}
	if v, _ := r.Get("lo"); !value.Equal(v, value.Int(25)) {
		t.Fatalf("min = %v", v)
	}
	if v, _ := r.Get("hi"); !value.Equal(v, value.Int(40)) {
		t.Fatalf("max = %v", v)
	}
	// count(expr) skips nulls: only two people carry a city.
	if v, _ := r.Get("cities"); !value.Equal(v, value.Int(2)) {
		t.Fatalf("count(city) = %v", v)
	}
	// A float mixed into sum floats the result; collect drops nulls.
	rows = runBoth(t, g,
		"MATCH (p:Person) RETURN sum(p.age * 0.5) AS s, size(collect(p.city)) AS cs")
	r, _ = rows.Next()
	if v, _ := r.Get("s"); !value.Equal(v, value.Float(65.0)) {
		t.Fatalf("float sum = %v", v)
	}
	if v, _ := r.Get("cs"); !value.Equal(v, value.Int(2)) {
		t.Fatalf("collect size = %v", v)
	}
}

func TestAggregateOverEmptyMatch(t *testing.T) {
	g := socialGraph(t)
	// Keyless: one zeroed row even over no input.
	rows := runBoth(t, g, "MATCH (p:Person {name: 'Zed'}) RETURN count(*) AS n, sum(p.age) AS s, min(p.age) AS lo")
	r, ok := rows.Next()
	if !ok {
		t.Fatal("keyless aggregate over empty match must emit one row")
	}
	if v, _ := r.Get("n"); !value.Equal(v, value.Int(0)) {
		t.Fatalf("count = %v", v)
	}
	if v, _ := r.Get("s"); !value.Equal(v, value.Int(0)) {
		t.Fatalf("empty sum = %v", v)
	}
	if v, _ := r.Get("lo"); !v.IsNull() {
		t.Fatalf("empty min = %v", v)
	}
	// Keyed: no groups, no rows.
	rows = runBoth(t, g, "MATCH (p:Person {name: 'Zed'}) RETURN p.name AS name, count(*) AS n")
	if _, ok := rows.Next(); ok {
		t.Fatal("keyed aggregate over empty match emits nothing")
	}
}

func TestAggregateHavingBoundary(t *testing.T) {
	g := socialGraph(t)
	// The HAVING idiom: aggregate, then FILTER the projected count.
	wantStrs(t, strCol(t, g,
		"MATCH (c:Company)<-[:WORKS_AT]-(p:Person) RETURN c.name AS name, count(*) AS n NEXT FILTER n > 1 RETURN name", "name"),
		"Acme")
}

func TestCaseInsideSum(t *testing.T) {
	g := socialGraph(t)
	if got := intCol(t, g,
		"MATCH (p:Person) RETURN sum(CASE WHEN p.age > 30 THEN 1 ELSE 0 END) AS n", "n"); got[0] != 2 {
		t.Fatalf("conditional sum = %v", got)
	}
}

func TestForOverCollectedList(t *testing.T) {
	g := socialGraph(t)
	// collect into a boundary, then FOR back into rows.
	wantStrs(t, strCol(t, g,
		"MATCH (p:Person) WHERE p.age > 30 RETURN collect(p.name) AS names NEXT FOR n IN names RETURN n AS n", "n"),
		"Bob", "Carol")
	// FOR over null emits nothing; over a scalar emits one row.
	rows := runBoth(t, g, "MATCH (p:Person {name: 'Dave'}) FOR x IN p.city RETURN x AS x")
	if _, ok := rows.Next(); ok {
		t.Fatal("FOR over null emits nothing")
	}
	rows = runBoth(t, g, "MATCH (p:Person {name: 'Alice'}) FOR x IN p.age RETURN x AS x")
	r, ok := rows.Next()
	if !ok {
		t.Fatal("FOR over a scalar emits one row")
	}
	if v, _ := r.Get("x"); !value.Equal(v, value.Int(30)) {
		t.Fatalf("scalar FOR = %v", v)
	}
}

func TestCallSubqueryCorrelated(t *testing.T) {
	g := socialGraph(t)
	rows := runBoth(t, g,
		"MATCH (p:Person) CALL (p) { MATCH (p)-[:KNOWS]->(f) RETURN count(f) AS friends } RETURN p.name AS name, friends ORDER BY name")
	want := map[string]int64{"Alice": 2, "Bob": 2, "Carol": 2, "Dave": 1}
	n := 0
	for r := range rows.All() {
		n++
		nv, _ := r.Get("name")
		name, _ := nv.AsStr()
		fv, _ := r.Get("friends")
		f, _ := fv.AsInt()
		if f != want[name] {
			t.Fatalf("%s friends = %d, want %d", name, f, want[name])
		}
	}
	if n != 4 {
		t.Fatalf("rows = %d", n)
	}
}

func TestCallSubqueryUncorrelatedCrossJoin(t *testing.T) {
	g := socialGraph(t)
	rows := runBoth(t, g,
		"MATCH (c:Company) CALL { MATCH (p:Person {name: 'Alice'}) RETURN p.name AS a } RETURN c.name AS cn, a ORDER BY cn")
	batch := rows.NextBatch(10)
	if len(batch) != 2 {
		t.Fatalf("cross-join rows = %d", len(batch))
	}
	for _, r := range batch {
		if v, _ := r.Get("a"); func() string { s, _ := v.AsStr(); return s }() != "Alice" {
			t.Fatalf("a = %v", v)
		}
	}
	// A non-matching inner subquery drops the outer row (inner join).
	rows = runBoth(t, g,
		"MATCH (c:Company) CALL { MATCH (p:Person {name: 'Zed'}) RETURN p.name AS a } RETURN c.name AS cn")
	if _, ok := rows.Next(); ok {
		t.Fatal("empty subquery drops outer rows")
	}
}

func TestGroupKeyInsideNestedAggWrapper(t *testing.T) {
	g := socialGraph(t)
	// A carried list concatenated with a fresh collect (LDBC Q8's
	// `interestedPersons + collect(person)`): the grouping key `ps` is a
	// legal reference inside the wrapper.
	rows := runBoth(t, g,
		"MATCH (p:Person) RETURN collect(p.name) AS ps "+
			"NEXT MATCH (c:Company) RETURN ps, size(ps + collect(c.name)) AS n")
	r, ok := rows.Next()
	if !ok {
		t.Fatal("no row")
	}
	if v, _ := r.Get("n"); !value.Equal(v, value.Int(6)) {
		t.Fatalf("concat size = %v, want 6 (4 people + 2 companies)", v)
	}
	// A group key projected under a different alias re-points property
	// access in the wrapper at the output column.
	rows = runBoth(t, g,
		"MATCH (c:Company)<-[:WORKS_AT]-(p:Person) RETURN c AS firm, c.name + toString(count(*)) AS tag ORDER BY tag")
	var tags []string
	for r := range rows.All() {
		v, _ := r.Get("tag")
		s, _ := v.AsStr()
		tags = append(tags, s)
	}
	if len(tags) != 2 || tags[0] != "Acme2" || tags[1] != "Globex1" {
		t.Fatalf("tags = %v", tags)
	}
	// A reference that is neither a group key nor inside an aggregate is
	// still a bind error.
	if _, err := Run(g, "MATCH (p:Person) RETURN collect(p.name) AS ps NEXT MATCH (c:Company) RETURN size(ps + collect(c.name)) AS n"); !errors.Is(err, ErrBind) {
		t.Fatalf("unprojected carried var in wrapper: %v", err)
	}
}

func TestDistinctCollectAndMinMaxStrings(t *testing.T) {
	g := socialGraph(t)
	// DISTINCT inside collect: duplicate 2-hop endpoints collapse.
	rows := runBoth(t, g,
		"MATCH (a:Person {name: 'Alice'})-[:KNOWS]->()-[:KNOWS]->(f) RETURN size(collect(DISTINCT f.name)) AS n")
	r, _ := rows.Next()
	if v, _ := r.Get("n"); !value.Equal(v, value.Int(3)) {
		t.Fatalf("collect distinct size = %v", v)
	}
	// min/max order strings via the comparison semantics.
	rows = runBoth(t, g, "MATCH (p:Person) RETURN min(p.name) AS lo, max(p.name) AS hi")
	r, _ = rows.Next()
	if v, _ := r.Get("lo"); func() string { s, _ := v.AsStr(); return s }() != "Alice" {
		t.Fatalf("min name = %v", v)
	}
	if v, _ := r.Get("hi"); func() string { s, _ := v.AsStr(); return s }() != "Dave" {
		t.Fatalf("max name = %v", v)
	}
}
