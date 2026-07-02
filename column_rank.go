// Rank/select columns for the moderately-sparse band: O(1) reads without
// the binary search of a sparse column. present marks which positions carry
// a value; blockRank[b] caches the number of set bits before block b
// (rankBlock positions per block); values hold the values in ascending
// position order. Built by Finalize; serialized as sparse columns.

package chickpeas

import (
	"iter"

	"github.com/freeeve/gochickpeas/internal/bitset"
)

// rankBlock is the positions-per-block granularity: a Get does one indexed
// blockRank read plus a popcount over at most this many bits.
const rankBlock = 512

type rankIndex struct {
	present   *bitset.Bits
	blockRank []uint32
}

// slot returns the index into values for pos, or not-ok when pos carries no
// value. O(1): one blockRank read plus a bounded popcount.
func (r rankIndex) slot(pos uint32) (int, bool) {
	p := int(pos)
	if p >= r.present.Len() || !r.present.Get(p) {
		return 0, false
	}
	b := p / rankBlock
	return int(r.blockRank[b]) + r.present.CountRange(b*rankBlock, p), true
}

func (r rankIndex) entries(value func(slot int) Value) iter.Seq2[uint32, Value] {
	return func(yield func(uint32, Value) bool) {
		slot := 0
		for pos := range r.present.Ones() {
			if !yield(uint32(pos), value(slot)) {
				return
			}
			slot++
		}
	}
}

// buildRankIndex builds the presence bitmap + block-rank index from
// position-sorted pairs over a span-position column space.
func buildRankIndex(positions []uint32, span int) rankIndex {
	present := bitset.New(span)
	for _, pos := range positions {
		present.Set(int(pos), true)
	}
	nblocks := (span + rankBlock - 1) / rankBlock
	blockRank := make([]uint32, 0, nblocks+1)
	blockRank = append(blockRank, 0)
	acc := uint32(0)
	for b := range nblocks {
		acc += uint32(present.CountRange(b*rankBlock, min((b+1)*rankBlock, span)))
		blockRank = append(blockRank, acc)
	}
	return rankIndex{present: present, blockRank: blockRank}
}

type rankI64Col struct {
	rankIndex
	vals []int64
}

func (c rankI64Col) Get(pos uint32) (Value, bool) {
	if slot, ok := c.slot(pos); ok {
		return I64Value(c.vals[slot]), true
	}
	return Value{}, false
}

func (c rankI64Col) Entries() iter.Seq2[uint32, Value] {
	return c.entries(func(slot int) Value { return I64Value(c.vals[slot]) })
}

func (c rankI64Col) Dtype() Dtype { return DtypeI64 }
func (c rankI64Col) Len() int     { return len(c.vals) }

type rankF64Col struct {
	rankIndex
	vals []float64
}

func (c rankF64Col) Get(pos uint32) (Value, bool) {
	if slot, ok := c.slot(pos); ok {
		return F64Value(c.vals[slot]), true
	}
	return Value{}, false
}

func (c rankF64Col) Entries() iter.Seq2[uint32, Value] {
	return c.entries(func(slot int) Value { return F64Value(c.vals[slot]) })
}

func (c rankF64Col) Dtype() Dtype { return DtypeF64 }
func (c rankF64Col) Len() int     { return len(c.vals) }

type rankBoolCol struct {
	rankIndex
	vals *bitset.Bits
}

func (c rankBoolCol) Get(pos uint32) (Value, bool) {
	if slot, ok := c.slot(pos); ok {
		return BoolValue(c.vals.Get(slot)), true
	}
	return Value{}, false
}

func (c rankBoolCol) Entries() iter.Seq2[uint32, Value] {
	return c.entries(func(slot int) Value { return BoolValue(c.vals.Get(slot)) })
}

func (c rankBoolCol) Dtype() Dtype { return DtypeBool }
func (c rankBoolCol) Len() int     { return c.vals.Len() }

type rankStrCol struct {
	rankIndex
	vals []uint32
}

func (c rankStrCol) Get(pos uint32) (Value, bool) {
	if slot, ok := c.slot(pos); ok {
		return StrValue(c.vals[slot]), true
	}
	return Value{}, false
}

func (c rankStrCol) Entries() iter.Seq2[uint32, Value] {
	return c.entries(func(slot int) Value { return StrValue(c.vals[slot]) })
}

func (c rankStrCol) Dtype() Dtype { return DtypeStr }
func (c rankStrCol) Len() int     { return len(c.vals) }
