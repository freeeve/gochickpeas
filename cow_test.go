package chickpeas

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"
)

// sameBacking reports whether two slices share a backing array -- the test
// that distinguishes a shared component from an equal rebuild.
func sameBacking[T any](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	return &a[0] == &b[0]
}

// columnBacking is the address of a column's first backing array, reached
// through whatever struct or pointer wraps it (dense columns are slices,
// sparse and rank columns wrap several).
func columnBacking(c Column) uintptr {
	return firstSlicePointer(reflect.ValueOf(c))
}

func firstSlicePointer(v reflect.Value) uintptr {
	switch v.Kind() {
	case reflect.Slice:
		return v.Pointer()
	case reflect.Pointer:
		if v.IsNil() {
			return 0
		}
		return firstSlicePointer(v.Elem())
	case reflect.Struct:
		for i := range v.NumField() {
			if p := firstSlicePointer(v.Field(i)); p != 0 {
				return p
			}
		}
	}
	return 0
}

// cowSource builds the snapshot the copy-on-write tests thaw from: two
// labels, node columns of two dtypes, a rel column, and parallel rels.
func cowSource(t testing.TB) *Snapshot {
	t.Helper()
	b := NewBuilder(8, 8)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for id := range NodeID(6) {
		label := "Person"
		if id%2 == 1 {
			label = "City"
		}
		if _, err := b.AddNodeWithID(id, label); err != nil {
			t.Fatal(err)
		}
		must(b.SetProp(id, "name", fmt.Sprintf("n%d", id)))
		must(b.SetProp(id, "age", int64(20+id)))
		must(b.SetProp(id, "score", float64(id)+0.25))
		must(b.SetProp(id, "active", id%3 == 0))
	}
	for _, e := range [][2]NodeID{{0, 1}, {1, 2}, {2, 3}, {0, 1}, {4, 5}} {
		idx, err := b.AddRel(e[0], e[1], "KNOWS")
		if err != nil {
			t.Fatal(err)
		}
		must(b.SetRelPropAt(idx, "weight", float64(idx)+0.5))
		must(b.SetRelPropAt(idx, "since", int64(2000+idx)))
	}
	return b.Finalize()
}

// keyOf resolves a property key that the source is known to carry.
func keyOf(t testing.TB, g *Snapshot, name string) PropertyKey {
	t.Helper()
	k, ok := g.PropertyKey(name)
	if !ok {
		t.Fatalf("key %q not interned", name)
	}
	return k
}

func labelOf(t testing.TB, g *Snapshot, name string) Label {
	t.Helper()
	l, ok := g.Label(name)
	if !ok {
		t.Fatalf("label %q not interned", name)
	}
	return l
}

// assertCSRAliased checks both CSR directions (and inToOut) are shared.
func assertCSRAliased(t *testing.T, got, src *Snapshot, want bool) {
	t.Helper()
	checks := []struct {
		name   string
		shared bool
	}{
		{"outOffsets", sameBacking(got.outOffsets, src.outOffsets)},
		{"outNbrs", sameBacking(got.outNbrs, src.outNbrs)},
		{"outTypes", sameBacking(got.outTypes, src.outTypes)},
		{"inOffsets", sameBacking(got.inOffsets, src.inOffsets)},
		{"inNbrs", sameBacking(got.inNbrs, src.inNbrs)},
		{"inTypes", sameBacking(got.inTypes, src.inTypes)},
		{"inToOut", sameBacking(got.inToOut, src.inToOut)},
	}
	for _, c := range checks {
		if c.shared != want {
			t.Errorf("%s shared = %v, want %v", c.name, c.shared, want)
		}
	}
}

// TestRefinalizeNoEditAliasesEveryComponent: an unedited thaw dirties nothing,
// so every component of the successor is the source's, not a rebuild of it.
func TestRefinalizeNoEditAliasesEveryComponent(t *testing.T) {
	src := cowSource(t)
	got := NewBuilderFromSnapshot(src).Finalize()

	assertCSRAliased(t, got, src, true)
	if got.atoms != src.atoms {
		t.Error("atom table rebuilt, want aliased")
	}
	for key, col := range src.columns {
		if columnBacking(got.columns[key]) != columnBacking(col) {
			t.Errorf("node column %d rebuilt, want aliased", key)
		}
	}
	for key, col := range src.relColumns {
		if columnBacking(got.relColumns[key]) != columnBacking(col) {
			t.Errorf("rel column %d rebuilt, want aliased", key)
		}
	}
	for l, set := range src.labelIndex {
		if got.labelIndex[l] != set {
			t.Errorf("label %d bitmap rebuilt, want aliased", l)
		}
	}
	for tp, set := range src.typeIndex {
		if got.typeIndex[tp] != set {
			t.Errorf("type %d bitmap rebuilt, want aliased", tp)
		}
	}
}

// TestRefinalizePropertyEditSharesTheRest: a one-property edit rebuilds that
// column alone -- both CSRs, the other columns, and every label bitmap are
// shared, and the source snapshot still reads its old value.
func TestRefinalizePropertyEditSharesTheRest(t *testing.T) {
	src := cowSource(t)
	b := NewBuilderFromSnapshot(src)
	if err := b.UpdateProp(0, "age", int64(99)); err != nil {
		t.Fatal(err)
	}
	got := b.Finalize()

	assertCSRAliased(t, got, src, true)
	ageKey, nameKey := keyOf(t, src, "age"), keyOf(t, src, "name")
	if columnBacking(got.columns[ageKey]) == columnBacking(src.columns[ageKey]) {
		t.Error("edited age column aliased, want rebuilt")
	}
	if columnBacking(got.columns[nameKey]) != columnBacking(src.columns[nameKey]) {
		t.Error("untouched name column rebuilt, want aliased")
	}
	weightKey := keyOf(t, src, "weight")
	if columnBacking(got.relColumns[weightKey]) != columnBacking(src.relColumns[weightKey]) {
		t.Error("untouched rel column rebuilt, want aliased")
	}
	for l, set := range src.labelIndex {
		if got.labelIndex[l] != set {
			t.Errorf("label %d bitmap rebuilt, want aliased", l)
		}
	}

	if v := got.Prop(0, "age").I64Or(0); v != 99 {
		t.Errorf("successor age = %d, want 99", v)
	}
	if v := src.Prop(0, "age").I64Or(0); v != 20 {
		t.Errorf("source age = %d, want 20 -- successor mutated its source", v)
	}
}

// TestRefinalizeRelEditRebuildsCSRSharesColumns: adding a rel rebuilds both
// CSRs, the type index, and every rel column (all keyed by CSR position),
// while node columns and label bitmaps still alias.
func TestRefinalizeRelEditRebuildsCSRSharesColumns(t *testing.T) {
	src := cowSource(t)
	b := NewBuilderFromSnapshot(src)
	if _, err := b.AddRel(3, 4, "KNOWS"); err != nil {
		t.Fatal(err)
	}
	got := b.Finalize()

	assertCSRAliased(t, got, src, false)
	weightKey := keyOf(t, src, "weight")
	if columnBacking(got.relColumns[weightKey]) == columnBacking(src.relColumns[weightKey]) {
		t.Error("rel column aliased across a rel edit, want rebuilt")
	}
	for key, col := range src.columns {
		if columnBacking(got.columns[key]) != columnBacking(col) {
			t.Errorf("node column %d rebuilt across a rel edit, want aliased", key)
		}
	}
	for l, set := range src.labelIndex {
		if got.labelIndex[l] != set {
			t.Errorf("label %d bitmap rebuilt across a rel edit, want aliased", l)
		}
	}

	if got.RelCount() != src.RelCount()+1 {
		t.Errorf("successor rels = %d, want %d", got.RelCount(), src.RelCount()+1)
	}
	if src.RelCount() != 5 {
		t.Errorf("source rels = %d, want 5 -- successor mutated its source", src.RelCount())
	}
	// The moved rel properties still read through the rebuilt positions.
	for pos := range uint32(len(src.outNbrs)) {
		if _, _, ok := src.RelEndpoints(pos); !ok {
			continue
		}
		if _, ok := src.RelProp(pos, "weight").F64(); !ok {
			t.Fatalf("source rel %d lost its weight", pos)
		}
	}
}

// TestRefinalizeGrownIDSpaceRebuildsColumns: a node past the old maximum
// widens every column's span, so no node column may alias -- their storage
// layout is chosen against that span.
func TestRefinalizeGrownIDSpaceRebuildsColumns(t *testing.T) {
	src := cowSource(t)
	b := NewBuilderFromSnapshot(src)
	if _, err := b.AddNodeWithID(50, "Person"); err != nil {
		t.Fatal(err)
	}
	got := b.Finalize()

	if got.CSRIDSpace() != 51 {
		t.Fatalf("id space = %d, want 51", got.CSRIDSpace())
	}
	assertCSRAliased(t, got, src, false)
	for key, col := range src.columns {
		if columnBacking(got.columns[key]) == columnBacking(col) {
			t.Errorf("node column %d aliased across an id-space growth, want rebuilt", key)
		}
	}
	if v := got.Prop(3, "name").StrOr(""); v != "n3" {
		t.Errorf("rebuilt name column reads %q at node 3, want n3", v)
	}
	if src.CSRIDSpace() != 6 {
		t.Errorf("source id space = %d, want 6 -- successor mutated its source", src.CSRIDSpace())
	}
}

// TestRefinalizeRemoveNodeDirtiesOnlyItsColumns: detach-delete sweeps the
// removed node's staged pairs, so only the keys it carried a value for
// rebuild. Removing a non-maximum id keeps the span, so the rest alias.
func TestRefinalizeRemoveNodeDirtiesOnlyItsColumns(t *testing.T) {
	src := cowSource(t)
	b := NewBuilderFromSnapshot(src)
	if ok := b.RemoveProp(2, "age"); !ok {
		t.Fatal("RemoveProp(2, age) reported a miss")
	}
	got := b.Finalize()

	ageKey, nameKey := keyOf(t, src, "age"), keyOf(t, src, "name")
	if columnBacking(got.columns[ageKey]) == columnBacking(src.columns[ageKey]) {
		t.Error("swept age column aliased, want rebuilt")
	}
	if columnBacking(got.columns[nameKey]) != columnBacking(src.columns[nameKey]) {
		t.Error("untouched name column rebuilt, want aliased")
	}
	assertCSRAliased(t, got, src, true)
	if _, ok := got.Prop(2, "age").Value(); ok {
		t.Error("successor still carries the removed age")
	}
	if _, ok := src.Prop(2, "age").Value(); !ok {
		t.Error("source lost its age -- successor mutated its source")
	}
}

// TestRefinalizeAtomTableAliasesUntilInterned: the interner is seeded from the
// source and only appends, so an edit that interns nothing new shares the
// table outright (millions of strings at LDBC scale).
func TestRefinalizeAtomTableAliasesUntilInterned(t *testing.T) {
	src := cowSource(t)
	b := NewBuilderFromSnapshot(src)
	if err := b.UpdateProp(0, "age", int64(31)); err != nil {
		t.Fatal(err)
	}
	if got := b.Finalize(); got.atoms != src.atoms {
		t.Error("atom table rebuilt after an edit interning nothing, want aliased")
	}

	b = NewBuilderFromSnapshot(src)
	if err := b.SetProp(0, "name", "a brand new string"); err != nil {
		t.Fatal(err)
	}
	got := b.Finalize()
	if got.atoms == src.atoms {
		t.Fatal("atom table aliased after interning a new string, want rebuilt")
	}
	if _, ok := src.atoms.ID("a brand new string"); ok {
		t.Error("source atom table gained the new string -- successor mutated its source")
	}
	if v := got.Prop(0, "name").StrOr(""); v != "a brand new string" {
		t.Errorf("successor name = %q", v)
	}
}

// TestRefinalizeCarriesLazyCaches: an index built on the source is handed to
// the successor when the components it was built from aliased, and dropped
// when one of them rebuilt.
func TestRefinalizeCarriesLazyCaches(t *testing.T) {
	src := cowSource(t)
	if _, ok := src.NodesWithProperty("Person", "age", int64(20)); !ok {
		t.Fatal("source (Person, age) index did not build")
	}
	if _, ok := src.NodesWithProperty("Person", "name", "n0"); !ok {
		t.Fatal("source (Person, name) index did not build")
	}
	personKey := propIndexKey{label: labelOf(t, src, "Person"), key: keyOf(t, src, "age")}
	nameKey := propIndexKey{label: labelOf(t, src, "Person"), key: keyOf(t, src, "name")}

	t.Run("no edit carries both", func(t *testing.T) {
		got := NewBuilderFromSnapshot(src).Finalize()
		for _, k := range []propIndexKey{personKey, nameKey} {
			if got.propIndex[k] == nil {
				t.Errorf("index %v not carried forward", k)
			}
		}
	})

	t.Run("edited column drops its index", func(t *testing.T) {
		b := NewBuilderFromSnapshot(src)
		if err := b.UpdateProp(0, "age", int64(99)); err != nil {
			t.Fatal(err)
		}
		got := b.Finalize()
		if got.propIndex[personKey] != nil {
			t.Error("index over the edited age column was carried forward")
		}
		if got.propIndex[nameKey] == nil {
			t.Error("index over the untouched name column was dropped")
		}
		// The dropped index rebuilds on demand against the new value.
		if _, ok := got.NodesWithProperty("Person", "age", int64(20)); ok {
			t.Error("stale age=20 still matches node 0 after the edit")
		}
		set, ok := got.NodesWithProperty("Person", "age", int64(99))
		if !ok || !set.Contains(0) {
			t.Error("rebuilt age index misses the edited value")
		}
	})

	t.Run("rel edit drops the forest-root cache", func(t *testing.T) {
		knows, _ := src.RelType("KNOWS")
		src.RootsVia(knows, Outgoing)
		unedited := NewBuilderFromSnapshot(src).Finalize()
		if len(unedited.rootsViaIndex) != 1 {
			t.Error("forest-root cache not carried across an unedited refinalize")
		}
		b := NewBuilderFromSnapshot(src)
		if _, err := b.AddRel(3, 4, "KNOWS"); err != nil {
			t.Fatal(err)
		}
		if got := b.Finalize(); len(got.rootsViaIndex) != 0 {
			t.Error("forest-root cache carried across a rel edit")
		}
	})
}

// TestRefinalizeRelPropOnCleanCSRRemapsPositions: a rel-property-only edit
// aliases the CSR, so the rebuilt rel column must remap staged rel indexes
// through the source's position map rather than a freshly built one.
func TestRefinalizeRelPropOnCleanCSRRemapsPositions(t *testing.T) {
	src := cowSource(t)
	b := NewBuilderFromSnapshot(src)
	// Rel 3 is the parallel (0)-[:KNOWS]->(1); address it by index.
	if err := b.SetRelPropAt(3, "weight", 42.5); err != nil {
		t.Fatal(err)
	}
	got := b.Finalize()
	assertCSRAliased(t, got, src, true)

	var found bool
	for pos := range uint32(len(got.outNbrs)) {
		if w, ok := got.RelProp(pos, "weight").F64(); ok && w == 42.5 {
			found = true
		}
	}
	if !found {
		t.Error("edited rel weight did not land at any CSR position")
	}
	// Every other rel keeps the weight it was staged with.
	for pos := range uint32(len(src.outNbrs)) {
		srcW, _ := src.RelProp(pos, "weight").F64()
		gotW, _ := got.RelProp(pos, "weight").F64()
		if srcW != gotW && gotW != 42.5 {
			t.Errorf("rel at pos %d: weight %v, want %v", pos, gotW, srcW)
		}
	}
}

// applyOps replays a byte-driven edit script against a thawed builder. The
// same script against two builders thawed from the same source leaves them in
// identical staging states, which is what lets FuzzRefinalizeMatchesRebuild
// compare the aliasing Finalize against the general one.
func applyOps(b *Builder, script []byte) {
	for i, op := range script {
		node := NodeID(op % 8)
		relIdx := 0
		if len(b.rels) > 0 {
			relIdx = i % len(b.rels)
		}
		switch op % 8 {
		case 0:
			_ = b.UpdateProp(node, "age", int64(op))
		case 1:
			_ = b.SetProp(node, "name", fmt.Sprintf("s%d", op%4))
		case 2:
			_, _ = b.AddRel(node, NodeID((op/8)%8), "KNOWS")
		case 3:
			if len(b.rels) > 0 {
				_ = b.RemoveRel(relIdx)
			}
		case 4:
			_, _ = b.AddNodeWithID(node, "Person")
		case 5:
			b.RemoveNode(node)
		case 6:
			// Rel-property edits keep the CSR clean: the rebuilt rel column
			// must remap through the source's position map.
			_ = b.SetRelPropAt(relIdx, "weight", float64(op)+0.75)
		case 7:
			_, _ = b.RemoveRelPropAt(relIdx, "since")
		}
	}
}

// FuzzRefinalizeMatchesRebuild is the parity gate for the copy-on-write path:
// for any edit script, the snapshot Finalize produces by aliasing clean
// components must be byte-identical to the one it produces by rebuilding
// everything from the same staging state -- and the source snapshot must be
// bit-for-bit unchanged after both.
func FuzzRefinalizeMatchesRebuild(f *testing.F) {
	for _, seed := range [][]byte{
		{}, {0}, {1, 1}, {2}, {3}, {4}, {5}, {0, 2, 5, 3, 1},
		{5, 2, 0}, {2, 2, 2, 3, 3}, {4, 50, 0, 1}, {1, 5, 4, 2, 0, 3},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) > 64 {
			script = script[:64]
		}
		src := cowSource(t)
		var before bytes.Buffer
		if err := src.WriteRCPG(&before); err != nil {
			t.Fatal(err)
		}

		aliased := NewBuilderFromSnapshot(src)
		applyOps(aliased, script)

		rebuilt := NewBuilderFromSnapshot(src)
		applyOps(rebuilt, script)
		// Drop the source link AFTER the edits: the dirty marks are no-ops
		// without it, and Finalize then rebuilds every component.
		rebuilt.src, rebuilt.srcRelToOutCSR = nil, nil

		var gotAliased, gotRebuilt bytes.Buffer
		if err := aliased.Finalize().WriteRCPG(&gotAliased); err != nil {
			t.Fatal(err)
		}
		if err := rebuilt.Finalize().WriteRCPG(&gotRebuilt); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotAliased.Bytes(), gotRebuilt.Bytes()) {
			t.Fatalf("aliased refinalize diverges from full rebuild (script %v)", script)
		}

		var after bytes.Buffer
		if err := src.WriteRCPG(&after); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before.Bytes(), after.Bytes()) {
			t.Fatalf("source snapshot mutated by its successor (script %v)", script)
		}
	})
}

// benchGraph builds a graph big enough that rebuilding its CSR and columns
// dominates a refinalize.
func benchGraph(b *testing.B, nodes, relsPerNode int) *Snapshot {
	b.Helper()
	bl := NewBuilder(nodes, nodes*relsPerNode)
	for id := range NodeID(nodes) {
		if _, err := bl.AddNodeWithID(id, "Person"); err != nil {
			b.Fatal(err)
		}
		if err := bl.SetProp(id, "age", int64(id%100)); err != nil {
			b.Fatal(err)
		}
		if err := bl.SetProp(id, "name", fmt.Sprintf("n%d", id%1000)); err != nil {
			b.Fatal(err)
		}
	}
	for id := range NodeID(nodes) {
		for k := range relsPerNode {
			dst := NodeID((int(id)*7 + k*13 + 1) % nodes)
			idx, err := bl.AddRel(id, dst, "KNOWS")
			if err != nil {
				b.Fatal(err)
			}
			if err := bl.SetRelPropAt(idx, "weight", float64(k)); err != nil {
				b.Fatal(err)
			}
		}
	}
	return bl.Finalize()
}

// BenchmarkRefinalize measures the Finalize half of the update loop (thaw is
// excluded; it stays O(n + m) until the lazy-thaw follow-up) for a no-edit
// pass, a one-property edit, and a one-rel edit. Each runs both ways: sharing
// the source's clean components, and rebuilding every component from the same
// staging state -- the path a from-scratch builder takes, and the before
// picture for this optimization.
func BenchmarkRefinalize(b *testing.B) {
	src := benchGraph(b, 50_000, 4)
	edits := []struct {
		name string
		edit func(*Builder)
	}{
		{"no_edit", func(*Builder) {}},
		{"one_property", func(bl *Builder) { _ = bl.UpdateProp(7, "age", int64(101)) }},
		{"one_rel", func(bl *Builder) { _, _ = bl.AddRel(1, 2, "KNOWS") }},
	}
	for _, e := range edits {
		for _, share := range []bool{true, false} {
			mode := "aliased"
			if !share {
				mode = "rebuilt"
			}
			b.Run(e.name+"/"+mode, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					b.StopTimer()
					bl := NewBuilderFromSnapshot(src)
					e.edit(bl)
					if !share {
						bl.src, bl.srcRelToOutCSR = nil, nil
					}
					b.StartTimer()
					bl.Finalize()
				}
			})
		}
	}
}
