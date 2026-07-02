// Group keys: a canonical byte encoding of a value for DISTINCT / grouping
// / dedup map keys, port of the Rust GroupKey enum (Go maps can't key on
// slice-bearing structs, so the projection is a byte string instead of a
// hashable enum). Encounter order is preserved by the callers' side slice.
package value

import (
	"encoding/binary"
	"math"
)

// Encoding tags. Distinct from Kind: an integral float canonicalizes to the
// Int tag so DISTINCT / grouping agree with = (1 and 1.0 are one group).
const (
	tagNull byte = iota
	tagBool
	tagInt
	tagFloat
	tagStr
	tagList
	tagNode
	tagRel
	tagPath
	tagMap
	tagTemporal
	tagDuration
)

// Key is the canonical byte encoding of v as a string, for map keys.
func Key(v Value) string { return string(AppendKey(nil, v)) }

// AppendKey appends the canonical byte encoding of v to b: two values
// encode identically iff they are one DISTINCT/group key. Floats key by bit
// pattern, except an integral float in i64 range canonicalizes to the equal
// integer (and -0.0 to 0.0) so grouping agrees with =; NaN keeps its bit
// pattern. Map entries encode in key-sorted order, so insertion order does
// not split groups.
func AppendKey(b []byte, v Value) []byte {
	switch v.kind {
	case KindNull:
		return append(b, tagNull)
	case KindBool:
		return append(b, tagBool, byte(v.num))
	case KindInt:
		return appendU64(append(b, tagInt), v.num)
	case KindFloat:
		return appendFloatKey(b, math.Float64frombits(v.num))
	case KindStr:
		b = appendLen(append(b, tagStr), len(v.str))
		return append(b, v.str...)
	case KindList:
		b = appendLen(append(b, tagList), len(v.ext.list))
		for _, e := range v.ext.list {
			b = AppendKey(b, e)
		}
		return b
	case KindNode:
		return appendU32(append(b, tagNode), uint32(v.num))
	case KindRel:
		return appendU32(append(b, tagRel), uint32(v.num))
	case KindPath:
		b = appendLen(append(b, tagPath), len(v.ext.nodes))
		for _, n := range v.ext.nodes {
			b = appendU32(b, uint32(n))
		}
		b = appendLen(b, len(v.ext.rels))
		for _, r := range v.ext.rels {
			b = appendU32(b, r)
		}
		return b
	case KindMap:
		b = appendLen(append(b, tagMap), len(v.ext.entries))
		for _, i := range keySorted(v.ext.entries) {
			e := v.ext.entries[i]
			b = appendLen(b, len(e.Key))
			b = append(b, e.Key...)
			b = AppendKey(b, e.Val)
		}
		return b
	case KindTemporal:
		return append(appendU64(append(b, tagTemporal), v.num), v.aux)
	case KindDuration:
		b = appendU64(append(b, tagDuration), uint64(v.ext.months))
		b = appendU64(b, uint64(v.ext.days))
		return appendU64(b, v.num)
	}
	return append(b, tagNull)
}

// appendFloatKey canonicalizes an integral float in i64 range to the Int
// tag (the half-open bound matches the Rust float_group_key exactly); other
// floats key by bit pattern.
func appendFloatKey(b []byte, f float64) []byte {
	if f == math.Trunc(f) && f >= math.MinInt64 && f < 9223372036854775808.0 {
		return appendU64(append(b, tagInt), uint64(int64(f)))
	}
	return appendU64(append(b, tagFloat), math.Float64bits(f))
}

func appendU64(b []byte, v uint64) []byte { return binary.BigEndian.AppendUint64(b, v) }

func appendU32(b []byte, v uint32) []byte { return binary.BigEndian.AppendUint32(b, v) }

func appendLen(b []byte, n int) []byte { return appendU32(b, uint32(n)) }
