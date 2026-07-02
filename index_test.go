package chickpeas_test

import (
	"math"
	"slices"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

func cityFixture(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(8, 2)
	add := func(id chickpeas.NodeID, name string, lat, lon float64) {
		b.AddNodeWithID(id, "City")
		b.SetProp(id, "desc", name)
		b.SetProp(id, "lat", lat)
		b.SetProp(id, "lon", lon)
	}
	add(1, "rainy London calling", 51.5074, -0.1278)
	add(2, "Paris the city of light", 48.8566, 2.3522)
	add(3, "New York city that never sleeps", 40.7128, -74.0060)
	add(4, "sunny Sydney harbour city", -33.8688, 151.2093)
	// A non-city with matching text must not leak into City searches.
	b.AddNodeWithID(9, "Other")
	b.SetProp(9, "desc", "London city")
	return b.Finalize()
}

func TestFullTextSearch(t *testing.T) {
	g := cityFixture(t)
	hits := g.FullTextSearch("City", "desc", "city")
	if !slices.Equal(hits.ToSlice(), []uint32{2, 3, 4}) {
		t.Fatalf("city hits: %v", hits.ToSlice())
	}
	// Conjunctive, case-insensitive.
	if got := g.FullTextSearch("City", "desc", "CITY light"); !slices.Equal(got.ToSlice(), []uint32{2}) {
		t.Fatalf("conjunctive: %v", got.ToSlice())
	}
	// Label scoping: node 9 matches "london" textually but not by label.
	if got := g.FullTextSearch("City", "desc", "london"); !slices.Equal(got.ToSlice(), []uint32{1}) {
		t.Fatalf("label scope: %v", got.ToSlice())
	}
	// Unknowns and empties.
	if !g.FullTextSearch("City", "desc", "").IsEmpty() ||
		!g.FullTextSearch("City", "desc", "zzz").IsEmpty() ||
		!g.FullTextSearch("Nope", "desc", "city").IsEmpty() ||
		!g.FullTextSearch("City", "nope", "city").IsEmpty() {
		t.Fatal("empty cases leaked")
	}

	// Ranked: "city" appears in 3 docs; repeated tokens rank higher via tf.
	ranked := g.FullTextSearchRanked("City", "desc", "city sleeps", 10)
	if len(ranked) != 3 || ranked[0].Node != 3 {
		t.Fatalf("ranked: %+v", ranked)
	}
	if got := g.FullTextSearchRanked("City", "desc", "city", 1); len(got) != 1 {
		t.Fatalf("top-k: %+v", got)
	}
}

func TestFullTextFieldDirect(t *testing.T) {
	f := chickpeas.BuildFullTextField(func(yield func(uint32, string) bool) {
		yield(5, "kiwi")
		yield(3, "kiwi")
		yield(9, "kiwi")
	})
	// Identical docs tie; ties break by ascending node id.
	ranked := f.QueryRanked("kiwi", 10)
	ids := []uint32{ranked[0].Node, ranked[1].Node, ranked[2].Node}
	if !slices.Equal(ids, []uint32{3, 5, 9}) {
		t.Fatalf("tie order: %v", ids)
	}
	if f.TermCount() != 1 || f.DocCount() != 3 {
		t.Fatalf("counts: %d/%d", f.TermCount(), f.DocCount())
	}
	if got := chickpeas.Tokenize("Hello, WORLD! it's foo_bar"); !slices.Equal(got, []string{"hello", "world", "it", "s", "foo", "bar"}) {
		t.Fatalf("tokenize: %v", got)
	}
}

func TestGeoSearch(t *testing.T) {
	g := cityFixture(t)
	// 400 km of London: London + Paris.
	got := g.GeoWithinRadius("City", "lat", "lon", 51.5074, -0.1278, 400)
	if !slices.Equal(got.ToSlice(), []uint32{1, 2}) {
		t.Fatalf("radius: %v", got.ToSlice())
	}
	if got := g.GeoWithinRadius("City", "lat", "lon", 51.5074, -0.1278, 100); !slices.Equal(got.ToSlice(), []uint32{1}) {
		t.Fatalf("tight radius: %v", got.ToSlice())
	}
	// KNN from London: London itself then Paris, distances near haversine.
	knn := g.GeoKNN("City", "lat", "lon", 51.5074, -0.1278, 2)
	if len(knn) != 2 || knn[0].Node != 1 || knn[1].Node != 2 {
		t.Fatalf("knn: %+v", knn)
	}
	want := chickpeas.HaversineKM(51.5074, -0.1278, 48.8566, 2.3522)
	if math.Abs(knn[1].KM-want) > 0.5 {
		t.Fatalf("knn distance %v, haversine %v", knn[1].KM, want)
	}
	// Western Europe bbox.
	if got := g.GeoWithinBBox("City", "lat", "lon", 40, -10, 55, 10); !slices.Equal(got.ToSlice(), []uint32{1, 2}) {
		t.Fatalf("bbox: %v", got.ToSlice())
	}
	// Unknown label/keys are empty, not a panic.
	if !g.GeoWithinRadius("Nope", "lat", "lon", 0, 0, 10).IsEmpty() ||
		g.GeoKNN("City", "nope", "lon", 0, 0, 3) != nil {
		t.Fatal("unknown geo lookups leaked")
	}
}

func TestGeoAntimeridianAndInvalid(t *testing.T) {
	idx := chickpeas.BuildGeoIndex(func(yield func(uint32, float64, float64) bool) {
		yield(1, 0, 179)
		yield(2, 0, -179)
		yield(3, 0, 0)
		yield(4, math.NaN(), 0) // skipped
		yield(5, 200, 0)        // skipped
	})
	if idx.Len() != 3 {
		t.Fatalf("len: %d", idx.Len())
	}
	// Radius across the antimeridian (~222 km apart).
	if got := idx.WithinRadius(0, 179, 300); !slices.Equal(got.ToSlice(), []uint32{1, 2}) {
		t.Fatalf("antimeridian radius: %v", got.ToSlice())
	}
	if got := idx.WithinRadius(0, 179, 100); !slices.Equal(got.ToSlice(), []uint32{1}) {
		t.Fatalf("tight antimeridian: %v", got.ToSlice())
	}
	// Wrapping bbox.
	if got := idx.WithinBBox(-10, 170, 10, -170); !slices.Equal(got.ToSlice(), []uint32{1, 2}) {
		t.Fatalf("wrap bbox: %v", got.ToSlice())
	}
	// Degenerate queries.
	if !idx.WithinRadius(0, 0, -1).IsEmpty() || idx.KNN(0, 0, 0) != nil {
		t.Fatal("degenerate queries leaked")
	}
	if math.Abs(chickpeas.HaversineKM(51.5074, -0.1278, 48.8566, 2.3522)-343.5) > 3 {
		t.Fatal("haversine off")
	}
}

func TestManager(t *testing.T) {
	m := chickpeas.NewManager()
	if m.Len() != 0 {
		t.Fatal("new manager not empty")
	}
	b := chickpeas.NewBuilder(2, 0)
	b.AddNodeWithID(0, "V")
	b.SetVersion("v1")
	m.AddSnapshot(b.Finalize())

	unversioned := chickpeas.NewBuilder(2, 0)
	unversioned.AddNodeWithID(0, "V")
	m.AddSnapshot(unversioned.Finalize())

	if m.Len() != 2 {
		t.Fatalf("len: %d", m.Len())
	}
	if _, ok := m.Snapshot("v1"); !ok {
		t.Fatal("v1 missing")
	}
	if _, ok := m.Snapshot(chickpeas.LatestVersion); !ok {
		t.Fatal("latest missing")
	}
	versions := m.Versions()
	slices.Sort(versions)
	if !slices.Equal(versions, []string{"latest", "v1"}) {
		t.Fatalf("versions: %v", versions)
	}
	if !m.RemoveSnapshot("v1") || m.RemoveSnapshot("v1") {
		t.Fatal("remove semantics wrong")
	}
	m.Clear()
	if m.Len() != 0 {
		t.Fatal("clear failed")
	}
}
