package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// TestIntFloatValues covers the per-node vector converters that the CALL
// analytics procedures return through.
func TestIntFloatValues(t *testing.T) {
	iv := intValues([]uint32{0, 5, 9})
	if len(iv) != 3 {
		t.Fatalf("intValues len = %d, want 3", len(iv))
	}
	for i, want := range []int64{0, 5, 9} {
		if iv[i].Kind() != value.KindInt {
			t.Fatalf("intValues[%d] kind = %v, want Int", i, iv[i].Kind())
		}
		if got, _ := iv[i].AsInt(); got != want {
			t.Fatalf("intValues[%d] = %d, want %d", i, got, want)
		}
	}
	if len(intValues(nil)) != 0 {
		t.Fatal("intValues(nil) must be empty")
	}

	fv := floatValues([]float64{1.5, -2.0})
	if len(fv) != 2 {
		t.Fatalf("floatValues len = %d, want 2", len(fv))
	}
	for i, want := range []float64{1.5, -2.0} {
		if fv[i].Kind() != value.KindFloat {
			t.Fatalf("floatValues[%d] kind = %v, want Float", i, fv[i].Kind())
		}
		if got, _ := fv[i].AsFloat(); got != want {
			t.Fatalf("floatValues[%d] = %v, want %v", i, got, want)
		}
	}
}

// TestCdlpInit covers CDLP label seeding: an int seed property seeds each
// node that carries it, and every other case -- no seed property, a missing
// value, a non-existent column, and a non-i64 column -- falls back to the
// dense node id.
func TestCdlpInit(t *testing.T) {
	b := chickpeas.NewBuilder(8, 0)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	n0, _ := b.AddNode("N")
	must(b.SetProp(n0, "seed", int64(100)))
	n1, _ := b.AddNode("N")
	must(b.SetProp(n1, "seed", int64(200)))
	n2, _ := b.AddNode("N")
	must(b.SetProp(n2, "name", "x")) // string column, no seed
	_, _ = b.AddNode("N")            // node 3: no properties
	g := b.Finalize("cdlp")

	denseIDs := func(init []uint32, ctx string) {
		t.Helper()
		for i := range init {
			if init[i] != uint32(i) {
				t.Fatalf("%s: init[%d] = %d, want dense id %d", ctx, i, init[i], i)
			}
		}
	}

	// No seed property: every label is the dense id.
	denseIDs(cdlpInit(g, ""), "empty seedProp")
	// A non-existent column: dense ids.
	denseIDs(cdlpInit(g, "nonexistent"), "unknown column")
	// A non-i64 (string) column: dense ids.
	denseIDs(cdlpInit(g, "name"), "string column")

	// The int seed column seeds where present, dense id where absent.
	got := cdlpInit(g, "seed")
	if got[0] != 100 || got[1] != 200 {
		t.Fatalf("seeded labels = %d,%d, want 100,200", got[0], got[1])
	}
	if got[2] != 2 || got[3] != 3 {
		t.Fatalf("unseeded labels = %d,%d, want dense ids 2,3", got[2], got[3])
	}
}
