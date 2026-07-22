package plan

import (
	"slices"
	"testing"
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
