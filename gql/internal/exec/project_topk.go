// topKRows: the bounded ORDER BY + LIMIT accumulator behind the
// projection sink (and the aggregator's finalizeTopK) -- a max-heap over
// parallel key/payload/sequence arrays whose streaming selection equals
// materialize-sort-truncate exactly, plus the typed float64 shadow the
// sink's prefilter rejects against. Split from project.go.
package exec

import (
	"slices"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/value"
)

// topKRows is the sink's bounded ORDER BY + LIMIT accumulator: at most
// bound rows survive, under the same total order finalize's sort applies
// (ORDER BY keys, then arrival sequence), so streaming selection equals
// materialize-sort-truncate exactly. A max-heap over parallel arrays;
// the worst survivor sits at the root and each rejected offer costs one
// comparison.
type topKRows struct {
	bound int
	nk    int
	desc  []bool
	keys  []value.Value // n*nk, admitted key vectors
	outs  [][]value.Value
	seqs  []int
	n     int
	seq   int
	// Typed shadow of the root's key tuple for the sink's prefilter:
	// valid only while every root key is a KindInt (the i64-column
	// boxed form the typed readers mirror); refreshed after each
	// successful offer.
	typedArmed bool
	thr        []float64
	thrValid   bool
}

// refreshTyped re-derives the typed shadow from the current root.
func (t *topKRows) refreshTyped() {
	if !t.typedArmed || t.n < t.bound {
		t.thrValid = false
		return
	}
	for k := 0; k < t.nk; k++ {
		v, ok := t.keys[k].AsInt()
		if !ok || t.keys[k].Kind() != value.KindInt {
			t.thrValid = false
			return
		}
		t.thr[k] = float64(v)
	}
	t.thrValid = true
}

// typedReject reports whether a candidate with these typed keys would be
// refused by wouldAccept: accepted only when strictly before the root
// under the sort order; a tie loses (a fresh arrival's sequence is
// larger than every admitted row's).
func (t *topKRows) typedReject(keys []float64) bool {
	for k := 0; k < t.nk; k++ {
		if keys[k] != t.thr[k] {
			less := keys[k] < t.thr[k]
			if t.desc[k] {
				less = !less
			}
			return !less
		}
	}
	return true
}

func newTopKRows(bound, nk int, order []ast.SortItem) *topKRows {
	t := &topKRows{bound: bound, nk: nk, desc: make([]bool, nk)}
	for i := range order {
		t.desc[i] = order[i].Desc
	}
	// The heap fills to bound and stays there; presizing (capped, so a
	// huge LIMIT over few offers does not overcommit) removes the append
	// regrowth of the three parallel arrays.
	if pre := min(bound, 1024); pre > 0 {
		t.keys = make([]value.Value, 0, pre*nk)
		t.outs = make([][]value.Value, 0, pre)
		t.seqs = make([]int, 0, pre)
	}
	return t
}

// cmpTo compares candidate (keys, seq) against admitted entry i.
func (t *topKRows) cmpTo(keys []value.Value, seq, i int) int {
	for k := 0; k < t.nk; k++ {
		ord := value.OrderCmp(keys[k], t.keys[i*t.nk+k])
		if t.desc[k] {
			ord = -ord
		}
		if ord != 0 {
			return ord
		}
	}
	return seq - t.seqs[i]
}

// cmpEntries compares admitted entries a and b.
func (t *topKRows) cmpEntries(a, b int) int {
	return t.cmpTo(t.keys[a*t.nk:(a+1)*t.nk], t.seqs[a], b)
}

// offer admits the row when it belongs in the current top bound, copying
// its keys (the caller's buffer is reused). Reports whether the row was
// retained.
// wouldAccept reports whether an offer carrying keys could be admitted,
// consulted BEFORE the payload is built: true under capacity; once full,
// only when keys sort strictly before the worst survivor. A key TIE
// loses -- a new candidate's arrival sequence is larger than every
// admitted row's, so a tied offer would be popped immediately -- which
// makes skipping a rejected candidate byte-identical to offering it, and
// preserves stability across a tie straddling the LIMIT boundary (the
// kept rows' relative order never changes).
func (t *topKRows) wouldAccept(keys []value.Value) bool {
	if t.bound == 0 {
		return false
	}
	if t.n < t.bound {
		return true
	}
	for k := 0; k < t.nk; k++ {
		ord := value.OrderCmp(keys[k], t.keys[k])
		if t.desc[k] {
			ord = -ord
		}
		if ord != 0 {
			return ord < 0
		}
	}
	return false
}

func (t *topKRows) offer(keys []value.Value, out []value.Value) bool {
	seq := t.seq
	t.seq++
	if t.bound == 0 {
		return false
	}
	if t.n == t.bound && t.cmpTo(keys, seq, 0) >= 0 {
		return false
	}
	if t.n < t.bound {
		t.keys = append(t.keys, keys...)
		t.outs = append(t.outs, out)
		t.seqs = append(t.seqs, seq)
		t.n++
		t.siftUp(t.n - 1)
		t.refreshTyped()
		return true
	}
	copy(t.keys[:t.nk], keys)
	t.outs[0], t.seqs[0] = out, seq
	t.siftDown(0)
	t.refreshTyped()
	return true
}

func (t *topKRows) swap(a, b int) {
	for k := 0; k < t.nk; k++ {
		t.keys[a*t.nk+k], t.keys[b*t.nk+k] = t.keys[b*t.nk+k], t.keys[a*t.nk+k]
	}
	t.outs[a], t.outs[b] = t.outs[b], t.outs[a]
	t.seqs[a], t.seqs[b] = t.seqs[b], t.seqs[a]
}

func (t *topKRows) siftUp(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if t.cmpEntries(i, parent) <= 0 {
			return
		}
		t.swap(i, parent)
		i = parent
	}
}

func (t *topKRows) siftDown(i int) {
	for {
		l := 2*i + 1
		if l >= t.n {
			return
		}
		big := l
		if r := l + 1; r < t.n && t.cmpEntries(r, l) > 0 {
			big = r
		}
		if t.cmpEntries(big, i) <= 0 {
			return
		}
		t.swap(i, big)
		i = big
	}
}

// sorted returns the survivors in final order (ascending under the total
// order).
func (t *topKRows) sorted() [][]value.Value {
	idx := make([]int, t.n)
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, t.cmpEntries)
	outs := make([][]value.Value, t.n)
	for i, j := range idx {
		outs[i] = t.outs[j]
	}
	return outs
}
