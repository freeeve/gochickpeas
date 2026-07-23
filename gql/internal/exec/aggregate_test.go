package exec

import (
	"math"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// TestDistinctSetEntities covers the entity dedup contract: a node id and a
// relationship position of equal value never conflate (separate id spaces),
// and repeats within a kind are dropped.
func TestDistinctSetEntities(t *testing.T) {
	var d distinctSet
	var scratch []byte
	node := func(id uint32) bool { return d.add(value.Node(chickpeas.NodeID(id)), &scratch) }
	rel := func(pos uint32) bool { return d.add(value.Rel(pos), &scratch) }

	if !node(5) || node(5) {
		t.Fatal("node 5: first newly seen, repeat a duplicate")
	}
	if !node(7) {
		t.Fatal("node 7 is newly seen")
	}
	// A relationship with the same numeric id as a node is distinct.
	if !rel(5) {
		t.Fatal("rel 5 must not conflate with node 5")
	}
	if rel(5) {
		t.Fatal("rel 5 repeat is a duplicate")
	}
}

// TestDistinctSetOverflow drives the inline entity array (8 slots) past its
// capacity so it spills into the probe set, and checks dedup holds across
// the inline/spilled boundary.
func TestDistinctSetOverflow(t *testing.T) {
	var d distinctSet
	var scratch []byte
	const n = 20
	for i := uint32(0); i < n; i++ {
		if !d.add(value.Node(chickpeas.NodeID(i)), &scratch) {
			t.Fatalf("node %d should be newly seen", i)
		}
	}
	for i := uint32(0); i < n; i++ {
		if d.add(value.Node(chickpeas.NodeID(i)), &scratch) {
			t.Fatalf("node %d should be a duplicate after the fill", i)
		}
	}
}

// TestDistinctSetOtherKinds covers the non-entity byte-key store: scalars
// dedup by their kind-tagged key, so equal values collapse and different
// kinds stay distinct.
func TestDistinctSetOtherKinds(t *testing.T) {
	var d distinctSet
	var scratch []byte
	if !d.add(value.Int(3), &scratch) || d.add(value.Int(3), &scratch) {
		t.Fatal("int 3: first newly seen, repeat a duplicate")
	}
	if !d.add(value.Str("a"), &scratch) || d.add(value.Str("a"), &scratch) {
		t.Fatal("str a: first newly seen, repeat a duplicate")
	}
	// Distinct scalars and distinct kinds are all newly seen.
	if !d.add(value.Int(4), &scratch) || !d.add(value.Str("b"), &scratch) || !d.add(value.Bool(true), &scratch) {
		t.Fatal("int 4, str b, bool true are each newly seen")
	}
}

// TestPackedEntityAndGroupKey2 covers the entity group-key packers: the
// single-entity 31-bit pack (kind bit + id, out-of-range declines) and the
// order-sensitive pair form.
func TestPackedEntityAndGroupKey2(t *testing.T) {
	if e, ok := packedEntity30(value.Node(chickpeas.NodeID(5))); !ok || e != 5 {
		t.Fatalf("packedEntity30(node 5) = %d,%v, want 5", e, ok)
	}
	if e, ok := packedEntity30(value.Rel(7)); !ok || e != 1<<30|7 {
		t.Fatalf("packedEntity30(rel 7) = %d,%v", e, ok)
	}
	// Non-entity values and ids at/above 2^30 do not pack.
	if _, ok := packedEntity30(value.Int(3)); ok {
		t.Fatal("int must not pack as an entity")
	}
	if _, ok := packedEntity30(value.Node(chickpeas.NodeID(1 << 30))); ok {
		t.Fatal("id >= 2^30 must not pack")
	}

	// The pair form packs iff both sides pack, deterministically, and is
	// order-sensitive (node,rel differs from rel,node).
	k1, ok := packGroupKey2(value.Node(chickpeas.NodeID(5)), value.Rel(7))
	if !ok {
		t.Fatal("node,rel pair must pack")
	}
	if k2, _ := packGroupKey2(value.Node(chickpeas.NodeID(5)), value.Rel(7)); k2 != k1 {
		t.Fatal("pair packing must be deterministic")
	}
	if kSwap, _ := packGroupKey2(value.Rel(7), value.Node(chickpeas.NodeID(5))); kSwap == k1 {
		t.Fatal("pair packing must be order-sensitive")
	}
	if _, ok := packGroupKey2(value.Node(chickpeas.NodeID(5)), value.Int(3)); ok {
		t.Fatal("a non-entity second operand must not pack")
	}
	if _, ok := packGroupKey2(value.Int(1), value.Node(chickpeas.NodeID(2))); ok {
		t.Fatal("a non-entity first operand must not pack")
	}
}

// TestAggStateCountSumAvg pins the per-group accumulator arithmetic for the
// scalar kinds: count(*) counts nulls while count(expr) skips them, sum
// promotes to float on a mixed column and reports an int64-overflowing total
// as Null (the engine's no-error overflow policy), and avg is Null over an
// empty group.
func TestAggStateCountSumAvg(t *testing.T) {
	// count(*) folds every row, present=false, including nulls.
	star := &aggState{kind: plan.AggCount}
	star.update(value.Null(), false)
	star.update(value.Int(1), false)
	if c, _ := star.finalize().AsInt(); c != 2 {
		t.Fatalf("count(*) = %v, want 2", star.finalize())
	}
	// count(expr) skips null arguments, counts the rest.
	cnt := &aggState{kind: plan.AggCount}
	cnt.update(value.Null(), true)
	cnt.update(value.Int(7), true)
	cnt.update(value.Str("x"), true)
	if c, _ := cnt.finalize().AsInt(); c != 2 {
		t.Fatalf("count(expr) = %v, want 2 (null skipped)", cnt.finalize())
	}

	// An empty sum is Int(0), not Null.
	empty := &aggState{kind: plan.AggSum}
	if v, ok := empty.finalize().AsInt(); !ok || v != 0 {
		t.Fatalf("empty sum = %v, want Int(0)", empty.finalize())
	}
	// An all-int sum stays Int.
	si := &aggState{kind: plan.AggSum}
	for _, x := range []int64{2, 3, 5} {
		si.update(value.Int(x), true)
	}
	if v, ok := si.finalize().AsInt(); !ok || v != 10 {
		t.Fatalf("int sum = %v, want Int(10)", si.finalize())
	}
	// A mixed int+float column promotes the total to Float.
	mix := &aggState{kind: plan.AggSum}
	mix.update(value.Int(4), true)
	mix.update(value.Float(0.5), true)
	if f, ok := mix.finalize().AsFloat(); !ok || math.Abs(f-4.5) > 1e-12 {
		t.Fatalf("mixed sum = %v, want Float(4.5)", mix.finalize())
	}
	// A total outside int64 range is Null (no per-row overflow error).
	ov := &aggState{kind: plan.AggSum}
	ov.update(value.Int(math.MaxInt64), true)
	ov.update(value.Int(math.MaxInt64), true)
	if !ov.finalize().IsNull() {
		t.Fatalf("overflowing sum = %v, want Null", ov.finalize())
	}

	// avg is Null over an empty group, else the arithmetic mean.
	ea := &aggState{kind: plan.AggAvg}
	if !ea.finalize().IsNull() {
		t.Fatalf("empty avg = %v, want Null", ea.finalize())
	}
	av := &aggState{kind: plan.AggAvg}
	for _, x := range []int64{2, 4, 9} {
		av.update(value.Int(x), true)
	}
	if f, ok := av.finalize().AsFloat(); !ok || math.Abs(f-5.0) > 1e-12 {
		t.Fatalf("avg = %v, want Float(5)", av.finalize())
	}
}

// TestAggStateStddevWelford pins the single-pass Welford stddev: sample
// stddev is 0 for fewer than two values (Neo4j semantics) and the unbiased
// sample deviation otherwise, while population stddev is 0 on empty and the
// biased deviation otherwise. Fixture {2,4,4,4,5,5,7,9}: mean 5, squared
// deviations sum 32, so pop var 4 (stddev 2) and sample var 32/7.
func TestAggStateStddevWelford(t *testing.T) {
	feed := func(k plan.AggKind, xs ...float64) *aggState {
		s := &aggState{kind: k}
		for _, x := range xs {
			s.update(value.Float(x), true)
		}
		return s
	}
	approx := func(name string, got value.Value, want float64) {
		t.Helper()
		f, ok := got.AsFloat()
		if !ok || math.Abs(f-want) > 1e-9 {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
	}

	set := []float64{2, 4, 4, 4, 5, 5, 7, 9}
	// Sample stddev needs at least two values; below that it is 0.
	approx("stddevSamp empty", feed(plan.AggStddevSamp).finalize(), 0)
	approx("stddevSamp single", feed(plan.AggStddevSamp, 3).finalize(), 0)
	approx("stddevSamp set", feed(plan.AggStddevSamp, set...).finalize(), math.Sqrt(32.0/7.0))
	// Population stddev is 0 on empty and the biased deviation otherwise.
	approx("stddevPop empty", feed(plan.AggStddevPop).finalize(), 0)
	approx("stddevPop set", feed(plan.AggStddevPop, set...).finalize(), 2)
}

// TestAggregatorSlabKinds drives the aggregator's off-struct slab kinds
// through a global aggregation: min/max read the extremum slab (mmOf),
// count(DISTINCT) the dedup slab (seenOf), and collect the item slab
// (itemsOf) -- paths the scalar-accumulator tests never touch.
func TestAggregatorSlabKinds(t *testing.T) {
	bld := chickpeas.NewBuilder(8, 0)
	for _, p := range []struct{ v, g int64 }{{10, 1}, {20, 1}, {30, 2}} {
		n, err := bld.AddNode("A")
		if err != nil {
			t.Fatal(err)
		}
		_ = bld.SetProp(n, "v", p.v)
		_ = bld.SetProp(n, "g", p.g)
	}
	g := graph.New(bld.Finalize("v", "g"))
	ctx := &eval.Ctx{G: g}

	q, err := parser.Parse("MATCH (a:A) RETURN min(a.v) AS mn, max(a.v) AS mx, count(DISTINCT a.g) AS cd, collect(a.v) AS c")
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(q, g)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Execute(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("global aggregate rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if mn, _ := r[0].AsInt(); mn != 10 {
		t.Fatalf("min(v) = %v, want 10", r[0])
	}
	if mx, _ := r[1].AsInt(); mx != 30 {
		t.Fatalf("max(v) = %v, want 30", r[1])
	}
	if cd, _ := r[2].AsInt(); cd != 2 {
		t.Fatalf("count(DISTINCT g) = %v, want 2", r[2])
	}
	if xs, ok := r[3].AsList(); !ok || len(xs) != 3 {
		t.Fatalf("collect(v) = %v, want a 3-element list", r[3])
	}
}
