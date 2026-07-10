package chickpeas_test

import (
	"bytes"
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/rcpg"
)

// refinalize runs one thaw -> Finalize -> WriteRCPG pass over raw bytes.
func refinalize(t *testing.T, raw []byte, opts rcpg.WriteOptions) []byte {
	t.Helper()
	g, err := chickpeas.ReadRCPG(raw)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var buf bytes.Buffer
	if err := chickpeas.NewBuilderFromSnapshot(g).Finalize().WriteRCPGWith(&buf, opts); err != nil {
		t.Fatalf("write: %v", err)
	}
	return buf.Bytes()
}

// TestThawRoundTripByteIdentical: for every corpus file whose bytes Finalize
// itself can produce, ReadRCPG -> NewBuilderFromSnapshot -> Finalize ->
// WriteRCPG must reproduce the file exactly -- sparse ids, parallel rels,
// rel props, dense/sparse columns of all four kinds, float specials,
// unaligned dense bool, and version strings all survive the thaw.
//
// "big" carries a deliberately optimize()d run container in its label index
// (a parser-coverage feature of the corpus; neither finalize emits run
// containers). An unedited thaw dirties nothing, so Finalize aliases that
// bitmap straight through rather than rebuilding it as a plain array -- the
// run container survives and the bytes match exactly.
func TestThawRoundTripByteIdentical(t *testing.T) {
	for _, name := range []string{"sparse_ids", "all_columns", "multi_label_types", "big"} {
		t.Run(name, func(t *testing.T) {
			raw := fixtureBytes(t, name)
			got := refinalize(t, raw, rcpg.DefaultWriteOptions())
			if !bytes.Equal(got, raw) {
				t.Fatalf("thaw round trip differs from golden bytes (got %d, want %d)",
					len(got), len(raw))
			}
			// And the round trip is a fixed point from there.
			if again := refinalize(t, got, rcpg.DefaultWriteOptions()); !bytes.Equal(again, got) {
				t.Fatal("thaw round trip is not idempotent")
			}
		})
	}
	t.Run("topology_only", func(t *testing.T) {
		raw := fixtureBytes(t, "topology_only")
		if got := refinalize(t, raw, rcpg.TopologyOnlyWriteOptions()); !bytes.Equal(got, raw) {
			t.Fatal("topology-only thaw round trip differs from golden bytes")
		}
	})
}

// TestThawRoundTripFinalizeNormalized covers the corpus files that are
// hand-built in ways no finalize emits; for those the thaw round trip must
// match the builder-canonical encoding of the same graph and be idempotent
// from there.
func TestThawRoundTripFinalizeNormalized(t *testing.T) {
	// "small" hand-builds its rel column sparse at 1-of-2 fill, which
	// finalize (Rust and Go alike) stores dense -- so the thaw of the golden
	// file must match the builder-built graph's bytes instead.
	t.Run("small", func(t *testing.T) {
		var want bytes.Buffer
		if err := buildSmall(t).Finalize().WriteRCPG(&want); err != nil {
			t.Fatal(err)
		}
		got := refinalize(t, fixtureBytes(t, "small"), rcpg.DefaultWriteOptions())
		if !bytes.Equal(got, want.Bytes()) {
			t.Fatal("thawed small differs from builder-finalized small")
		}
	})

	// "empty" is written with a zero-length offset span; finalize always
	// emits at least one id slot, so compare against an empty builder.
	t.Run("empty", func(t *testing.T) {
		var want bytes.Buffer
		if err := chickpeas.NewBuilder(0, 0).Finalize().WriteRCPG(&want); err != nil {
			t.Fatal(err)
		}
		got := refinalize(t, fixtureBytes(t, "empty"), rcpg.DefaultWriteOptions())
		if !bytes.Equal(got, want.Bytes()) {
			t.Fatal("thawed empty differs from builder-finalized empty")
		}
	})

	// "big" moved to TestThawRoundTripByteIdentical: aliasing carries its run
	// container through the thaw, so it no longer needs normalizing.
}

// TestThawAllRelColumnTypes round-trips a builder-made snapshot carrying rel
// columns of all four dtypes plus node columns of all four dtypes through
// thaw -> Finalize -> thaw, with a detach-delete in between to purge staged
// props of every type.
func TestThawAllRelColumnTypes(t *testing.T) {
	b := chickpeas.NewBuilder(0, 0)
	for id := range chickpeas.NodeID(3) {
		b.AddNodeWithID(id, "L")
		b.SetProp(id, "ni", int64(id)+1)
		b.SetProp(id, "nf", float64(id)+0.5)
		b.SetProp(id, "nb", id%2 == 0)
		b.SetProp(id, "ns", "v")
	}
	b.AddRel(0, 1, "R")
	b.AddRel(1, 2, "R")
	for i := range 2 {
		b.SetRelPropAt(i, "ri", int64(i)+10)
		b.SetRelPropAt(i, "rf", float64(i)+0.25)
		b.SetRelPropAt(i, "rb", i == 0)
		b.SetRelPropAt(i, "rs", "w")
	}
	g := b.Finalize()

	var raw bytes.Buffer
	if err := g.WriteRCPG(&raw); err != nil {
		t.Fatal(err)
	}
	if got := refinalize(t, raw.Bytes(), rcpg.DefaultWriteOptions()); !bytes.Equal(got, raw.Bytes()) {
		t.Fatal("four-dtype rel/node column thaw round trip not byte-identical")
	}

	// Detach-delete on the thawed builder purges staged pairs of all types.
	b2 := chickpeas.NewBuilderFromSnapshot(g)
	if !b2.RemoveNode(2) {
		t.Fatal("remove")
	}
	g2 := b2.Finalize()
	if g2.NodeCount() != 2 || g2.RelCount() != 1 {
		t.Fatalf("counts after thawed detach-delete: %d/%d", g2.NodeCount(), g2.RelCount())
	}
	for _, key := range []string{"ni", "nf", "nb", "ns"} {
		if _, ok := g2.Prop(2, key).Value(); ok {
			t.Fatalf("purged %s survived", key)
		}
	}
	for r := range g2.Rels(0, chickpeas.Outgoing) {
		if v := g2.RelProp(r.Pos, "ri").I64Or(-1); v != 10 {
			t.Fatalf("surviving rel prop: %d", v)
		}
	}
}

// TestThawPreservesAtomsAndVersion pins the keystone invariants directly:
// atom ids never move across a thaw, and the version string carries over.
func TestThawPreservesAtomsAndVersion(t *testing.T) {
	g := fixture(t, "small")
	b := chickpeas.NewBuilderFromSnapshot(g)
	g2 := b.Finalize()
	if !slices.Equal(g.Atoms().Strings(), g2.Atoms().Strings()) {
		t.Fatalf("atom table changed: %v -> %v", g.Atoms().Strings(), g2.Atoms().Strings())
	}
	v1, ok1 := g.Version()
	v2, ok2 := g2.Version()
	if v1 != v2 || ok1 != ok2 {
		t.Fatalf("version changed: %q/%v -> %q/%v", v1, ok1, v2, ok2)
	}
}

// TestThawThenEdit: the write loop this task exists for -- thaw, edit
// (including new atoms), refinalize -- with readers of the old snapshot
// unaffected.
func TestThawThenEdit(t *testing.T) {
	g := fixture(t, "small")
	b := chickpeas.NewBuilderFromSnapshot(g)
	id, err := b.AddNode("Person")
	if err != nil {
		t.Fatal(err)
	}
	if id != 2 {
		t.Fatalf("auto id after thaw: %d, want 2 (nextNodeID = max known + 1)", id)
	}
	if _, err := b.AddRel(0, id, "KNOWS"); err != nil {
		t.Fatal(err)
	}
	if err := b.SetProp(id, "name", "carol"); err != nil {
		t.Fatal(err)
	}
	g2 := b.Finalize()
	if g2.NodeCount() != 3 || g2.RelCount() != 3 {
		t.Fatalf("edited counts: %d nodes / %d rels", g2.NodeCount(), g2.RelCount())
	}
	if s := g2.Prop(2, "name").StrOr(""); s != "carol" {
		t.Fatalf("new prop: %q", s)
	}
	if s := g2.Prop(1, "name").StrOr(""); s != "bob" {
		t.Fatalf("carried prop: %q", s)
	}
	// The old snapshot is untouched.
	if g.NodeCount() != 2 || g.RelCount() != 2 {
		t.Fatal("thaw or edit mutated the source snapshot")
	}
	if _, ok := g.Prop(2, "name").Str(); ok {
		t.Fatal("source snapshot sees the new node's property")
	}
}

// TestThawRelIndexInterleaving pins the lazy (u, v, type) first-match
// index across a thaw + AddRel interleaving: the index builds over the
// thaw-restaged rels on first SetRelProp use, AddRel must maintain it for
// triples added afterwards, and a later parallel duplicate must not steal
// the first match. Thaw restages through the builder's staging core, so
// this covers the invariant living in one place.
func TestThawRelIndexInterleaving(t *testing.T) {
	b := chickpeas.NewBuilder(0, 0)
	if _, err := b.AddNodeWithID(0, "L"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddNodeWithID(1, "L"); err != nil {
		t.Fatal(err)
	}
	for range 2 { // parallel (0)-[:R]->(1) pair
		if _, err := b.AddRel(0, 1, "R"); err != nil {
			t.Fatal(err)
		}
	}
	g := b.Finalize()

	b2 := chickpeas.NewBuilderFromSnapshot(g)
	// Force the lazy index to build over the thaw-restaged rels.
	if err := b2.SetRelProp(0, 1, "R", "first", int64(1)); err != nil {
		t.Fatal(err)
	}
	// A triple added AFTER the index exists must be resolvable through it.
	if _, err := b2.AddRel(1, 0, "R"); err != nil {
		t.Fatal(err)
	}
	if err := b2.SetRelProp(1, 0, "R", "back", int64(2)); err != nil {
		t.Fatalf("index not maintained for a post-thaw AddRel: %v", err)
	}
	// A later parallel duplicate must not steal the first match.
	if _, err := b2.AddRel(0, 1, "R"); err != nil {
		t.Fatal(err)
	}
	if err := b2.SetRelProp(0, 1, "R", "still", int64(3)); err != nil {
		t.Fatal(err)
	}
	g2 := b2.Finalize()

	// The first parallel rel carries both first-match writes; the other
	// parallels carry neither.
	var first, back, still []int64
	for r := range g2.Rels(0, chickpeas.Outgoing) {
		first = append(first, g2.RelProp(r.Pos, "first").I64Or(-1))
		still = append(still, g2.RelProp(r.Pos, "still").I64Or(-1))
	}
	for r := range g2.Rels(1, chickpeas.Outgoing) {
		back = append(back, g2.RelProp(r.Pos, "back").I64Or(-1))
	}
	if !slices.Equal(first, []int64{1, -1, -1}) {
		t.Fatalf("first-match write landed wrong: %v", first)
	}
	if !slices.Equal(still, []int64{3, -1, -1}) {
		t.Fatalf("post-duplicate first-match write landed wrong: %v", still)
	}
	if !slices.Equal(back, []int64{2}) {
		t.Fatalf("post-thaw AddRel write landed wrong: %v", back)
	}
}
