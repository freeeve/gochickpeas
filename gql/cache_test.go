// M21 tests: plan-cache template sharing and byte-bound eviction (ports
// of the Rust execute.rs cache tests), Prepared round trips, cached-vs-
// uncached row equality, and concurrent cache use.
package gql

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// cachedInts collects an int column through the cache.
func cachedInts(t *testing.T, c *PlanCache, g *chickpeas.Snapshot, q, col string) []int64 {
	t.Helper()
	rows, err := c.Run(g, q)
	if err != nil {
		t.Fatalf("cached query failed: %s\n%v", q, err)
	}
	var out []int64
	for r := range rows.All() {
		v, _ := r.Get(col)
		i, ok := v.AsInt()
		if !ok {
			t.Fatalf("column %q not an int in %s: %v", col, q, v)
		}
		out = append(out, i)
	}
	return out
}

func TestAutoParamSharesPlanAcrossInlineLiterals(t *testing.T) {
	g := socialGraph(t)
	c := NewPlanCache(0)
	age := func(q string) []int64 { return cachedInts(t, c, g, q, "a") }
	// Three queries differing only in the inline-prop constant: each
	// resolves its own anchor value, all collapse to one cached template.
	if got := age("MATCH (p:Person {name: 'Alice'}) RETURN p.age AS a"); !slices.Equal(got, []int64{30}) {
		t.Fatalf("alice = %v", got)
	}
	if got := age("MATCH (p:Person {name: 'Bob'}) RETURN p.age AS a"); !slices.Equal(got, []int64{35}) {
		t.Fatalf("bob = %v", got)
	}
	if got := age("MATCH (p:Person {name: 'Carol'}) RETURN p.age AS a"); !slices.Equal(got, []int64{40}) {
		t.Fatalf("carol = %v", got)
	}
	if c.Len() != 1 {
		t.Fatalf("all three literals share one plan, Len = %d", c.Len())
	}
	l1, l2, misses := c.stats()
	if l1 != 0 || l2 != 2 || misses != 1 {
		t.Fatalf("counters = (l1 %d, l2 %d, miss %d), want (0, 2, 1)", l1, l2, misses)
	}
	// A verbatim repeat takes the L1 fast path -- and still returns its
	// own literal's rows, not another variant's.
	if got := age("MATCH (p:Person {name: 'Bob'}) RETURN p.age AS a"); !slices.Equal(got, []int64{35}) {
		t.Fatalf("bob repeat = %v", got)
	}
	if l1, _, _ := c.stats(); l1 != 1 {
		t.Fatalf("verbatim repeat missed L1: %d", l1)
	}
	// A structurally different query is a distinct template and matches
	// the uncached path.
	q := "MATCH (p:Person) WHERE p.age > 30 RETURN p.name AS a"
	rows, err := c.Run(g, q)
	if err != nil {
		t.Fatal(err)
	}
	var cached []string
	for r := range rows.All() {
		v, _ := r.Get("a")
		s, _ := v.AsStr()
		cached = append(cached, s)
	}
	slices.Sort(cached)
	wantStrs(t, cached, "Bob", "Carol")
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
}

func TestPlanCacheEvictsUnderByteBound(t *testing.T) {
	g := socialGraph(t)
	// A tiny budget: many distinct templates can't all stay resident.
	c := NewPlanCache(8 * 1024)
	for i := range 200 {
		q := fmt.Sprintf("MATCH (p:Person {name: 'Alice'}) RETURN p.age AS a%d", i)
		col := fmt.Sprintf("a%d", i)
		if got := cachedInts(t, c, g, q, col); !slices.Equal(got, []int64{30}) {
			t.Fatalf("query %d = %v", i, got)
		}
	}
	if c.Bytes() > c.MaxBytes() {
		t.Fatalf("over budget: %d > %d", c.Bytes(), c.MaxBytes())
	}
	if c.Len() >= 200 {
		t.Fatalf("nothing evicted: %d live", c.Len())
	}
	// Results stay correct after eviction.
	if got := cachedInts(t, c, g, "MATCH (p:Person {name: 'Bob'}) RETURN p.age AS a", "a"); !slices.Equal(got, []int64{35}) {
		t.Fatalf("post-eviction rows = %v", got)
	}
	// Shrinking the budget evicts immediately.
	c.SetMaxBytes(1)
	if !c.IsEmpty() {
		t.Fatalf("shrunk budget kept %d templates", c.Len())
	}
}

func TestPreparedRoundTrip(t *testing.T) {
	g := socialGraph(t)
	pr, err := Prepare(g, "MATCH (p:Person {name: $who}) WHERE p.age > 26 RETURN p.age AS age")
	if err != nil {
		t.Fatal(err)
	}
	if cols := pr.Columns(); len(cols) != 1 || cols[0] != "age" {
		t.Fatalf("columns = %v", cols)
	}
	// Re-execute with different named params; the lifted inline constant
	// (26) rebinds automatically each run.
	for who, want := range map[string]int64{"Alice": 30, "Carol": 40} {
		rows, err := pr.Execute(g, map[string]value.Value{"who": value.Str(who)})
		if err != nil {
			t.Fatal(err)
		}
		r, ok := rows.Next()
		if !ok {
			t.Fatalf("%s: no rows", who)
		}
		if v, _ := r.Get("age"); !value.Equal(v, value.Int(want)) {
			t.Fatalf("%s age = %v", who, v)
		}
	}
	// Dave (25) fails the lifted threshold.
	rows, err := pr.Execute(g, map[string]value.Value{"who": value.Str("Dave")})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rows.Next(); ok {
		t.Fatal("lifted threshold must still filter")
	}
	// Prepared matches Run for the same inputs.
	direct, err := RunWithParams(g, "MATCH (p:Person {name: $who}) WHERE p.age > 26 RETURN p.age AS age",
		map[string]value.Value{"who": value.Str("Alice")})
	if err != nil {
		t.Fatal(err)
	}
	viaPrep, _ := pr.Execute(g, map[string]value.Value{"who": value.Str("Alice")})
	dr, _ := direct.Next()
	prr, _ := viaPrep.Next()
	dv, _ := dr.GetAt(0)
	pv, _ := prr.GetAt(0)
	if value.Key(dv) != value.Key(pv) {
		t.Fatalf("prepared %v != direct %v", pv, dv)
	}
	// An EXPLAIN-mode Prepare renders the plan on Execute.
	ep, err := Prepare(g, "EXPLAIN MATCH (p:Person) RETURN p.name AS n")
	if err != nil {
		t.Fatal(err)
	}
	erows, err := ep.Execute(g, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cols := erows.Columns(); len(cols) != 1 || cols[0] != "plan" {
		t.Fatalf("explain prepared columns = %v", cols)
	}
	if len(erows.NextBatch(100)) == 0 {
		t.Fatal("explain prepared emitted no plan rows")
	}
}

func TestCachedVsUncachedRowEquality(t *testing.T) {
	g := socialGraph(t)
	c := NewPlanCache(0)
	// A selective literal anchor: the cached (template) plan may differ in
	// shape from the uncached literal-probed plan, never in rows.
	q := "MATCH (a:Person {name: 'Alice'})-[:KNOWS]->(f:Person) WHERE f.age > 26 RETURN f.name AS n ORDER BY n"
	direct := strColOrdered(t, g, q, "n")
	rows, err := c.Run(g, q)
	if err != nil {
		t.Fatal(err)
	}
	var cached []string
	for r := range rows.All() {
		v, _ := r.Get("n")
		s, _ := v.AsStr()
		cached = append(cached, s)
	}
	if !slices.Equal(direct, cached) {
		t.Fatalf("cached %v != direct %v", cached, direct)
	}
	// And again through the L1 fast path.
	rows, _ = c.Run(g, q)
	cached = cached[:0]
	for r := range rows.All() {
		v, _ := r.Get("n")
		s, _ := v.AsStr()
		cached = append(cached, s)
	}
	if !slices.Equal(direct, cached) {
		t.Fatalf("L1 cached %v != direct %v", cached, direct)
	}
}

func TestPlanCacheConcurrent(t *testing.T) {
	g := socialGraph(t)
	c := NewPlanCache(32 * 1024)
	names := []string{"Alice", "Bob", "Carol", "Dave"}
	want := map[string]int64{"Alice": 30, "Bob": 35, "Carol": 40, "Dave": 25}
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for w := range 8 {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := range 25 {
				name := names[(seed+i)%len(names)]
				q := fmt.Sprintf("MATCH (p:Person {name: '%s'}) RETURN p.age AS a", name)
				rows, err := c.Run(g, q)
				if err != nil {
					errs <- err.Error()
					return
				}
				r, ok := rows.Next()
				if !ok {
					errs <- "no rows for " + name
					return
				}
				v, _ := r.Get("a")
				if got, _ := v.AsInt(); got != want[name] {
					errs <- fmt.Sprintf("%s = %d, want %d", name, got, want[name])
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	if c.Len() != 1 {
		t.Fatalf("four literals of one template share one plan, Len = %d", c.Len())
	}
}
