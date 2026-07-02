// Typed column readers: the key -> column lookup is done once via
// Snapshot.Col / Snapshot.RelCol, then narrowed to a typed reader -- the
// polars column(name).i64() shape. Dense columns read through their raw
// slice fast path; sparse columns binary-search, or read O(1) through a
// lazily built position index (Snapshot.ColIndexed / RelColIndexed).

package chickpeas

import "github.com/freeeve/gochickpeas/internal/bitset"

// posIndex maps a position to the slot of its (pos, value) pair in a sparse
// column, replacing the binary search with an O(1) map read.
type posIndex map[uint32]uint32

func buildPosIndex(c Column) posIndex {
	m := make(posIndex, c.Len())
	slot := uint32(0)
	for pos := range c.Entries() {
		m[pos] = slot
		slot++
	}
	return m
}

// readIndexed reads a position through the resolved index when present,
// else the column's own lookup.
func readIndexed(c Column, idx posIndex, pos uint32) (Value, bool) {
	if idx == nil {
		return c.Get(pos)
	}
	slot, ok := idx[pos]
	if !ok {
		return Value{}, false
	}
	return valueAtSlot(c, slot)
}

// Col is a resolved property column, narrowed to a typed reader with I64 /
// F64 / Bool / Str. Build with Snapshot.Col (node columns, indexed by node
// id) or Snapshot.RelCol (rel columns, indexed by outgoing-CSR position).
type Col struct {
	col Column
	idx posIndex
}

// Dtype is the column's logical element type, for picking a typed reader
// without narrowing.
func (c Col) Dtype() Dtype { return c.col.Dtype() }

// Column is the raw generic reader.
func (c Col) Column() Column { return c.col }

// I64 narrows to a typed integer reader; a non-integer column reads back as
// absent, like a mistyped Prop.
func (c Col) I64() I64Col {
	dense, _ := c.col.(denseI64Col)
	return I64Col{dense: dense, col: c.col, idx: c.idx}
}

// F64 narrows to a typed float reader.
func (c Col) F64() F64Col {
	dense, _ := c.col.(denseF64Col)
	return F64Col{dense: dense, col: c.col, idx: c.idx}
}

// Bool narrows to a typed boolean reader.
func (c Col) Bool() BoolCol {
	var dense *bitset.Bits
	if d, ok := c.col.(denseBoolCol); ok {
		dense = d.bits
	}
	return BoolCol{dense: dense, col: c.col, idx: c.idx}
}

// Str narrows to a typed string reader exposing interned atom ids; resolve
// a comparison string to its atom once and compare ids.
func (c Col) Str() StrCol {
	dense, _ := c.col.(denseStrCol)
	return StrCol{dense: dense, col: c.col, idx: c.idx}
}

// I64Col is a resolved integer column reader.
type I64Col struct {
	dense denseI64Col
	col   Column
	idx   posIndex
}

// Get returns the value at pos (a node id for node columns, a CSR position
// for rel columns); ok is false when absent.
func (c I64Col) Get(pos uint32) (int64, bool) {
	if c.dense != nil {
		if int(pos) < len(c.dense) {
			return c.dense[pos], true
		}
		return 0, false
	}
	v, ok := readIndexed(c.col, c.idx, pos)
	if !ok {
		return 0, false
	}
	return v.I64()
}

// Slice is the dense value slice indexed directly by position; ok is false
// for a sparse column (fall back to Get).
func (c I64Col) Slice() ([]int64, bool) {
	return c.dense, c.dense != nil
}

// F64Col is a resolved float column reader.
type F64Col struct {
	dense denseF64Col
	col   Column
	idx   posIndex
}

// Get returns the value at pos; ok is false when absent.
func (c F64Col) Get(pos uint32) (float64, bool) {
	if c.dense != nil {
		if int(pos) < len(c.dense) {
			return c.dense[pos], true
		}
		return 0, false
	}
	v, ok := readIndexed(c.col, c.idx, pos)
	if !ok {
		return 0, false
	}
	return v.F64()
}

// Slice is the dense value slice; ok is false for a sparse column.
func (c F64Col) Slice() ([]float64, bool) {
	return c.dense, c.dense != nil
}

// BoolCol is a resolved boolean column reader.
type BoolCol struct {
	dense *bitset.Bits
	col   Column
	idx   posIndex
}

// Get returns the value at pos; ok is false when absent.
func (c BoolCol) Get(pos uint32) (bool, bool) {
	if c.dense != nil {
		if int(pos) < c.dense.Len() {
			return c.dense.Get(int(pos)), true
		}
		return false, false
	}
	v, ok := readIndexed(c.col, c.idx, pos)
	if !ok {
		return false, false
	}
	return v.Bool()
}

// Bits is the dense bit vector indexed directly by position; ok is false
// for a sparse column.
func (c BoolCol) Bits() (*bitset.Bits, bool) {
	return c.dense, c.dense != nil
}

// StrCol is a resolved string column reader exposing interned atom ids.
type StrCol struct {
	dense denseStrCol
	col   Column
	idx   posIndex
}

// ID returns the atom id at pos; ok is false when absent. Note atom 0 ("")
// in a dense column means missing -- Prop.Str folds that in; this raw
// reader does not.
func (c StrCol) ID(pos uint32) (uint32, bool) {
	if c.dense != nil {
		if int(pos) < len(c.dense) {
			return c.dense[pos], true
		}
		return 0, false
	}
	v, ok := readIndexed(c.col, c.idx, pos)
	if !ok {
		return 0, false
	}
	return v.StrID()
}

// IDs is the dense atom-id slice; ok is false for a sparse column.
func (c StrCol) IDs() ([]uint32, bool) {
	return c.dense, c.dense != nil
}
