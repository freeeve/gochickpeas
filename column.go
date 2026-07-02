// Columnar property storage: the engine-side column representations behind
// Snapshot property reads. Dense columns index directly by position (node id
// for node columns, outgoing-CSR position for rel columns); sparse columns
// binary-search (id, value) pairs sorted by id. The rank/select variants for
// the moderately-sparse band arrive with the builder (M4); the RCPG bridge
// never produces them.

package chickpeas

import (
	"iter"

	"github.com/freeeve/gochickpeas/internal/bitset"
)

// Dtype is the logical element type of a column, reported without narrowing.
type Dtype uint8

const (
	// DtypeI64 is an integer column.
	DtypeI64 Dtype = iota
	// DtypeF64 is a float column.
	DtypeF64
	// DtypeBool is a boolean column.
	DtypeBool
	// DtypeStr is an interned-string column.
	DtypeStr
)

// Column is one property column's storage. Get reads the value at a
// position; Entries iterates every (position, value) present, ascending.
// Concrete representations are internal -- read through Get/Entries or the
// typed Col readers.
type Column interface {
	// Get returns the value at pos; ok is false when absent.
	Get(pos uint32) (Value, bool)
	// Entries iterates (position, value) pairs in ascending position order.
	Entries() iter.Seq2[uint32, Value]
	// Dtype is the column's logical element type.
	Dtype() Dtype
	// Len is the number of positions carrying a value.
	Len() int
}

// --- dense ---------------------------------------------------------------------

type denseI64Col []int64

func (c denseI64Col) Get(pos uint32) (Value, bool) {
	if int(pos) >= len(c) {
		return Value{}, false
	}
	return I64Value(c[pos]), true
}

func (c denseI64Col) Entries() iter.Seq2[uint32, Value] {
	return func(yield func(uint32, Value) bool) {
		for i, v := range c {
			if !yield(uint32(i), I64Value(v)) {
				return
			}
		}
	}
}

func (c denseI64Col) Dtype() Dtype { return DtypeI64 }
func (c denseI64Col) Len() int     { return len(c) }

type denseF64Col []float64

func (c denseF64Col) Get(pos uint32) (Value, bool) {
	if int(pos) >= len(c) {
		return Value{}, false
	}
	return F64Value(c[pos]), true
}

func (c denseF64Col) Entries() iter.Seq2[uint32, Value] {
	return func(yield func(uint32, Value) bool) {
		for i, v := range c {
			if !yield(uint32(i), F64Value(v)) {
				return
			}
		}
	}
}

func (c denseF64Col) Dtype() Dtype { return DtypeF64 }
func (c denseF64Col) Len() int     { return len(c) }

type denseBoolCol struct{ bits *bitset.Bits }

func (c denseBoolCol) Get(pos uint32) (Value, bool) {
	if int(pos) >= c.bits.Len() {
		return Value{}, false
	}
	return BoolValue(c.bits.Get(int(pos))), true
}

func (c denseBoolCol) Entries() iter.Seq2[uint32, Value] {
	return func(yield func(uint32, Value) bool) {
		for i := range c.bits.Len() {
			if !yield(uint32(i), BoolValue(c.bits.Get(i))) {
				return
			}
		}
	}
}

func (c denseBoolCol) Dtype() Dtype { return DtypeBool }
func (c denseBoolCol) Len() int     { return c.bits.Len() }

type denseStrCol []uint32

func (c denseStrCol) Get(pos uint32) (Value, bool) {
	if int(pos) >= len(c) {
		return Value{}, false
	}
	return StrValue(c[pos]), true
}

func (c denseStrCol) Entries() iter.Seq2[uint32, Value] {
	return func(yield func(uint32, Value) bool) {
		for i, v := range c {
			if !yield(uint32(i), StrValue(v)) {
				return
			}
		}
	}
}

func (c denseStrCol) Dtype() Dtype { return DtypeStr }
func (c denseStrCol) Len() int     { return len(c) }

// --- sparse --------------------------------------------------------------------

// sparseSlot binary-searches sorted ids for pos, returning its slot.
func sparseSlot(ids []uint32, pos uint32) (int, bool) {
	lo, hi := 0, len(ids)
	for lo < hi {
		mid := (lo + hi) / 2
		if ids[mid] < pos {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(ids) && ids[lo] == pos {
		return lo, true
	}
	return 0, false
}

type sparseI64Col struct {
	ids  []uint32
	vals []int64
}

func (c sparseI64Col) Get(pos uint32) (Value, bool) {
	if slot, ok := sparseSlot(c.ids, pos); ok {
		return I64Value(c.vals[slot]), true
	}
	return Value{}, false
}

func (c sparseI64Col) Entries() iter.Seq2[uint32, Value] {
	return func(yield func(uint32, Value) bool) {
		for i, id := range c.ids {
			if !yield(id, I64Value(c.vals[i])) {
				return
			}
		}
	}
}

func (c sparseI64Col) Dtype() Dtype { return DtypeI64 }
func (c sparseI64Col) Len() int     { return len(c.ids) }

type sparseF64Col struct {
	ids  []uint32
	vals []float64
}

func (c sparseF64Col) Get(pos uint32) (Value, bool) {
	if slot, ok := sparseSlot(c.ids, pos); ok {
		return F64Value(c.vals[slot]), true
	}
	return Value{}, false
}

func (c sparseF64Col) Entries() iter.Seq2[uint32, Value] {
	return func(yield func(uint32, Value) bool) {
		for i, id := range c.ids {
			if !yield(id, F64Value(c.vals[i])) {
				return
			}
		}
	}
}

func (c sparseF64Col) Dtype() Dtype { return DtypeF64 }
func (c sparseF64Col) Len() int     { return len(c.ids) }

type sparseBoolCol struct {
	ids  []uint32
	vals []bool
}

func (c sparseBoolCol) Get(pos uint32) (Value, bool) {
	if slot, ok := sparseSlot(c.ids, pos); ok {
		return BoolValue(c.vals[slot]), true
	}
	return Value{}, false
}

func (c sparseBoolCol) Entries() iter.Seq2[uint32, Value] {
	return func(yield func(uint32, Value) bool) {
		for i, id := range c.ids {
			if !yield(id, BoolValue(c.vals[i])) {
				return
			}
		}
	}
}

func (c sparseBoolCol) Dtype() Dtype { return DtypeBool }
func (c sparseBoolCol) Len() int     { return len(c.ids) }

type sparseStrCol struct {
	ids  []uint32
	vals []uint32
}

func (c sparseStrCol) Get(pos uint32) (Value, bool) {
	if slot, ok := sparseSlot(c.ids, pos); ok {
		return StrValue(c.vals[slot]), true
	}
	return Value{}, false
}

func (c sparseStrCol) Entries() iter.Seq2[uint32, Value] {
	return func(yield func(uint32, Value) bool) {
		for i, id := range c.ids {
			if !yield(id, StrValue(c.vals[i])) {
				return
			}
		}
	}
}

func (c sparseStrCol) Dtype() Dtype { return DtypeStr }
func (c sparseStrCol) Len() int     { return len(c.ids) }

// isSparse reports the binary-searched pair representations -- the columns a
// position index makes O(1). Dense columns already read in O(1).
func isSparse(c Column) bool {
	switch c.(type) {
	case sparseI64Col, sparseF64Col, sparseBoolCol, sparseStrCol:
		return true
	}
	return false
}

// valueAtSlot reads a sparse column's value by slot (array index into its
// pairs) -- the O(1) companion to the binary search, used once a position
// index has resolved a position.
func valueAtSlot(c Column, slot uint32) (Value, bool) {
	i := int(slot)
	switch col := c.(type) {
	case sparseI64Col:
		if i < len(col.vals) {
			return I64Value(col.vals[i]), true
		}
	case sparseF64Col:
		if i < len(col.vals) {
			return F64Value(col.vals[i]), true
		}
	case sparseBoolCol:
		if i < len(col.vals) {
			return BoolValue(col.vals[i]), true
		}
	case sparseStrCol:
		if i < len(col.vals) {
			return StrValue(col.vals[i]), true
		}
	}
	return Value{}, false
}
