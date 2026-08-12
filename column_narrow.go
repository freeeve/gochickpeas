// Narrow integer-column storage (task 213): values that fit a small byte
// class after a minimum offset store little-endian deltas at width 1, 2,
// 4, or 6 instead of 8 bytes each. Narrowing is a storage choice made at
// column build -- the logical Dtype stays I64 and values read back
// identically through every class. The 6-byte class exists for
// epoch-millis timestamp columns, whose offset spans sit at 37-45 bits --
// past u32, far under u64. Dense, sparse, and rank layouts share one
// value-vector representation.
package chickpeas

import (
	"encoding/binary"
	"iter"

	"github.com/freeeve/gochickpeas/internal/bitset"
)

// narrowI64Vec is the shared narrow value store: value i = min + delta_i.
type narrowI64Vec struct {
	min int64
	w   uint8
	b   []byte
}

// n is the element count.
func (v narrowI64Vec) n() int { return len(v.b) / int(v.w) }

// at is the decoded value at slot i (caller bounds-checks). The u48
// class reads one unaligned 8-byte load masked to 48 bits -- the buffer
// carries a 2-byte pad so the load never runs off the end; two narrow
// loads plus a shift-combine measurably taxed per-candidate reads in
// microsecond-scale kernels (task 300).
func (v narrowI64Vec) at(i int) int64 {
	switch v.w {
	case 1:
		return v.min + int64(v.b[i])
	case 2:
		return v.min + int64(binary.LittleEndian.Uint16(v.b[i*2:]))
	case 4:
		return v.min + int64(binary.LittleEndian.Uint32(v.b[i*4:]))
	}
	return v.min + int64(binary.LittleEndian.Uint64(v.b[i*6:])&narrowU48Mask)
}

// narrowU48Mask keeps the low 48 bits of the padded 8-byte u48 load.
const narrowU48Mask = 0xFFFFFFFFFFFF

// narrowI64MinLen gates narrowing to columns big enough for the byte
// savings to matter; below it the extra representation buys nothing.
const narrowI64MinLen = 1024

// narrowI64Vals encodes vals into the narrowest byte class whose offset
// span fits; ok is false when the column is too small to bother or the
// span needs a full 8 bytes (ids and similar full-range values).
func narrowI64Vals(vals []int64) (narrowI64Vec, bool) {
	if len(vals) < narrowI64MinLen {
		return narrowI64Vec{}, false
	}
	mn, mx := vals[0], vals[0]
	for _, v := range vals {
		if v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
	}
	span := uint64(mx - mn)
	var w uint8
	switch {
	case span <= 0xFF:
		w = 1
	case span <= 0xFFFF:
		w = 2
	case span <= 0xFFFFFFFF:
		w = 4
	case span <= 0xFFFFFFFFFFFF:
		w = 6
	default:
		return narrowI64Vec{}, false
	}
	// The u48 buffer carries a 2-byte pad so at() can decode with one
	// 8-byte load; n() = len/w is unaffected (2 < w) and the pad is
	// never addressed as an element.
	pad := 0
	if w == 6 {
		pad = 2
	}
	b := make([]byte, len(vals)*int(w)+pad)
	for i, v := range vals {
		d := uint64(v - mn)
		switch w {
		case 1:
			b[i] = byte(d)
		case 2:
			binary.LittleEndian.PutUint16(b[i*2:], uint16(d))
		case 4:
			binary.LittleEndian.PutUint32(b[i*4:], uint32(d))
		default:
			binary.LittleEndian.PutUint32(b[i*6:], uint32(d))
			binary.LittleEndian.PutUint16(b[i*6+4:], uint16(d>>32))
		}
	}
	return narrowI64Vec{min: mn, w: w, b: b}, true
}

// denseI64NarrowCol is the dense narrow layout: slot == position.
type denseI64NarrowCol struct {
	narrowI64Vec
}

// at keeps the historical position-typed accessor.
func (c denseI64NarrowCol) at(pos uint32) int64 { return c.narrowI64Vec.at(int(pos)) }

func (c denseI64NarrowCol) Get(pos uint32) (Value, bool) {
	if int(pos) >= c.n() {
		return Value{}, false
	}
	return I64Value(c.at(pos)), true
}

func (c denseI64NarrowCol) Entries() iter.Seq2[uint32, Value] {
	return func(yield func(uint32, Value) bool) {
		for i := range c.n() {
			if !yield(uint32(i), I64Value(c.narrowI64Vec.at(i))) {
				return
			}
		}
	}
}

func (c denseI64NarrowCol) Dtype() Dtype { return DtypeI64 }
func (c denseI64NarrowCol) Len() int     { return c.n() }

// denseI64BitCol is the two-valued dense class: a column whose values are
// all min or min+1 (0/1 flags stored as integers being the archetype)
// packs to one bit per position. Dtype stays I64 like every narrow class.
type denseI64BitCol struct {
	min  int64
	bits *bitset.Bits
}

func (c denseI64BitCol) at(pos uint32) int64 {
	if c.bits.Get(int(pos)) {
		return c.min + 1
	}
	return c.min
}

func (c denseI64BitCol) Get(pos uint32) (Value, bool) {
	if int(pos) >= c.bits.Len() {
		return Value{}, false
	}
	return I64Value(c.at(pos)), true
}

func (c denseI64BitCol) Entries() iter.Seq2[uint32, Value] {
	return func(yield func(uint32, Value) bool) {
		for i := range c.bits.Len() {
			if !yield(uint32(i), I64Value(c.at(uint32(i)))) {
				return
			}
		}
	}
}

func (c denseI64BitCol) Dtype() Dtype { return DtypeI64 }
func (c denseI64BitCol) Len() int     { return c.bits.Len() }

// narrowI64Column picks the dense storage class: one bit per position for
// a two-valued column, else the narrowest byte class the span allows,
// else plain []int64.
func narrowI64Column(vals []int64) Column {
	if len(vals) >= narrowI64MinLen {
		mn, mx := vals[0], vals[0]
		for _, v := range vals {
			if v < mn {
				mn = v
			}
			if v > mx {
				mx = v
			}
		}
		if uint64(mx-mn) <= 1 {
			bits := bitset.New(len(vals))
			for i, v := range vals {
				bits.Set(i, v != mn)
			}
			return denseI64BitCol{min: mn, bits: bits}
		}
	}
	if nv, ok := narrowI64Vals(vals); ok {
		return denseI64NarrowCol{nv}
	}
	return denseI64Col(vals)
}

// sparseI64NarrowCol is the sparse narrow layout: position-sorted ids with
// narrow values, binary-searched exactly like sparseI64Col.
type sparseI64NarrowCol struct {
	ids []uint32
	narrowI64Vec
}

func (c sparseI64NarrowCol) Get(pos uint32) (Value, bool) {
	if slot, ok := sparseSlot(c.ids, pos); ok {
		return I64Value(c.at(slot)), true
	}
	return Value{}, false
}

func (c sparseI64NarrowCol) Entries() iter.Seq2[uint32, Value] {
	return func(yield func(uint32, Value) bool) {
		for i, id := range c.ids {
			if !yield(id, I64Value(c.at(i))) {
				return
			}
		}
	}
}

func (c sparseI64NarrowCol) Dtype() Dtype { return DtypeI64 }
func (c sparseI64NarrowCol) Len() int     { return len(c.ids) }

// rankI64NarrowCol is the rank-select narrow layout.
type rankI64NarrowCol struct {
	rankIndex
	narrowI64Vec
}

func (c rankI64NarrowCol) Get(pos uint32) (Value, bool) {
	if slot, ok := c.slot(pos); ok {
		return I64Value(c.at(slot)), true
	}
	return Value{}, false
}

func (c rankI64NarrowCol) Entries() iter.Seq2[uint32, Value] {
	return c.entries(func(slot int) Value { return I64Value(c.at(slot)) })
}

func (c rankI64NarrowCol) Dtype() Dtype { return DtypeI64 }
func (c rankI64NarrowCol) Len() int     { return c.n() }
