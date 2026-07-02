// Columnar property storage: the 8 on-disk column tags (dense/sparse x
// i64/f64/bool/str) and their codecs. Dense bool stays raw LSB-first packed
// bytes, exactly as on disk, keeping the codec dependency-free; the engine
// converts to its own bitset when it materializes a snapshot.

package rcpg

import (
	"bytes"
	"math"
)

const (
	colDenseI64 byte = iota
	colDenseF64
	colDenseBool
	colDenseStr
	colSparseI64
	colSparseF64
	colSparseBool
	colSparseStr
)

// ColumnData is one property column's storage. Dense columns are indexed by
// ID (node ID, or outgoing-CSR position for rel columns); sparse columns are
// (id, value) pairs sorted by ID. String values are atom IDs into
// GraphSection.Atoms; atom 0 ("") in a dense str column means missing.
type ColumnData interface {
	tag() byte
}

// DenseI64 is a dense int64 column.
type DenseI64 []int64

// DenseF64 is a dense float64 column; values are written as raw IEEE-754
// bits, so NaN payloads and -0.0 survive round trips exactly.
type DenseF64 []float64

// DenseBool is a dense bool column as stored: Len bits packed LSB-first
// within each byte, ceil(Len/8) bytes.
type DenseBool struct {
	Bits []byte
	Len  uint32
}

// Get reports bit i, false when out of range.
func (d DenseBool) Get(i uint32) bool {
	if i >= d.Len {
		return false
	}
	return d.Bits[i/8]&(1<<(i%8)) != 0
}

// Set sets bit i to v; i must be < Len.
func (d DenseBool) Set(i uint32, v bool) {
	if v {
		d.Bits[i/8] |= 1 << (i % 8)
	} else {
		d.Bits[i/8] &^= 1 << (i % 8)
	}
}

// NewDenseBool returns an all-false column of n bits.
func NewDenseBool(n uint32) DenseBool {
	return DenseBool{Bits: make([]byte, (n+7)/8), Len: n}
}

// DenseStr is a dense string column of value atoms.
type DenseStr []uint32

// I64Entry is one sparse int64 entry.
type I64Entry struct {
	ID  uint32
	Val int64
}

// F64Entry is one sparse float64 entry.
type F64Entry struct {
	ID  uint32
	Val float64
}

// BoolEntry is one sparse bool entry.
type BoolEntry struct {
	ID  uint32
	Val bool
}

// StrEntry is one sparse string entry (value is an atom).
type StrEntry struct {
	ID  uint32
	Val uint32
}

// SparseI64 is a sparse int64 column, sorted by ID.
type SparseI64 []I64Entry

// SparseF64 is a sparse float64 column, sorted by ID.
type SparseF64 []F64Entry

// SparseBool is a sparse bool column, sorted by ID.
type SparseBool []BoolEntry

// SparseStr is a sparse string column, sorted by ID.
type SparseStr []StrEntry

func (DenseI64) tag() byte   { return colDenseI64 }
func (DenseF64) tag() byte   { return colDenseF64 }
func (DenseBool) tag() byte  { return colDenseBool }
func (DenseStr) tag() byte   { return colDenseStr }
func (SparseI64) tag() byte  { return colSparseI64 }
func (SparseF64) tag() byte  { return colSparseF64 }
func (SparseBool) tag() byte { return colSparseBool }
func (SparseStr) tag() byte  { return colSparseStr }

func encodeColumns(columns []Column) ([]byte, error) {
	var buf bytes.Buffer
	if err := wLenPrefix(&buf, len(columns)); err != nil {
		return nil, err
	}
	for _, col := range columns {
		wU32(&buf, col.Key)
		wU8(&buf, col.Data.tag())
		var err error
		switch d := col.Data.(type) {
		case DenseI64:
			if err = wLenPrefix(&buf, len(d)); err == nil {
				for _, x := range d {
					wI64(&buf, x)
				}
			}
		case DenseF64:
			if err = wLenPrefix(&buf, len(d)); err == nil {
				for _, x := range d {
					wF64(&buf, x)
				}
			}
		case DenseBool:
			wU32(&buf, d.Len)
			buf.Write(d.Bits[:(d.Len+7)/8])
		case DenseStr:
			err = wU32Vec(&buf, d)
		case SparseI64:
			if err = wLenPrefix(&buf, len(d)); err == nil {
				for _, e := range d {
					wU32(&buf, e.ID)
					wI64(&buf, e.Val)
				}
			}
		case SparseF64:
			if err = wLenPrefix(&buf, len(d)); err == nil {
				for _, e := range d {
					wU32(&buf, e.ID)
					wF64(&buf, e.Val)
				}
			}
		case SparseBool:
			if err = wLenPrefix(&buf, len(d)); err == nil {
				for _, e := range d {
					wU32(&buf, e.ID)
					if e.Val {
						wU8(&buf, 1)
					} else {
						wU8(&buf, 0)
					}
				}
			}
		case SparseStr:
			if err = wLenPrefix(&buf, len(d)); err == nil {
				for _, e := range d {
					wU32(&buf, e.ID)
					wU32(&buf, e.Val)
				}
			}
		default:
			err = corruptf("unknown column data type %T", col.Data)
		}
		if err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func decodeColumns(body []byte) ([]Column, error) {
	c := newCursor(body)
	count, err := c.lenPrefix(5)
	if err != nil {
		return nil, err
	}
	columns := make([]Column, 0, count)
	for range count {
		key, err := c.u32()
		if err != nil {
			return nil, err
		}
		tag, err := c.u8()
		if err != nil {
			return nil, err
		}
		data, err := decodeColumnData(c, tag)
		if err != nil {
			return nil, err
		}
		columns = append(columns, Column{Key: key, Data: data})
	}
	return columns, nil
}

func decodeColumnData(c *cursor, tag byte) (ColumnData, error) {
	switch tag {
	case colDenseI64:
		v, err := c.i64Vec()
		return DenseI64(v), err
	case colDenseF64:
		v, err := c.f64Vec()
		return DenseF64(v), err
	case colDenseBool:
		bitLen, err := c.u32()
		if err != nil {
			return nil, err
		}
		packed, err := c.take(int((uint64(bitLen) + 7) / 8))
		if err != nil {
			return nil, err
		}
		return DenseBool{Bits: bytes.Clone(packed), Len: bitLen}, nil
	case colDenseStr:
		v, err := c.u32Vec()
		return DenseStr(v), err
	case colSparseI64:
		n, err := c.lenPrefix(12)
		if err != nil {
			return nil, err
		}
		v := make(SparseI64, 0, n)
		for range n {
			id, err := c.u32()
			if err != nil {
				return nil, err
			}
			x, err := c.i64()
			if err != nil {
				return nil, err
			}
			v = append(v, I64Entry{ID: id, Val: x})
		}
		return v, nil
	case colSparseF64:
		n, err := c.lenPrefix(12)
		if err != nil {
			return nil, err
		}
		v := make(SparseF64, 0, n)
		for range n {
			id, err := c.u32()
			if err != nil {
				return nil, err
			}
			bits, err := c.u64()
			if err != nil {
				return nil, err
			}
			v = append(v, F64Entry{ID: id, Val: math.Float64frombits(bits)})
		}
		return v, nil
	case colSparseBool:
		n, err := c.lenPrefix(5)
		if err != nil {
			return nil, err
		}
		v := make(SparseBool, 0, n)
		for range n {
			id, err := c.u32()
			if err != nil {
				return nil, err
			}
			b, err := c.u8()
			if err != nil {
				return nil, err
			}
			v = append(v, BoolEntry{ID: id, Val: b != 0})
		}
		return v, nil
	case colSparseStr:
		n, err := c.lenPrefix(8)
		if err != nil {
			return nil, err
		}
		v := make(SparseStr, 0, n)
		for range n {
			id, err := c.u32()
			if err != nil {
				return nil, err
			}
			x, err := c.u32()
			if err != nil {
				return nil, err
			}
			v = append(v, StrEntry{ID: id, Val: x})
		}
		return v, nil
	default:
		return nil, corruptf("unknown column tag %d", tag)
	}
}
