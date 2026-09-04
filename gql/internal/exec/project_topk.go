// topKRows: the bounded ORDER BY + LIMIT accumulator behind the
// projection sink (and the aggregator's finalizeTopK) -- a max-heap over
// parallel key/payload/sequence arrays whose streaming selection equals
// materialize-sort-truncate exactly, plus the typed float64 shadow the
// sink's prefilter rejects against. Split from project.go.
package exec

import (
	"slices"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
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

// chunkKeyBatch is the bulk-key segment size for the chunked candidate
// path: one GetMany dispatch per key per batch.
const chunkKeyBatch = 1024

// pushCandidates consumes a final match level's candidate fill against
// the top-k gate: while the heap is hot (full, threshold valid) and
// every key is typed, candidate-slot keys bulk-read via GetMany and
// chunk-constant keys (other slots) read once, so a refused candidate
// costs a float compare instead of a bind + dispatch + boxed key
// evaluation. Survivors and every other regime route through the
// per-row push, whose own prefilter re-checks against the CURRENT
// threshold -- sequential semantics are exactly the per-row path's.
func (p *projSink) pushCandidates(row []value.Value, slot int, cands []graph.NodeID) bool {
	if !p.kGate || p.kTyped == nil || p.topk == nil {
		for _, c := range cands {
			row[slot] = value.Node(c)
			if !p.push(row) {
				return false
			}
		}
		return true
	}
	nk := len(p.kTyped)
	if cap(p.ckVals) < nk*chunkKeyBatch {
		p.ckVals = make([]int64, nk*chunkKeyBatch)
		p.ckPresent = make([]bool, nk*chunkKeyBatch)
	}
	constKey := make([]float64, nk)
	i := 0
	for i < len(cands) {
		if !(p.topk.n == p.topk.bound && p.topk.thrValid) {
			row[slot] = value.Node(cands[i])
			if !p.push(row) {
				return false
			}
			i++
			continue
		}
		// Chunk-constant keys (reading slots other than the bound one)
		// resolve once per batch; a failed read sends the whole batch
		// per-row, mirroring the per-row prefilter's typedOK bail.
		j := min(i+chunkKeyBatch, len(cands))
		seg := cands[i:j]
		constOK := true
		for k := range p.kTyped {
			if p.kTyped[k].slot != slot {
				v, ok := p.kTyped[k].fn(row)
				if !ok {
					constOK = false
					break
				}
				constKey[k] = v
			}
		}
		if !constOK {
			for _, c := range seg {
				row[slot] = value.Node(c)
				if !p.push(row) {
					return false
				}
			}
			i = j
			continue
		}
		for k := range p.kTyped {
			if p.kTyped[k].slot == slot {
				p.kTyped[k].col.GetMany(seg, p.ckVals[k*chunkKeyBatch:], p.ckPresent[k*chunkKeyBatch:])
			}
		}
		for ci, c := range seg {
			ok := true
			for k := range p.kTyped {
				if p.kTyped[k].slot == slot {
					if !p.ckPresent[k*chunkKeyBatch+ci] {
						ok = false
						break
					}
					p.kTypedBuf[k] = float64(p.ckVals[k*chunkKeyBatch+ci])
				} else {
					p.kTypedBuf[k] = constKey[k]
				}
			}
			// The heap may have cooled mid-batch (a survivor invalidated
			// the shadow); re-route the remainder through the hot-check.
			if !ok || !(p.topk.n == p.topk.bound && p.topk.thrValid) {
				row[slot] = value.Node(c)
				if !p.push(row) {
					return false
				}
				continue
			}
			if p.topk.typedReject(p.kTypedBuf) {
				typedSinkRejects++
				p.topk.seq++
				continue
			}
			row[slot] = value.Node(c)
			if !p.push(row) {
				return false
			}
		}
		i = j
	}
	return true
}
