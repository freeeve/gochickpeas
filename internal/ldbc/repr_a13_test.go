// Real-kernel prototype for the [][]value.Value migration (task 172, the
// step-1 de-risk of [[155]]'s option V). spbA13V is a byte-for-byte copy of
// spbA13's computation whose only change is the OUTPUT: instead of one boxed
// []any per row it appends value.Str cells into a single pre-sized flat backing
// and sub-slices the rows out of it -- so the whole result is a small constant
// number of allocations regardless of row count. The bench measures the real
// drop on spb_canonical.rcpg; TestA13ReprParity proves the two produce the
// identical parity hash. Test-scoped: no production kernel changes until the
// full rollout is greenlit.
package ldbc

import (
	"os"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// spbA13V is spbA13 with a value.Value result path. The computation above the
// row build is identical to native_spb_c.go's spbA13.
func spbA13V(g *chickpeas.Snapshot) ([][]value.Value, error) {
	works, ok := g.NodesWithLabel("CreativeWork")
	if !ok {
		return [][]value.Value{}, nil
	}
	type pair struct{ w, t chickpeas.NodeID }
	var pairs []pair
	for w := range works.Iter() {
		inCategory := false
		for c := range g.Neighbors(w, chickpeas.Outgoing, "category") {
			if u, ok := g.Prop(c, "uri").Str(); ok && (u == spbCatCompany || u == spbCategory) {
				inCategory = true
				break
			}
		}
		if !inCategory {
			continue
		}
		if _, ok := g.Prop(w, "dateModified").Str(); !ok {
			continue
		}
		for t := range g.Neighbors(w, chickpeas.Outgoing, "tag") {
			pairs = append(pairs, pair{w, t})
		}
	}
	sortByLess(pairs, func(a, b pair) bool {
		if a.w != b.w {
			return a.w < b.w
		}
		return a.t < b.t
	})
	// Output path (the only divergence from spbA13): pre-size one flat backing
	// (upper bound = every pair survives), append two value.Str cells per
	// surviving row, then carve fixed-cap row views out of the finished
	// backing. Two allocations total -- the cells backing and the row spine.
	cells := make([]value.Value, 0, len(pairs)*2)
	var last pair
	for i, p := range pairs {
		if i > 0 && p == last {
			continue // SELECT DISTINCT over (?thing, ?tag)
		}
		last = p
		if tagURI, ok := g.Prop(p.t, "uri").Str(); ok {
			cells = append(cells, value.Str(spbURIOf(g, p.w)), value.Str(tagURI))
		}
	}
	n := len(cells) / 2
	rows := make([][]value.Value, n)
	for i := range rows {
		rows[i] = cells[i*2 : i*2+2 : i*2+2]
	}
	return rows, nil
}

// loadReprGraph loads the graph named by GOCHICKPEAS_SF1_RCPG (point it at
// spb_canonical.rcpg for a13), skipping when unset -- same gating as
// BenchmarkNativeKernel.
func loadReprGraph(tb testing.TB) *chickpeas.Snapshot {
	tb.Helper()
	path := os.Getenv("GOCHICKPEAS_SF1_RCPG")
	if path == "" {
		tb.Skip("GOCHICKPEAS_SF1_RCPG not set")
	}
	g, err := chickpeas.ReadRCPGFile(path)
	if err != nil {
		tb.Fatal(err)
	}
	return g
}

func BenchmarkA13Boxed(b *testing.B) {
	g := loadReprGraph(b)
	if _, err := spbA13(g); err != nil { // warm
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, err := spbA13(g)
		if err != nil {
			b.Fatal(err)
		}
		reprSinkAny = r
	}
}

func BenchmarkA13Value(b *testing.B) {
	g := loadReprGraph(b)
	if _, err := spbA13V(g); err != nil { // warm
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, err := spbA13V(g)
		if err != nil {
			b.Fatal(err)
		}
		reprSinkVal = r
	}
}

// TestA13ReprParity locks in that the value.Value a13 produces the identical
// parity hash to the boxed a13 on real data -- the migration precondition for
// this kernel.
func TestA13ReprParity(t *testing.T) {
	if os.Getenv("GOCHICKPEAS_SF1_RCPG") == "" {
		t.Skip("GOCHICKPEAS_SF1_RCPG not set")
	}
	g := loadReprGraph(t)
	boxed, err := spbA13(g)
	if err != nil {
		t.Fatalf("boxed a13: %v", err)
	}
	hA, err := RowsHash(boxed)
	if err != nil {
		t.Fatalf("boxed hash: %v", err)
	}
	val, err := spbA13V(g)
	if err != nil {
		t.Fatalf("value a13: %v", err)
	}
	hV, err := RowsHashV(val)
	if err != nil {
		t.Fatalf("value hash: %v", err)
	}
	if hA != hV {
		t.Fatalf("a13 repr mismatch: boxed=%s value=%s", hA, hV)
	}
}
