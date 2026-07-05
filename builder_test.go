package chickpeas_test

import (
	"bytes"
	"math"
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// buildSmall stages the conformance corpus's "small" graph through the
// public Builder API in the exact atom order the golden file encodes.
func buildSmall(t *testing.T) *chickpeas.Builder {
	t.Helper()
	b := chickpeas.NewBuilder(4, 4)
	b.SetVersion("conformance-v1")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must2 := func(_ chickpeas.NodeID, err error) { t.Helper(); must(err) }
	must2(b.AddNodeWithID(0, "Person"))
	must2(b.AddNodeWithID(1, "Person"))
	if _, err := b.AddRel(0, 1, "KNOWS"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddRel(1, 0, "KNOWS"); err != nil {
		t.Fatal(err)
	}
	must(b.SetProp(0, "name", "alice"))
	must(b.SetProp(1, "name", "bob"))
	must(b.SetRelProp(0, 1, "KNOWS", "name", int64(42)))
	return b
}

// TestFinalizeMatchesGoldenBytes is the M4 acceptance test: staging the
// corpus graphs through the Builder and finalizing must produce snapshots
// that serialize byte-identically to the Rust-generated golden files --
// same CSR order, same dense/sparse column selection, same index sorting.
func TestFinalizeMatchesGoldenBytes(t *testing.T) {
	check := func(t *testing.T, g *chickpeas.Snapshot, golden string) {
		t.Helper()
		var buf bytes.Buffer
		if err := g.WriteRCPG(&buf); err != nil {
			t.Fatal(err)
		}
		want := fixtureBytes(t, golden)
		if !bytes.Equal(buf.Bytes(), want) {
			t.Fatalf("finalized snapshot differs from %s.rcpg (got %d bytes, want %d)",
				golden, buf.Len(), len(want))
		}
	}

	t.Run("small", func(t *testing.T) {
		// The golden "small" hand-builds its rel column sparse at 1-of-2
		// fill; the historical finalize (Rust, and Go before tasks/041)
		// stored that dense via the truncating 80% floor (1 >= int(2*0.8)),
		// present-zeroing the unset position. Since 041, i64 dense requires
		// full coverage, so 1-of-2 stays sparse -- matching the golden's own
		// layout AND its absent semantics.
		g := buildSmall(t).Finalize()
		want := fixture(t, "small")
		if g.NodeCount() != want.NodeCount() || g.RelCount() != want.RelCount() {
			t.Fatal("counts differ from golden")
		}
		if a, b := g.RelProp(0, "name").I64Or(-1), want.RelProp(0, "name").I64Or(-1); a != b {
			t.Fatalf("rel prop at 0: %d vs %d", a, b)
		}
		// The unset position reads absent, independent of storage layout.
		if v, ok := g.RelProp(1, "name").I64(); ok {
			t.Fatalf("unset position must read absent, got (%d,%v)", v, ok)
		}
		if s := g.Prop(1, "name").StrOr(""); s != "bob" {
			t.Fatalf("prop: %q", s)
		}
		rc, ok := g.RelCol("name")
		if !ok {
			t.Fatal("rel col missing")
		}
		if _, dense := rc.I64().Slice(); dense {
			t.Fatal("1-of-2 staged rel column must not finalize dense (full coverage required)")
		}
	})

	t.Run("multi_label_types", func(t *testing.T) {
		b := chickpeas.NewBuilder(4, 8)
		b.AddNodeWithID(0, "A")
		b.AddNodeWithID(1, "A", "B")
		b.AddNodeWithID(2, "B")
		rels := [][3]any{
			{0, 0, "LOOP"}, {1, 2, "DUP"}, {1, 2, "DUP"}, {1, 2, "OTHER"}, {2, 1, "DUP"},
		}
		for i, r := range rels {
			idx, err := b.AddRel(uint32(r[0].(int)), uint32(r[1].(int)), r[2].(string))
			if err != nil || idx != i {
				t.Fatalf("AddRel %d: idx=%d err=%v", i, idx, err)
			}
		}
		// Rel column keyed "OTHER" (already interned as a type), 1..5 by
		// rel index -- source-grouped staging makes index == CSR position.
		for i := range 5 {
			if err := b.SetRelPropAt(i, "OTHER", int64(i+1)); err != nil {
				t.Fatal(err)
			}
		}
		check(t, b.Finalize(), "multi_label_types")
	})

	t.Run("sparse_ids", func(t *testing.T) {
		b := chickpeas.NewBuilder(0, 0)
		for _, id := range []chickpeas.NodeID{0, 5, 1000, 65000} {
			b.AddNodeWithID(id, "Thing")
		}
		b.AddRel(0, 65000, "REL")
		b.AddRel(1000, 5, "REL")
		b.AddRel(0, 5, "REL")
		b.SetProp(0, "weight", int64(-1))
		b.SetProp(5, "weight", int64(500))
		b.SetProp(65000, "weight", int64(math.MaxInt64))
		b.SetProp(1000, "tag", "hi")
		b.SetProp(65000, "tag", chickpeas.StrValue(0)) // explicit atom-0 value
		check(t, b.Finalize(), "sparse_ids")
	})

	t.Run("all_columns", func(t *testing.T) {
		b := chickpeas.NewBuilder(16, 8)
		for i := range chickpeas.NodeID(13) {
			b.AddNodeWithID(i, "N")
		}
		for _, r := range [][2]chickpeas.NodeID{{0, 1}, {1, 2}, {2, 0}, {3, 4}, {12, 0}} {
			b.AddRel(r[0], r[1], "T")
		}
		// The golden file interns every column key before any value, so
		// pre-intern in atom order (the documented use of InternPropertyKey).
		for _, key := range []string{"di", "df", "db", "ds", "si", "sf", "sb", "ss"} {
			b.InternPropertyKey(key)
		}
		nanPayload := math.Float64frombits(0x7FF8_DEAD_BEEF_0001)
		rustNaN := math.Float64frombits(0x7FF8_0000_0000_0000)
		dfVals := []float64{
			0.0, math.Copysign(0, -1), math.Inf(1), math.Inf(-1), rustNaN,
			nanPayload, 1.5, -2.5, math.Float64frombits(0x0010_0000_0000_0000),
			math.MaxFloat64, math.Float64frombits(0x3CB0_0000_0000_0000), 3.14, -1e-300,
		}
		dbTrue := []chickpeas.NodeID{0, 2, 3, 5, 7, 11, 12}
		dsVals := []uint32{11, 0, 11, 0, 0, 11, 0, 11, 11, 0, 11, 0, 11}
		for i := range chickpeas.NodeID(13) {
			b.SetProp(i, "di", (int64(i)-6)*1_000_000_007)
			b.SetProp(i, "df", dfVals[i])
			b.SetProp(i, "db", slices.Contains(dbTrue, i))
			if dsVals[i] == 11 {
				b.SetProp(i, "ds", "v1")
			} else {
				b.SetProp(i, "ds", chickpeas.StrValue(0))
			}
		}
		b.SetProp(0, "si", int64(math.MinInt64))
		b.SetProp(6, "si", int64(0))
		b.SetProp(12, "si", int64(math.MaxInt64))
		b.SetProp(1, "sf", math.Copysign(0, -1))
		b.SetProp(5, "sf", nanPayload)
		b.SetProp(9, "sf", 2.25)
		b.SetProp(2, "sb", true)
		b.SetProp(3, "sb", false)
		b.SetProp(10, "sb", true)
		b.SetProp(4, "ss", "v1")
		b.SetProp(8, "ss", chickpeas.StrValue(0))
		// Rel columns by rel index (== CSR position: sources ascending).
		for i, v := range []int64{10, 20, 30, 40, 50} {
			b.SetRelPropAt(i, "di", v)
		}
		for i, v := range []float64{0.5, rustNaN, math.Copysign(0, -1), 4.0, 5.0} {
			b.SetRelPropAt(i, "df", v)
		}
		b.SetRelPropAt(0, "ss", "v1")
		b.SetRelPropAt(4, "ss", "v1")
		b.SetVersion("")
		check(t, b.Finalize(), "all_columns")
	})
}

func TestBuilderStagingQueries(t *testing.T) {
	b := buildSmall(t)
	if b.NodeCount() != 2 || b.RelCount() != 2 {
		t.Fatalf("counts: %d/%d", b.NodeCount(), b.RelCount())
	}
	if v, ok := b.Prop(0, "name"); !ok {
		t.Fatal("staged prop missing")
	} else if atom, _ := v.StrID(); atom != 4 {
		t.Fatalf("staged prop atom: %d", atom)
	}
	if _, ok := b.Prop(0, "never"); ok {
		t.Fatal("unknown key resolved")
	}
	if got := b.NodeLabels(0); !slices.Equal(got, []string{"Person"}) {
		t.Fatalf("labels: %v", got)
	}
	if got := b.NeighborIDs(0, chickpeas.Both); !slices.Equal(got, []chickpeas.NodeID{1, 1}) {
		t.Fatalf("neighbors: %v", got)
	}
	if err := b.SetRelProp(0, 9, "KNOWS", "k", int64(1)); err == nil {
		t.Fatal("missing rel accepted a property")
	}
	if err := b.SetProp(0, "bad", struct{}{}); err == nil {
		t.Fatal("unsupported value type accepted")
	}
}

func TestBuilderAutoIDsAndGrowth(t *testing.T) {
	b := chickpeas.NewBuilder(1, 1) // force growth
	first, err := b.AddNode("L")
	if err != nil || first != 0 {
		t.Fatalf("first auto id: %d/%v", first, err)
	}
	if id, _ := b.AddNodeWithID(500, "L"); id != 500 {
		t.Fatal("explicit id lost")
	}
	// Auto ids continue past the highest explicit id.
	next, _ := b.AddNode("L")
	if next != 501 {
		t.Fatalf("auto id after explicit: %d", next)
	}
	if _, err := b.AddRel(0, 500, "R"); err != nil {
		t.Fatal(err)
	}
	g := b.Finalize()
	if g.NodeCount() != 3 || g.CSRIDSpace() != 502 {
		t.Fatalf("counts: %d/%d", g.NodeCount(), g.CSRIDSpace())
	}
	if got := slices.Collect(g.Neighbors(0, chickpeas.Outgoing)); !slices.Equal(got, []uint32{500}) {
		t.Fatalf("neighbors: %v", got)
	}
}

func TestUpdatePropLastWriteWins(t *testing.T) {
	// Duplicate staged writes resolve to the newest value in BOTH storage
	// layouts (the sparse path sorts last-write-wins; the dense fill
	// overwrites in order).
	b := chickpeas.NewBuilder(0, 0)
	b.AddNodeWithID(0, "L")
	b.AddNodeWithID(9, "L") // widen the span so the column stays sparse
	b.SetProp(0, "x", int64(1))
	b.SetProp(0, "x", int64(2))
	g := b.Finalize()
	if v := g.Prop(0, "x").I64Or(0); v != 2 {
		t.Fatalf("sparse duplicate resolved to %d, want 2", v)
	}

	b2 := chickpeas.NewBuilder(0, 0)
	b2.AddNodeWithID(0, "L")
	b2.SetProp(0, "x", int64(1))
	b2.UpdateProp(0, "x", int64(3))
	g2 := b2.Finalize()
	if v := g2.Prop(0, "x").I64Or(0); v != 3 {
		t.Fatalf("update resolved to %d, want 3", v)
	}
}

func TestEagerPropertyIndex(t *testing.T) {
	b := buildSmall(t)
	g := b.Finalize("name")
	set, ok := g.NodesWithProperty("Person", "name", "alice")
	if !ok || !slices.Equal(set.ToSlice(), []uint32{0}) {
		t.Fatal("eager index lookup wrong")
	}
}

// TestRankSelectColumn drives the moderately-sparse band: a 1M+-span column
// whose fill is under 80% but dense enough that the rank layout wins. Reads
// must agree with a reference map, and serialization (rank -> sparse on
// disk) must round-trip.
func TestRankSelectColumn(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 1M-node graph")
	}
	const span = 1_200_000
	b := chickpeas.NewBuilder(span, 1)
	b.AddNodeWithID(span-1, "L") // widen the id space
	key := b.InternPropertyKey("r")
	want := map[uint32]int64{}
	// ~1/3 fill: well under dense, far above the rank-worth break-even.
	for id := uint32(0); id < span; id += 3 {
		val := int64(id) * 7
		b.SetPropByKey(id, key, val)
		want[id] = val
	}
	g := b.Finalize()
	col, ok := g.Col("r")
	if !ok {
		t.Fatal("column missing")
	}
	if _, isDense := col.I64().Slice(); isDense {
		t.Fatal("column unexpectedly dense")
	}
	for _, probe := range []uint32{0, 3, 511, 512, 513, 999_999, span - 3, 1, 4, span - 1} {
		got, ok := col.I64().Get(probe)
		wantVal, wantOK := want[probe]
		if ok != wantOK || got != wantVal {
			t.Fatalf("rank get(%d): got (%d,%v), want (%d,%v)", probe, got, ok, wantVal, wantOK)
		}
	}

	// Round trip: rank serializes as sparse and reads back identically.
	var buf bytes.Buffer
	if err := g.WriteRCPG(&buf); err != nil {
		t.Fatal(err)
	}
	back, err := chickpeas.ReadRCPG(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	for _, probe := range []uint32{0, 513, span - 3, 1} {
		a := g.Prop(probe, "r").I64Or(-1)
		z := back.Prop(probe, "r").I64Or(-1)
		if a != z {
			t.Fatalf("round trip disagrees at %d: %d vs %d", probe, a, z)
		}
	}
}

// TestTypeIndexUsesCSRPositions pins the FORMAT.md semantics: when rels are
// staged out of source order, the type index still holds outgoing-CSR
// positions.
func TestTypeIndexUsesCSRPositions(t *testing.T) {
	b := chickpeas.NewBuilder(0, 0)
	b.AddNodeWithID(0, "L")
	b.AddNodeWithID(5, "L")
	// Staged high-source first: builder rel 0 = (5->0), rel 1 = (0->5).
	// CSR order puts (0->5) at position 0.
	b.AddRel(5, 0, "A")
	b.AddRel(0, 5, "B")
	g := b.Finalize()
	bPos, ok := g.RelsWithType("B")
	if !ok || !slices.Equal(bPos.ToSlice(), []uint32{0}) {
		t.Fatalf("type B positions: %v", bPos.ToSlice())
	}
	aPos, _ := g.RelsWithType("A")
	if !slices.Equal(aPos.ToSlice(), []uint32{1}) {
		t.Fatalf("type A positions: %v", aPos.ToSlice())
	}
}
