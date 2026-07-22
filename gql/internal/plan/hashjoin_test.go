package plan

import (
	"slices"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// TestFreshBind covers the slot a BindOp newly binds: a scan binds its slot
// unless it reuses a carried-in arg (nothing fresh), and an expand binds its
// target slot unless that target is a rebind (join/cycle/carried-in).
func TestFreshBind(t *testing.T) {
	// A scan of a bound arg introduces no fresh variable.
	if got := freshBind(&BindOp{Kind: OpScan, Slot: 4, Source: ScanSource{Kind: ScanArg}}); got != -1 {
		t.Fatalf("scan-arg fresh = %d, want -1", got)
	}
	// A real scan binds its own slot.
	if got := freshBind(&BindOp{Kind: OpScan, Slot: 4, Source: ScanSource{Kind: ScanLabel}}); got != 4 {
		t.Fatalf("label-scan fresh = %d, want 4", got)
	}
	// An expand onto an already-bound target binds nothing fresh.
	if got := freshBind(&BindOp{Kind: OpExpand, To: 7, Rebind: true}); got != -1 {
		t.Fatalf("rebind expand fresh = %d, want -1", got)
	}
	// An expand onto a fresh target binds it.
	if got := freshBind(&BindOp{Kind: OpExpand, To: 7}); got != 7 {
		t.Fatalf("fresh expand fresh = %d, want 7", got)
	}
}

// TestOpReads covers the slots a BindOp reads before binding: an arg /
// node-id-var scan reads its source slot, an exists-seed scan reads every
// seed chain's anchor slot, a label/all scan reads nothing, and an expand
// reads its from-slot plus the to-slot when it rebinds.
func TestOpReads(t *testing.T) {
	eq := func(name string, got, want []int) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Fatalf("%s reads = %v, want %v", name, got, want)
		}
	}
	eq("scan-arg", opReads(&BindOp{Kind: OpScan, Source: ScanSource{Kind: ScanArg, Slot: 3}}, nil), []int{3})
	eq("scan-nodeidvar", opReads(&BindOp{Kind: OpScan, Source: ScanSource{Kind: ScanNodeIDVar, Slot: 5}}, nil), []int{5})
	eq("scan-existsseed", opReads(&BindOp{Kind: OpScan, Source: ScanSource{
		Kind: ScanExistsSeed, Seeds: []SeedChain{{AnchorSlot: 2}, {AnchorSlot: 6}}}}, nil), []int{2, 6})

	// A label scan reads nothing (its candidates come from the index).
	if got := opReads(&BindOp{Kind: OpScan, Source: ScanSource{Kind: ScanLabel}}, nil); len(got) != 0 {
		t.Fatalf("label-scan reads = %v, want none", got)
	}

	eq("expand", opReads(&BindOp{Kind: OpExpand, From: 1}, nil), []int{1})
	eq("expand-rebind", opReads(&BindOp{Kind: OpExpand, From: 1, To: 9, Rebind: true}, nil), []int{1, 9})

	// out is appended to, not overwritten.
	eq("appends", opReads(&BindOp{Kind: OpExpand, From: 4}, []int{0}), []int{0, 4})
}

// TestAndWith covers conjoining onto a possibly-nil base.
func TestAndWith(t *testing.T) {
	extra := &ast.Var{Name: "e"}
	if got := andWith(nil, extra); got != ast.Expr(extra) {
		t.Fatal("nil base should return extra unchanged")
	}
	base := &ast.Var{Name: "b"}
	got, ok := andWith(base, extra).(*ast.Binary)
	if !ok || got.Op != ast.OpAnd || got.LHS != ast.Expr(base) || got.RHS != ast.Expr(extra) {
		t.Fatalf("andWith(base, extra) = %#v", got)
	}
}

// TestAndJoin covers folding a conjunct list into a left-leaning AND tree.
func TestAndJoin(t *testing.T) {
	if andJoin(nil) != nil {
		t.Fatal("empty conjuncts fold to nil")
	}
	a := &ast.Var{Name: "a"}
	if got := andJoin([]ast.Expr{a}); got != ast.Expr(a) {
		t.Fatal("single conjunct folds to itself")
	}
	b, c := &ast.Var{Name: "b"}, &ast.Var{Name: "c"}
	// Left-folded: ((a AND b) AND c).
	top, ok := andJoin([]ast.Expr{a, b, c}).(*ast.Binary)
	if !ok || top.Op != ast.OpAnd || top.RHS != ast.Expr(c) {
		t.Fatalf("andJoin top = %#v", top)
	}
	inner, ok := top.LHS.(*ast.Binary)
	if !ok || inner.LHS != ast.Expr(a) || inner.RHS != ast.Expr(b) {
		t.Fatalf("andJoin inner = %#v", inner)
	}
}

// TestConjSlotRefs covers resolving a conjunct's referenced segment slots,
// sorted, with names absent from the slot map ignored.
func TestConjSlotRefs(t *testing.T) {
	slots := map[string]int{"x": 5, "y": 2}
	e := &ast.Binary{Op: ast.OpEq, LHS: &ast.Prop{Var: "x", Key: "k"}, RHS: &ast.Var{Name: "y"}}
	if got := conjSlotRefs(e, slots); !slices.Equal(got, []int{2, 5}) {
		t.Fatalf("conjSlotRefs = %v, want [2 5]", got)
	}
	// A variable not in the slot map (e.g. a subquery local) is ignored.
	if got := conjSlotRefs(&ast.Var{Name: "unknown"}, slots); len(got) != 0 {
		t.Fatalf("unknown var slots = %v", got)
	}
}
