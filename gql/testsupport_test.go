// Shared test support for both gql test packages: the white-box suite
// (package gql) and the public end-to-end suite (package gql_test) in this
// directory compile into one test binary, so the fixtures and the dual-path
// harness live here once, exported to be reachable from both. These names
// exist only in the test binary -- they are not part of the gql API.
package gql

import (
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// SocialGraph is the Rust execute.rs fixture: four Persons (name, age,
// joined YYYYMMDD, optional city), two Companies, KNOWS diamond + WORKS_AT.
func SocialGraph(t testing.TB) *chickpeas.Snapshot {
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

// MultiSocialGraph is the SocialGraph shape with every fourth rel
// duplicated, exercising parallel-rel (multigraph) handling.
func MultiSocialGraph(t testing.TB) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(8, 32)
	people := []struct {
		name string
		age  int64
	}{{"Alice", 30}, {"Bob", 35}, {"Carol", 40}, {"Dave", 25}}
	for _, p := range people {
		id, _ := b.AddNode("Person")
		_ = b.SetProp(id, "name", p.name)
		_ = b.SetProp(id, "age", p.age)
	}
	for _, name := range []string{"Acme", "Globex"} {
		id, _ := b.AddNode("Company")
		_ = b.SetProp(id, "name", name)
	}
	rels := []struct {
		u, v chickpeas.NodeID
		t    string
	}{
		{0, 1, "KNOWS"}, {0, 2, "KNOWS"}, {1, 2, "KNOWS"}, {1, 3, "KNOWS"},
		{2, 3, "KNOWS"}, {2, 1, "KNOWS"}, {3, 0, "KNOWS"},
		{0, 4, "WORKS_AT"}, {1, 4, "WORKS_AT"}, {2, 5, "WORKS_AT"},
	}
	for i, r := range rels {
		if _, err := b.AddRel(r.u, r.v, r.t); err != nil {
			t.Fatal(err)
		}
		if i%4 == 0 {
			if _, err := b.AddRel(r.u, r.v, r.t); err != nil {
				t.Fatal(err)
			}
		}
	}
	return b.Finalize("multisocial")
}

// GeoGraph is three labeled places: Paris, Versailles (~17km away), and
// Lyon (~390km away).
func GeoGraph(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(4, 0)
	places := []struct {
		name     string
		lat, lon float64
	}{
		{"Paris", 48.8566, 2.3522},
		{"Versailles", 48.8049, 2.1204},
		{"Lyon", 45.7640, 4.8357},
	}
	for _, p := range places {
		id, _ := b.AddNode("Place")
		_ = b.SetProp(id, "name", p.name)
		_ = b.SetProp(id, "lat", p.lat)
		_ = b.SetProp(id, "lon", p.lon)
	}
	return b.Finalize("name")
}

// ReplyForest is the Rust fixture: root <- a, root <- b, a <- c (replyOf
// points child -> parent).
func ReplyForest(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(4, 4)
	for _, n := range []string{"root", "a", "b", "c"} {
		id, _ := b.AddNode("Msg")
		_ = b.SetProp(id, "name", n)
	}
	for _, e := range [][2]chickpeas.NodeID{{1, 0}, {2, 0}, {3, 1}} {
		if _, err := b.AddRel(e[0], e[1], "replyOf"); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize("name")
}

// RunBoth executes q under the compiled and interpreted eval paths,
// asserts identical rows, and returns the compiled result -- the dual-path
// differential harness every end-to-end test runs through.
func RunBoth(t *testing.T, g *chickpeas.Snapshot, q string) *Rows {
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

// StrColOrdered collects a string column preserving result order.
func StrColOrdered(t *testing.T, g *chickpeas.Snapshot, q, col string) []string {
	t.Helper()
	rows := RunBoth(t, g, q)
	var out []string
	for r := range rows.All() {
		v, _ := r.Get(col)
		s, _ := v.AsStr()
		out = append(out, s)
	}
	return out
}

// WantStrs asserts got equals want exactly, order included.
func WantStrs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// CachedInts runs q through the plan cache c and collects an integer
// column preserving result order.
func CachedInts(t *testing.T, c *PlanCache, g *chickpeas.Snapshot, q, col string) []int64 {
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
