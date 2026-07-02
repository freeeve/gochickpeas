package chickpeas_test

import (
	"math"
	"slices"
	"sync"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// all_columns atoms: 3=di (dense i64), 4=df (dense f64), 5=db (dense bool),
// 6=ds (dense str), 7=si (sparse i64), 8=sf (sparse f64), 9=sb (sparse
// bool), 10=ss (sparse str), 11=v1. 13 nodes; rel columns di/df/ss by CSR
// position.
func TestPropTypedReads(t *testing.T) {
	g := fixture(t, "all_columns")

	if v, ok := g.Prop(0, "di").I64(); !ok || v != -6*1_000_000_007 {
		t.Fatalf("di[0]: got %d/%v", v, ok)
	}
	if v := g.Prop(12, "di").I64Or(0); v != 6*1_000_000_007 {
		t.Fatalf("di[12]: got %d", v)
	}

	// f64 bit exactness: canonical Rust NaN at 4, payload NaN at 5, -0.0 at 1.
	if v, ok := g.Prop(4, "df").F64(); !ok || !math.IsNaN(v) {
		t.Fatal("df[4] should be NaN")
	}
	if v, ok := g.Prop(5, "df").Value(); !ok {
		t.Fatal("df[5] absent")
	} else if f, _ := v.F64(); math.Float64bits(f) != 0x7FF8_DEAD_BEEF_0001 {
		t.Fatalf("df[5] payload: got %x", math.Float64bits(f))
	}
	if v, ok := g.Prop(1, "df").F64(); !ok || !math.Signbit(v) || v != 0 {
		t.Fatal("df[1] should be -0.0")
	}

	if v, ok := g.Prop(0, "db").Bool(); !ok || !v {
		t.Fatal("db[0] should be true")
	}
	if v, ok := g.Prop(1, "db").Bool(); !ok || v {
		t.Fatal("db[1] should be present false")
	}

	// Dense str: atom 0 means missing; Str folds that in.
	if s, ok := g.Prop(0, "ds").Str(); !ok || s != "v1" {
		t.Fatalf("ds[0]: got %q/%v", s, ok)
	}
	if _, ok := g.Prop(1, "ds").Str(); ok {
		t.Fatal("ds[1] (atom 0) should read as absent")
	}
	if s := g.Prop(1, "ds").StrOr("fallback"); s != "fallback" {
		t.Fatalf("StrOr: got %q", s)
	}

	// Sparse columns: present ids read, absent ids default.
	if v, ok := g.Prop(0, "si").I64(); !ok || v != math.MinInt64 {
		t.Fatal("si[0] wrong")
	}
	if v, ok := g.Prop(6, "si").I64(); !ok || v != 0 {
		t.Fatal("si[6] should be present zero")
	}
	if v := g.Prop(1, "si").I64Or(7); v != 7 {
		t.Fatalf("si[1] absent: got %d", v)
	}
	// A present false must beat BoolOr's default.
	if v := g.Prop(3, "sb").BoolOr(true); v {
		t.Fatal("sb[3] is stored false")
	}
	if _, ok := g.Prop(4, "sb").Bool(); ok {
		t.Fatal("sb[4] should be absent")
	}
	if _, ok := g.Prop(8, "ss").Str(); ok {
		t.Fatal("ss[8] (atom 0) should read as absent")
	}

	// Mistyped and missing reads are absent.
	if _, ok := g.Prop(0, "di").Str(); ok {
		t.Fatal("i64 read as string")
	}
	if _, ok := g.Prop(0, "nope").Value(); ok {
		t.Fatal("unknown key resolved")
	}
	if _, ok := g.Prop(99, "si").Value(); ok {
		t.Fatal("out-of-range node resolved")
	}

	// Present-but-empty version survives (distinct from absent).
	if v, ok := g.Version(); !ok || v != "" {
		t.Fatalf("version: got %q/%v", v, ok)
	}
}

func TestColReaders(t *testing.T) {
	g := fixture(t, "all_columns")

	di, ok := g.Col("di")
	if !ok || di.Dtype() != chickpeas.DtypeI64 {
		t.Fatal("di col missing or mistyped")
	}
	i64s := di.I64()
	if s, ok := i64s.Slice(); !ok || len(s) != 13 {
		t.Fatal("dense i64 slice missing")
	}
	if v, ok := i64s.Get(3); !ok || v != -3*1_000_000_007 {
		t.Fatalf("di.Get(3): got %d/%v", v, ok)
	}
	if _, ok := i64s.Get(13); ok {
		t.Fatal("past-end dense read resolved")
	}
	// Mistyped narrowing reads as absent.
	if _, ok := di.F64().Get(0); ok {
		t.Fatal("i64 column read as f64")
	}

	si, _ := g.Col("si")
	if _, ok := si.I64().Slice(); ok {
		t.Fatal("sparse column exposed a dense slice")
	}
	if v, ok := si.I64().Get(12); !ok || v != math.MaxInt64 {
		t.Fatal("sparse get wrong")
	}
	if _, ok := si.I64().Get(1); ok {
		t.Fatal("absent sparse id resolved")
	}

	// Indexed reads agree with binary-searched reads everywhere.
	siIdx, ok := g.ColIndexed("si")
	if !ok {
		t.Fatal("ColIndexed missing")
	}
	for pos := uint32(0); pos < 14; pos++ {
		a, aok := si.I64().Get(pos)
		b, bok := siIdx.I64().Get(pos)
		if aok != bok || a != b {
			t.Fatalf("indexed disagreement at %d: (%d,%v) vs (%d,%v)", pos, a, aok, b, bok)
		}
	}

	db, _ := g.Col("db")
	bools := db.Bool()
	if bits, ok := bools.Bits(); !ok || bits.Len() != 13 {
		t.Fatal("dense bool bits missing")
	}
	if v, ok := bools.Get(2); !ok || !v {
		t.Fatal("db[2] wrong")
	}

	ds, _ := g.Col("ds")
	strs := ds.Str()
	if ids, ok := strs.IDs(); !ok || ids[0] != 11 || ids[1] != 0 {
		t.Fatal("dense str ids wrong")
	}
	if id, ok := strs.ID(1); !ok || id != 0 {
		t.Fatal("raw str reader must expose atom 0 (Prop folds it, this must not)")
	}

	// Rel columns index by CSR position.
	rdi, ok := g.RelCol("di")
	if !ok {
		t.Fatal("rel col missing")
	}
	if v, ok := rdi.I64().Get(4); !ok || v != 50 {
		t.Fatalf("rel di[4]: got %d/%v", v, ok)
	}
	rss, ok := g.RelColIndexed("ss")
	if !ok {
		t.Fatal("rel col indexed missing")
	}
	if id, ok := rss.Str().ID(0); !ok || id != 11 {
		t.Fatal("rel ss[0] wrong")
	}
	if _, ok := rss.Str().ID(2); ok {
		t.Fatal("rel ss[2] should be absent")
	}
	if _, ok := g.Col("nope"); ok {
		t.Fatal("unknown column resolved")
	}
}

func TestNodePropertyKeys(t *testing.T) {
	g := fixture(t, "all_columns")
	// Node 0 carries di, df, db, ds (atom 11) and si (id 0). Dense str
	// reports present even at atom 0 (matching the Rust column semantics),
	// so node 1's keys still include ds.
	if got := g.NodePropertyKeys(0); !slices.Equal(got, []string{"db", "df", "di", "ds", "si"}) {
		t.Fatalf("keys(0): got %v", got)
	}
	got := g.NodePropertyKeys(1)
	if !slices.Contains(got, "ds") || !slices.Contains(got, "sf") || slices.Contains(got, "si") {
		t.Fatalf("keys(1): got %v", got)
	}
}

func TestNodesWithPropertyIndex(t *testing.T) {
	g := fixture(t, "small")
	alice, ok := g.NodesWithProperty("Person", "name", "alice")
	if !ok || !slices.Equal(alice.ToSlice(), []uint32{0}) {
		t.Fatal("alice lookup wrong")
	}
	bob, ok := g.NodesWithProperty("Person", "name", "bob")
	if !ok || !slices.Equal(bob.ToSlice(), []uint32{1}) {
		t.Fatal("bob lookup wrong")
	}
	if _, ok := g.NodesWithProperty("Person", "name", "never-interned"); ok {
		t.Fatal("unknown value matched")
	}
	if _, ok := g.NodesWithProperty("Nope", "name", "alice"); ok {
		t.Fatal("unknown label matched")
	}
	if n, ok := g.NodeWithProperty("name", "bob"); !ok || n != 1 {
		t.Fatal("NodeWithProperty wrong")
	}
	if n, ok := g.NodeWithLabelProperty("Person", "name", "alice"); !ok || n != 0 {
		t.Fatal("NodeWithLabelProperty wrong")
	}

	// Typed values through the any boundary, on the all-columns fixture.
	ac := fixture(t, "all_columns")
	if set, ok := ac.NodesWithProperty("N", "di", -6*1_000_000_007); !ok || !slices.Equal(set.ToSlice(), []uint32{0}) {
		t.Fatal("int lookup wrong")
	}
	if set, ok := ac.NodesWithProperty("N", "db", true); !ok || set.Len() != 7 {
		t.Fatal("bool lookup wrong")
	}
	if set, ok := ac.NodesWithProperty("N", "df", 1.5); !ok || !slices.Equal(set.ToSlice(), []uint32{6}) {
		t.Fatal("float lookup wrong")
	}
	if set, ok := ac.NodesWithValue("N", "si", chickpeas.I64Value(math.MinInt64)); !ok || !slices.Equal(set.ToSlice(), []uint32{0}) {
		t.Fatal("typed Value lookup wrong")
	}
}

// TestNodesWithPropertyConcurrent exercises the lazy-index choreography
// under the race detector: many goroutines triggering the same build.
func TestNodesWithPropertyConcurrent(t *testing.T) {
	g := fixture(t, "big") // score column over 20k nodes
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				g.NodesWithProperty("Node", "tag", "x")
				g.NodeWithProperty("tag", "x")
				g.ColIndexed("tag")
				g.RelTypeCount("KNOWS")
			}
		}()
	}
	wg.Wait()
	set, ok := g.NodesWithProperty("Node", "tag", "x")
	if !ok || set.Len() != (20_000+96)/97 {
		l := -1
		if ok {
			l = set.Len()
		}
		t.Fatalf("tag lookup after concurrency: len %d", l)
	}
}
