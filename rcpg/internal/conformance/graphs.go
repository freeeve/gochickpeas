// The corpus graph builders, mirroring rustychickpeas-format's
// conformance.rs case by case (same atoms, same rels, same xorshift seeds).

package conformance

import (
	"math"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/freeeve/gochickpeas/rcpg"
)

func smallGraph() *rcpg.GraphSection {
	// atoms: 0="", 1=Person, 2=KNOWS, 3=name, 4=alice, 5=bob
	g := buildCSR(2, []rel{{0, 1, 2}, {1, 0, 2}})
	g.NNodes = 2
	g.Atoms = []string{"", "Person", "KNOWS", "name", "alice", "bob"}
	g.LabelIndex = []rcpg.BitmapEntry{{Atom: 1, Bitmap: bitmap(0, 1)}}
	g.NodeColumns = []rcpg.Column{{Key: 3, Data: rcpg.DenseStr{4, 5}}}
	g.RelColumns = []rcpg.Column{{Key: 3, Data: rcpg.SparseI64{{ID: 0, Val: 42}}}}
	g.Version = strp("conformance-v1")
	return g
}

func sparseIDsGraph() *rcpg.GraphSection {
	// atoms: 0="", 1=Thing, 2=REL, 3=weight, 4=tag, 5=hi
	g := buildCSR(65_001, []rel{{0, 65_000, 2}, {1000, 5, 2}, {0, 5, 2}})
	g.NNodes = 4
	g.Atoms = []string{"", "Thing", "REL", "weight", "tag", "hi"}
	g.LabelIndex = []rcpg.BitmapEntry{{Atom: 1, Bitmap: bitmap(0, 5, 1000, 65_000)}}
	g.NodeColumns = []rcpg.Column{
		{Key: 3, Data: rcpg.SparseI64{{ID: 0, Val: -1}, {ID: 5, Val: 500}, {ID: 65_000, Val: math.MaxInt64}}},
		{Key: 4, Data: rcpg.SparseStr{{ID: 1000, Val: 5}, {ID: 65_000, Val: 0}}},
	}
	return g
}

func allColumnsGraph() *rcpg.GraphSection {
	// 13 nodes (dense bool length not a multiple of 8), 5 rels.
	g := buildCSR(13, []rel{{0, 1, 2}, {1, 2, 2}, {2, 0, 2}, {3, 4, 2}, {12, 0, 2}})
	g.NNodes = 13
	g.Atoms = []string{"", "N", "T", "di", "df", "db", "ds", "si", "sf", "sb", "ss", "v1"}
	g.LabelIndex = []rcpg.BitmapEntry{{Atom: 1, Bitmap: bitmapRange(0, 13)}}
	nanPayload := math.Float64frombits(0x7FF8_DEAD_BEEF_0001)
	// Rust's f64::NAN is 0x7FF8...0000; Go's math.NaN() is 0x7FF8...0001.
	// The corpus is defined by the Rust bits.
	rustNaN := math.Float64frombits(0x7FF8_0000_0000_0000)
	di := make(rcpg.DenseI64, 13)
	for i := range di {
		di[i] = (int64(i) - 6) * 1_000_000_007
	}
	db := rcpg.NewDenseBool(13)
	for _, i := range []uint32{0, 2, 3, 5, 7, 11, 12} {
		db.Set(i, true)
	}
	g.NodeColumns = []rcpg.Column{
		{Key: 3, Data: di},
		{Key: 4, Data: rcpg.DenseF64{
			0.0, math.Copysign(0, -1), math.Inf(1), math.Inf(-1), rustNaN,
			nanPayload, 1.5, -2.5, minPositive(), math.MaxFloat64,
			eps(), 3.14, -1e-300,
		}},
		{Key: 5, Data: db},
		// atom 0 entries = missing values in a dense str column.
		{Key: 6, Data: rcpg.DenseStr{11, 0, 11, 0, 0, 11, 0, 11, 11, 0, 11, 0, 11}},
		{Key: 7, Data: rcpg.SparseI64{{ID: 0, Val: math.MinInt64}, {ID: 6, Val: 0}, {ID: 12, Val: math.MaxInt64}}},
		{Key: 8, Data: rcpg.SparseF64{{ID: 1, Val: math.Copysign(0, -1)}, {ID: 5, Val: nanPayload}, {ID: 9, Val: 2.25}}},
		{Key: 9, Data: rcpg.SparseBool{{ID: 2, Val: true}, {ID: 3, Val: false}, {ID: 10, Val: true}}},
		{Key: 10, Data: rcpg.SparseStr{{ID: 4, Val: 11}, {ID: 8, Val: 0}}},
	}
	// Rel columns index by outgoing-CSR position (0..5), not node id.
	g.RelColumns = []rcpg.Column{
		{Key: 3, Data: rcpg.DenseI64{10, 20, 30, 40, 50}},
		{Key: 4, Data: rcpg.DenseF64{0.5, rustNaN, math.Copysign(0, -1), 4.0, 5.0}},
		{Key: 10, Data: rcpg.SparseStr{{ID: 0, Val: 11}, {ID: 4, Val: 11}}},
	}
	g.Version = strp("") // present-but-empty differs from absent on disk
	return g
}

// eps is f64::EPSILON (2^-52), Rust's machine epsilon constant.
func eps() float64 {
	return math.Float64frombits(0x3CB0_0000_0000_0000)
}

// minPositive mirrors Rust's f64::MIN_POSITIVE (smallest positive normal).
func minPositive() float64 {
	return math.Float64frombits(0x0010_0000_0000_0000)
}

func multiLabelTypesGraph() *rcpg.GraphSection {
	// atoms: 0="", 1=A, 2=B, 3=LOOP, 4=DUP, 5=OTHER
	g := buildCSR(3, []rel{
		{0, 0, 3}, // self-loop
		{1, 2, 4}, // parallel pair (same endpoints, same type, twice)
		{1, 2, 4},
		{1, 2, 5}, // same endpoints, different type
		{2, 1, 4}, // reverse direction of the pair
	})
	g.NNodes = 3
	g.Atoms = []string{"", "A", "B", "LOOP", "DUP", "OTHER"}
	g.LabelIndex = []rcpg.BitmapEntry{
		{Atom: 1, Bitmap: bitmap(0, 1)},
		{Atom: 2, Bitmap: bitmap(1, 2)},
	}
	g.RelColumns = []rcpg.Column{{Key: 5, Data: rcpg.DenseI64{1, 2, 3, 4, 5}}}
	return g
}

// xorshift matches the Rust generator's xorshift64 stream.
type xorshift struct{ s uint64 }

func (x *xorshift) next() uint64 {
	x.s ^= x.s << 13
	x.s ^= x.s >> 7
	x.s ^= x.s << 17
	return x.s
}

func bigGraph() *rcpg.GraphSection {
	const n = 20_000
	const m = 100_000
	rng := &xorshift{s: 0x9E37_79B9_7F4A_7C15}
	rels := make([]rel, 0, m)
	for range m {
		u := uint32(rng.next() % n)
		v := uint32(rng.next() % n)
		t := 3 + uint32(rng.next()%3) // atoms 3..=5
		rels = append(rels, rel{u, v, t})
	}
	g := buildCSR(n, rels)
	g.NNodes = n
	g.Atoms = []string{"", "Node", "Even", "KNOWS", "LIKES", "FOLLOWS", "score", "tag", "x"}
	// "Node" over the full contiguous range, run-optimized; "Even" (10k ids
	// in chunk 0) stays a bitmap container.
	all := bitmapRange(0, n)
	all.RunOptimize()
	even := roaring.New()
	for i := uint32(0); i < n; i += 2 {
		even.Add(i)
	}
	g.LabelIndex = []rcpg.BitmapEntry{{Atom: 1, Bitmap: all}, {Atom: 2, Bitmap: even}}
	score := &xorshift{s: 42}
	di := make(rcpg.DenseI64, n)
	for i := range di {
		di[i] = int64(score.next())
	}
	var ss rcpg.SparseStr
	for i := uint32(0); i < n; i += 97 {
		ss = append(ss, rcpg.StrEntry{ID: i, Val: 8})
	}
	g.NodeColumns = []rcpg.Column{{Key: 6, Data: di}, {Key: 7, Data: ss}}
	weights := make(rcpg.DenseF64, m)
	for i := range weights {
		weights[i] = float64(score.next()%10_000) / 100.0
	}
	g.RelColumns = []rcpg.Column{{Key: 6, Data: weights}}
	g.Version = strp("big-sf0")
	return g
}
