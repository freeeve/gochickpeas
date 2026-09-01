// Cross-segment scan CSE: when a later segment's bare label scan repeats
// an earlier segment's -- same label, same variable, and a WHERE whose
// conjuncts extend the earlier one's -- the two share one filtered pass
// at execution. The earlier (recorder) segment's fused columnar pass
// publishes its survivor ids on the eval context; the later (consumer)
// seeds from them and evaluates only the residual conjuncts (the NEXT
// idiom that computes a whole-set aggregate, then re-scans the same set
// with a stricter filter). Detection is shape-generic: it compares
// label, variable, and conjunct structure, never query identity.
// Auto-lifted parameter slots compare conditionally -- a cached template
// rebinds slot values per run, so the consumer validates the recorded
// slot pairs hold equal values before trusting the share.
package plan

import (
	"reflect"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
)

// DisableScanCSE pins differential tests to the unshared path; the
// marking pass is skipped entirely, so both segments plan and execute
// their full scans.
var DisableScanCSE bool

// linkScanCSE pairs each qualifying consumer segment with the first
// earlier recorder whose conjuncts it subsumes. The recorder must carry
// at least one conjunct (sharing an unfiltered label walk saves
// nothing), and every recorder conjunct must be row-independent -- free
// variables limited to the scanned one -- so the survivor set is a pure
// function of the graph.
func linkScanCSE(segs []*Segment) {
	if DisableScanCSE {
		return
	}
	for j := 1; j < len(segs); j++ {
		cj, ok := cseScanOf(segs[j])
		if !ok {
			continue
		}
		for i := 0; i < j; i++ {
			ci, ok := cseScanOf(segs[i])
			if !ok || ci.label != cj.label || ci.varName != cj.varName ||
				len(ci.conjs) == 0 || !cseRowIndependent(ci) {
				continue
			}
			residual, conds, ok := cseSubsumes(ci.conjs, cj.conjs)
			if !ok {
				continue
			}
			segs[i].CSERecord = true
			segs[j].CSEFrom = segs[i]
			segs[j].CSEResidual = rebuildAnd(residual)
			segs[j].CSEParamConds = conds
			break
		}
	}
}

// cseScan is one segment's scan identity for the pairing walk.
type cseScan struct {
	label   string
	varName string
	conjs   []ast.Expr
}

// cseScanOf extracts the scan identity of a colagg-marked, non-optional
// single-label scan head. OPTIONAL declines: the fused pass's null-fill
// row semantics make its survivor set an unsound seed.
func cseScanOf(seg *Segment) (cseScan, bool) {
	if !seg.ColAgg || !colAggScanStage(seg) {
		return cseScan{}, false
	}
	ms := seg.Stages[0].(*MatchStage)
	if ms.Optional {
		return cseScan{}, false
	}
	op := &ms.Ops[0]
	varName := ""
	for nm, s := range seg.Slots {
		if s == op.Slot {
			varName = nm
		}
	}
	if varName == "" {
		return cseScan{}, false
	}
	var conjs []ast.Expr
	if ms.Where != nil {
		SplitAnd(ms.Where, &conjs)
	}
	return cseScan{label: op.Source.Label, varName: varName, conjs: conjs}, true
}

// cseRowIndependent reports whether every conjunct's free variables are
// limited to the scanned one (carried columns would make the survivor
// set depend on the segment's input row).
func cseRowIndependent(c cseScan) bool {
	for _, e := range c.conjs {
		if len(freeVarsOutside(e, []string{c.varName})) != 0 {
			return false
		}
	}
	return true
}

// cseSubsumes matches every recorder conjunct to a distinct consumer
// conjunct, returning the consumer's unmatched remainder and the
// parameter-slot conditions the matches depend on.
func cseSubsumes(rec, cons []ast.Expr) ([]ast.Expr, [][2]uint32, bool) {
	used := make([]bool, len(cons))
	var conds [][2]uint32
	for _, a := range rec {
		found := false
		for k, b := range cons {
			if used[k] {
				continue
			}
			trial := conds
			if equalModParams(a, b, &trial) {
				used[k] = true
				conds = trial
				found = true
				break
			}
		}
		if !found {
			return nil, nil, false
		}
	}
	var residual []ast.Expr
	for k, b := range cons {
		if !used[k] {
			residual = append(residual, b)
		}
	}
	return residual, conds, true
}

// equalModParams reports structural equality of two expressions,
// treating a pair of auto-lifted parameter slots as equal SUBJECT TO
// their runtime values matching: each differing pair is appended to
// conds for the executor to validate per run. A param against anything
// else is unequal (the baked side is a fixed constant; the lifted side
// rebinds).
func equalModParams(a, b ast.Expr, conds *[][2]uint32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if la, ok := a.(*ast.Lit); ok {
		if lb, ok := b.(*ast.Lit); ok {
			ap, bp := la.Value.Kind == ast.LitParam, lb.Value.Kind == ast.LitParam
			if ap || bp {
				if !ap || !bp {
					return false
				}
				if la.Value.P != lb.Value.P {
					*conds = append(*conds, [2]uint32{la.Value.P, lb.Value.P})
				}
				return true
			}
		}
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.Type() != vb.Type() {
		return false
	}
	// Deref the node pointer here so cseEqVal's ast.Expr re-entry (which
	// calls back into this function) always makes progress.
	if va.Kind() == reflect.Ptr {
		if va.IsNil() || vb.IsNil() {
			return va.IsNil() == vb.IsNil()
		}
		va, vb = va.Elem(), vb.Elem()
	}
	return cseEqVal(va, vb, conds)
}

// cseEqVal is a structural walk mirroring reflect.DeepEqual over the
// expression tree, except that nested ast.Expr values re-enter
// equalModParams so parameter slots compare by condition at any depth.
// Kinds an AST node never holds (maps, funcs, channels) refuse to
// equate rather than guess.
func cseEqVal(a, b reflect.Value, conds *[][2]uint32) bool {
	if a.Type() != b.Type() {
		return false
	}
	switch a.Kind() {
	case reflect.Ptr:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		if ae, ok := a.Interface().(ast.Expr); ok {
			be, _ := b.Interface().(ast.Expr)
			return equalModParams(ae, be, conds)
		}
		return cseEqVal(a.Elem(), b.Elem(), conds)
	case reflect.Interface:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		if ae, ok := a.Interface().(ast.Expr); ok {
			be, ok2 := b.Interface().(ast.Expr)
			return ok2 && equalModParams(ae, be, conds)
		}
		return cseEqVal(a.Elem(), b.Elem(), conds)
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			if !cseEqVal(a.Field(i), b.Field(i), conds) {
				return false
			}
		}
		return true
	case reflect.Slice, reflect.Array:
		if a.Len() != b.Len() {
			return false
		}
		for i := 0; i < a.Len(); i++ {
			if !cseEqVal(a.Index(i), b.Index(i), conds) {
				return false
			}
		}
		return true
	case reflect.Map, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return false
	default:
		return a.Interface() == b.Interface()
	}
}
