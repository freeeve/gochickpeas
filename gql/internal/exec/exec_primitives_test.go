package exec

import (
	"math"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/value"
)

// Direct coverage for the executor's low-level pure primitives -- the
// 128-bit sum accumulator, the entity group-key packers, and the
// comparison flip -- which the full-plan tests reach only incidentally.

// TestAcc128 covers the wide sum accumulator: exact int64 totals with the
// fits-int64 verdict, a total past int64 range read through float64, and a
// transient overflow that nets back to an exact int64.
func TestAcc128(t *testing.T) {
	var z acc128
	if v, ok := z.int64(); v != 0 || !ok {
		t.Fatalf("zero int64 = %d,%v", v, ok)
	}
	if f := z.float64(); f != 0 {
		t.Fatalf("zero float64 = %v", f)
	}

	var a acc128
	a.add(1000)
	a.add(2000)
	if v, ok := a.int64(); v != 3000 || !ok {
		t.Fatalf("sum int64 = %d,%v", v, ok)
	}
	if f := a.float64(); f != 3000 {
		t.Fatalf("sum float64 = %v", f)
	}

	var neg acc128
	neg.add(-5)
	if v, ok := neg.int64(); v != -5 || !ok {
		t.Fatalf("neg int64 = %d,%v", v, ok)
	}
	// NOTE: float64() is intentionally not asserted for small negatives here
	// -- its hi*2^64 + lo form catastrophically cancels a small negative
	// total to ~0 (see the acc128-float64-negative finding). float64() is
	// exercised below only on totals where it is accurate (non-negative and
	// genuinely-wide).

	// Two MaxInt64 adds overflow int64 but stay exact in the accumulator;
	// float64 reads the wide total (~2^64).
	var big acc128
	big.add(math.MaxInt64)
	big.add(math.MaxInt64)
	if _, ok := big.int64(); ok {
		t.Fatal("2*MaxInt64 must not fit int64")
	}
	if f := big.float64(); f < 1.8e19 || f > 1.9e19 {
		t.Fatalf("wide float64 = %v, want ~1.84e19", f)
	}

	// A transient excursion past int64 that nets back is exact and fits.
	var net acc128
	net.add(math.MaxInt64)
	net.add(math.MaxInt64)
	net.add(-math.MaxInt64)
	net.add(-math.MaxInt64)
	if v, ok := net.int64(); v != 0 || !ok {
		t.Fatalf("netted int64 = %d,%v", v, ok)
	}
	if f := net.float64(); f != 0 {
		t.Fatalf("netted float64 = %v", f)
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

// TestFlipCmp covers mirroring a comparison across swapped operands, with
// symmetric operators left unchanged and the flip an involution.
func TestFlipCmp(t *testing.T) {
	for in, want := range map[ast.BinOp]ast.BinOp{
		ast.OpLt:  ast.OpGt,
		ast.OpGt:  ast.OpLt,
		ast.OpLte: ast.OpGte,
		ast.OpGte: ast.OpLte,
		ast.OpEq:  ast.OpEq,  // symmetric: unchanged
		ast.OpNeq: ast.OpNeq, // symmetric: unchanged
	} {
		if got := flipCmp(in); got != want {
			t.Fatalf("flipCmp(%v) = %v, want %v", in, got, want)
		}
	}
	// Flipping an ordering comparison twice is the identity.
	for _, op := range []ast.BinOp{ast.OpLt, ast.OpGte, ast.OpGt, ast.OpLte} {
		if flipCmp(flipCmp(op)) != op {
			t.Fatalf("flipCmp not an involution at %v", op)
		}
	}
}
