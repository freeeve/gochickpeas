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
	"math"
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

// Multiple groups with a post-aggregate wrapper: the finalize rows share one
// arena backing slab, so each group's row must stay an independent window
// (no aliasing) and the hidden accumulator slot must read back correctly.
func TestGroupedRowsDoNotAliasAcrossArena(t *testing.T) {
	g := socialGraph(t)
	rows := runBoth(t, g,
		"MATCH (c:Company)<-[:WORKS_AT]-(p:Person) "+
			"RETURN c.name AS company, count(*) AS n, count(*) * 2 AS dbl ORDER BY company")
	type row struct {
		c      string
		n, dbl int64
	}
	var got []row
	for r := range rows.All() {
		cv, _ := r.Get("company")
		c, _ := cv.AsStr()
		nv, _ := r.Get("n")
		n, _ := nv.AsInt()
		dv, _ := r.Get("dbl")
		d, _ := dv.AsInt()
		got = append(got, row{c, n, d})
	}
	if len(got) != 2 || got[0] != (row{"Acme", 2, 4}) || got[1] != (row{"Globex", 1, 2}) {
		t.Fatalf("arena-backed grouped rows = %+v", got)
	}
}

// min/max and collect state lives in per-group overflow slabs (off the
// aggState struct). Group by company and check each group's extrema and
// collected list resolve independently -- guards the slab indexing.
func TestGroupedMinMaxCollectOverflowSlabs(t *testing.T) {
	g := socialGraph(t)
	rows := runBoth(t, g,
		"MATCH (c:Company)<-[:WORKS_AT]-(p:Person) "+
			"RETURN c.name AS co, min(p.age) AS lo, max(p.age) AS hi, size(collect(p.name)) AS n "+
			"ORDER BY co")
	type row struct {
		co        string
		lo, hi, n int64
	}
	var got []row
	for r := range rows.All() {
		cv, _ := r.Get("co")
		co, _ := cv.AsStr()
		lv, _ := r.Get("lo")
		lo, _ := lv.AsInt()
		hv, _ := r.Get("hi")
		hi, _ := hv.AsInt()
		nv, _ := r.Get("n")
		n, _ := nv.AsInt()
		got = append(got, row{co, lo, hi, n})
	}
	// Acme employs Alice(30) and Bob(35); Globex employs Carol(40).
	if len(got) != 2 || got[0] != (row{"Acme", 30, 35, 2}) || got[1] != (row{"Globex", 40, 40, 1}) {
		t.Fatalf("grouped min/max/collect = %+v", got)
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

// strRows drains a name column into a slice.
func strRows(t *testing.T, rows *Rows, col string) []string {
	t.Helper()
	var out []string
	for r := range rows.All() {
		v, _ := r.Get(col)
		s, _ := v.AsStr()
		out = append(out, s)
	}
	return out
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

// TestSumInt64Overflow ports the rustychickpeas tasks/236 sum-overflow
// semantics: a true total outside int64 range is Null (the engine's
// overflow policy -- no per-row error channel), decided by the total
// alone; a transient excursion that nets back into range stays exact; a
// sum over no rows is 0, never Null.
func TestSumInt64Overflow(t *testing.T) {
	build := func(vals ...int64) *chickpeas.Snapshot {
		b := chickpeas.NewBuilder(len(vals)+1, 1)
		for _, v := range vals {
			id, _ := b.AddNode("N")
			_ = b.SetProp(id, "v", v)
		}
		return b.Finalize()
	}
	const q = "MATCH (n:N) RETURN sum(n.v) AS s"
	sumOf := func(g *chickpeas.Snapshot) value.Value {
		rows := runBoth(t, g, q)
		r, ok := rows.Next()
		if !ok {
			t.Fatal("no row")
		}
		v, _ := r.Get("s")
		return v
	}

	// Exact at both boundaries.
	if v := sumOf(build(9223372036854775807)); func() int64 { i, _ := v.AsInt(); return i }() != 9223372036854775807 {
		t.Fatalf("sum at MaxInt64 = %v", v)
	}
	if v := sumOf(build(-9223372036854775808)); func() int64 { i, _ := v.AsInt(); return i }() != -9223372036854775808 {
		t.Fatalf("sum at MinInt64 = %v", v)
	}
	// One past either boundary: Null.
	if v := sumOf(build(9223372036854775807, 1)); !v.IsNull() {
		t.Fatalf("Max+1 = %v, want null", v)
	}
	if v := sumOf(build(-9223372036854775808, -1)); !v.IsNull() {
		t.Fatalf("Min-1 = %v, want null", v)
	}
	// Transient excursion netting back to zero stays exact.
	if v := sumOf(build(9223372036854775807, 9223372036854775807,
		-9223372036854775807, -9223372036854775807)); func() int64 { i, ok := v.AsInt(); _ = ok; return i }() != 0 || v.IsNull() {
		t.Fatalf("net-zero excursion = %v, want 0", v)
	}
	// Sum over no rows is 0, never Null.
	empty := chickpeas.NewBuilder(1, 1).Finalize()
	if v := sumOf(empty); v.IsNull() {
		t.Fatalf("empty sum = %v, want 0", v)
	}
}

// TestStddevAggregates pins the Welford accumulators against hand-derived
// values (121): Person ages 25/30/35 -- samp = sqrt(50/2) = 5, pop =
// sqrt(50/3); DISTINCT collapses duplicates first; empty and single
// groups finalize to 0, matching Neo4j.
func TestStddevAggregates(t *testing.T) {
	b := chickpeas.NewBuilder(8, 2)
	for _, age := range []int64{25, 30, 35, 35} { // 35 duplicated for DISTINCT
		n, err := b.AddNode("P")
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SetProp(n, "age", age); err != nil {
			t.Fatal(err)
		}
	}
	lone, err := b.AddNode("Lone")
	if err != nil {
		t.Fatal(err)
	}
	_ = b.SetProp(lone, "age", int64(50))
	g := b.Finalize()
	one := func(q string) float64 {
		t.Helper()
		rows := runBoth(t, g, q)
		r, ok := rows.Next()
		if !ok {
			t.Fatalf("no row: %s", q)
		}
		v, _ := r.GetAt(0)
		f, _ := v.AsFloat()
		return f
	}
	if got := one("MATCH (p:P) RETURN stddev_samp(DISTINCT p.age) AS s"); got != 5 {
		t.Fatalf("stddev_samp(DISTINCT) = %v, want 5", got)
	}
	if got, want := one("MATCH (p:P) RETURN stddev_pop(DISTINCT p.age) AS s"), math.Sqrt(50.0/3); math.Abs(got-want) > 1e-12 {
		t.Fatalf("stddev_pop(DISTINCT) = %v, want %v", got, want)
	}
	if got := one("MATCH (p:Lone) RETURN stddev_samp(p.age) AS s"); got != 0 {
		t.Fatalf("single-row stddev_samp = %v, want 0", got)
	}
	if got := one("MATCH (p:Nope) RETURN stddev_pop(p.age) AS s"); got != 0 {
		t.Fatalf("empty stddev_pop = %v, want 0", got)
	}
}

// TestPercentileAggregates pins percentile_cont/percentile_disc (Neo4j
// semantics): cont interpolates linearly (Float), disc picks the
// nearest-rank collected value, DISTINCT dedups before ranking,
// non-numeric args skip, an empty group and an out-of-range percentile
// are null, and the percentile must be a constant.
func TestPercentileAggregates(t *testing.T) {
	g := socialGraph(t)
	one := func(q string) value.Value {
		t.Helper()
		rows := runBoth(t, g, q)
		r, ok := rows.Next()
		if !ok {
			t.Fatalf("no row: %s", q)
		}
		v, _ := r.GetAt(0)
		return v
	}
	// Ages sorted: 25, 30, 35, 40.
	if v := one("MATCH (p:Person) RETURN percentile_cont(p.age, 0.5) AS m"); !value.Equal(v, value.Float(32.5)) {
		t.Fatalf("cont(0.5) = %v, want 32.5", v)
	}
	if v := one("MATCH (p:Person) RETURN percentile_cont(p.age, 0) AS m"); !value.Equal(v, value.Float(25)) {
		t.Fatalf("cont(0) = %v, want 25.0", v)
	}
	if v := one("MATCH (p:Person) RETURN percentile_cont(p.age, 1) AS m"); !value.Equal(v, value.Float(40)) {
		t.Fatalf("cont(1) = %v, want 40.0", v)
	}
	// disc returns the collected value itself (an Int here).
	if v := one("MATCH (p:Person) RETURN percentile_disc(p.age, 0.5) AS m"); !value.Equal(v, value.Int(30)) {
		t.Fatalf("disc(0.5) = %v, want 30", v)
	}
	if v := one("MATCH (p:Person) RETURN percentile_disc(p.age, 0) AS m"); !value.Equal(v, value.Int(25)) {
		t.Fatalf("disc(0) = %v, want 25", v)
	}
	if v := one("MATCH (p:Person) RETURN percentile_disc(p.age, 1) AS m"); !value.Equal(v, value.Int(40)) {
		t.Fatalf("disc(1) = %v, want 40", v)
	}
	// DISTINCT dedups before ranking: [1,2,2,10] p=0.75 -> 4.0 plain
	// (rank 2.25 over 4 values), 6.0 distinct (rank 1.5 over 3).
	if v := one("FOR x IN [1,2,2,10] RETURN percentile_cont(x, 0.75) AS m"); !value.Equal(v, value.Float(4)) {
		t.Fatalf("cont(0.75) multiset = %v, want 4.0", v)
	}
	if v := one("FOR x IN [1,2,2,10] RETURN percentile_cont(DISTINCT x, 0.75) AS m"); !value.Equal(v, value.Float(6)) {
		t.Fatalf("cont(0.75) DISTINCT = %v, want 6.0", v)
	}
	// Non-numeric values skip (like avg).
	if v := one("FOR x IN [1, 'a', 3] RETURN percentile_cont(x, 0.5) AS m"); !value.Equal(v, value.Float(2)) {
		t.Fatalf("cont over mixed = %v, want 2.0", v)
	}
	// Grouped: Acme {30, 35} -> 32.5; Globex {40} -> 40.
	rows := runBoth(t, g,
		"MATCH (p:Person)-[:WORKS_AT]->(c:Company) RETURN c.name AS cn, percentile_cont(p.age, 0.5) AS m ORDER BY cn")
	want := map[string]float64{"Acme": 32.5, "Globex": 40}
	n := 0
	for r := range rows.All() {
		n++
		cv, _ := r.Get("cn")
		cn, _ := cv.AsStr()
		mv, _ := r.Get("m")
		if !value.Equal(mv, value.Float(want[cn])) {
			t.Fatalf("group %s = %v, want %v", cn, mv, want[cn])
		}
	}
	if n != 2 {
		t.Fatalf("groups = %d", n)
	}
	// Empty input and out-of-range percentile are null.
	if v := one("MATCH (p:Person {name: 'Zed'}) RETURN percentile_cont(p.age, 0.5) AS m"); !v.IsNull() {
		t.Fatalf("empty group = %v, want null", v)
	}
	if v := one("MATCH (p:Person) RETURN percentile_cont(p.age, 1.5) AS m"); !v.IsNull() {
		t.Fatalf("out-of-range p = %v, want null", v)
	}
	// The percentile must be a constant literal; arity is two.
	if _, err := Run(g, "MATCH (p:Person) RETURN percentile_cont(p.age, p.age) AS m"); err == nil {
		t.Fatal("non-constant percentile must be a plan error")
	}
	if _, err := Run(g, "MATCH (p:Person) RETURN percentile_cont(p.age) AS m"); err == nil {
		t.Fatal("one-arg percentile must be a plan error")
	}
	// A parameter percentile works (it is a constant per execution).
	rows2, err := RunWithParams(g, "MATCH (p:Person) RETURN percentile_disc(p.age, $p) AS m",
		map[string]value.Value{"p": value.Float(0.5)})
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := rows2.Next()
	if v, _ := r2.GetAt(0); !value.Equal(v, value.Int(30)) {
		t.Fatalf("param percentile = %v, want 30", v)
	}
}
