// Bounds-checked little-endian reader over a byte slice, plus the matching
// writer helpers. Port of the Rust codec's cursor.rs; the read side rejects
// any length that exceeds the remaining input before allocating.

package rcpg

import (
	"bytes"
	"encoding/binary"
	"math"
	"unicode/utf8"
)

type cursor struct {
	b   []byte
	pos int
}

func newCursor(b []byte) *cursor {
	return &cursor{b: b}
}

func (c *cursor) remaining() int {
	return len(c.b) - c.pos
}

func (c *cursor) take(n int) ([]byte, error) {
	if n < 0 || c.remaining() < n {
		return nil, corruptf("unexpected end of input: need %d bytes at offset %d, have %d",
			n, c.pos, c.remaining())
	}
	s := c.b[c.pos : c.pos+n]
	c.pos += n
	return s, nil
}

func (c *cursor) u8() (uint8, error) {
	s, err := c.take(1)
	if err != nil {
		return 0, err
	}
	return s[0], nil
}

func (c *cursor) u16() (uint16, error) {
	s, err := c.take(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(s), nil
}

func (c *cursor) u32() (uint32, error) {
	s, err := c.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(s), nil
}

func (c *cursor) u64() (uint64, error) {
	s, err := c.take(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(s), nil
}

func (c *cursor) i64() (int64, error) {
	v, err := c.u64()
	return int64(v), err
}

func (c *cursor) f64() (float64, error) {
	v, err := c.u64()
	return math.Float64frombits(v), err
}

// lenPrefix reads a u32 length that will be used to allocate elemSize-byte
// elements, rejecting lengths that exceed the remaining input (prevents huge
// allocations from corrupt length fields).
func (c *cursor) lenPrefix(elemSize int) (int, error) {
	v, err := c.u32()
	if err != nil {
		return 0, err
	}
	n := int(v)
	need := n * elemSize
	if need/elemSize != n || need > c.remaining() {
		return 0, corruptf("declared length %d (%d bytes) exceeds remaining input %d at offset %d",
			n, need, c.remaining(), c.pos)
	}
	return n, nil
}

func (c *cursor) u32Vec() ([]uint32, error) {
	n, err := c.lenPrefix(4)
	if err != nil {
		return nil, err
	}
	s, err := c.take(n * 4)
	if err != nil {
		return nil, err
	}
	out := make([]uint32, n)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(s[i*4:])
	}
	return out, nil
}

func (c *cursor) i64Vec() ([]int64, error) {
	n, err := c.lenPrefix(8)
	if err != nil {
		return nil, err
	}
	s, err := c.take(n * 8)
	if err != nil {
		return nil, err
	}
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(binary.LittleEndian.Uint64(s[i*8:]))
	}
	return out, nil
}

func (c *cursor) f64Vec() ([]float64, error) {
	n, err := c.lenPrefix(8)
	if err != nil {
		return nil, err
	}
	s, err := c.take(n * 8)
	if err != nil {
		return nil, err
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = math.Float64frombits(binary.LittleEndian.Uint64(s[i*8:]))
	}
	return out, nil
}

func (c *cursor) str() (string, error) {
	n, err := c.lenPrefix(1)
	if err != nil {
		return "", err
	}
	s, err := c.take(n)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(s) {
		return "", corruptf("invalid utf8 at offset %d", c.pos)
	}
	return string(s), nil
}

// --- writer helpers ---------------------------------------------------------

// wLenPrefix writes a collection length as the format's u32 length prefix,
// erroring rather than silently truncating a length that exceeds u32 capacity
// (the on-disk format caps counts at 2^32-1).
func wLenPrefix(w *bytes.Buffer, n int) error {
	if n < 0 || uint64(n) > math.MaxUint32 {
		return corruptf("length %d exceeds the format's u32 capacity", n)
	}
	wU32(w, uint32(n))
	return nil
}

func wU8(w *bytes.Buffer, v uint8) {
	w.WriteByte(v)
}

func wU16(w *bytes.Buffer, v uint16) {
	w.Write(binary.LittleEndian.AppendUint16(nil, v))
}

func wU32(w *bytes.Buffer, v uint32) {
	w.Write(binary.LittleEndian.AppendUint32(nil, v))
}

func wU64(w *bytes.Buffer, v uint64) {
	w.Write(binary.LittleEndian.AppendUint64(nil, v))
}

func wI64(w *bytes.Buffer, v int64) {
	wU64(w, uint64(v))
}

func wF64(w *bytes.Buffer, v float64) {
	wU64(w, math.Float64bits(v))
}

func wU32Vec(w *bytes.Buffer, v []uint32) error {
	if err := wLenPrefix(w, len(v)); err != nil {
		return err
	}
	for _, x := range v {
		wU32(w, x)
	}
	return nil
}

func wString(w *bytes.Buffer, s string) error {
	if err := wLenPrefix(w, len(s)); err != nil {
		return err
	}
	w.WriteString(s)
	return nil
}
