// Package rrsr reads and writes the RRSR record store, byte-compatible with
// roaringrange's RECORDS.md spec and the Rust rustychickpeas-format codec.
//
// Layout (all integers little-endian):
//
//	.idx: magic "RRSR" | version u16 | reserved u16 | count u32 | reserved2 u32
//	      then (count + 1) x u64 offsets into .bin
//	.bin: record payloads concatenated in ID order; record d spans
//	      bin[off[d] .. off[d+1]]
//
// This package implements version 1 (raw payloads). Version 2 framing
// (per-record zstd with a shared dictionary) is read-rejected here.
package rrsr

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"slices"
)

// ErrCorrupt reports input bytes that are not a valid RRSR index.
var ErrCorrupt = errors.New("corrupt input")

// ErrUnsupportedVersion reports an RRSR index of a newer, unsupported
// version.
var ErrUnsupportedVersion = errors.New("unsupported version")

// Magic is the RRSR index file magic.
var Magic = [4]byte{'R', 'R', 'S', 'R'}

// Version is the RRSR format version this package reads and writes.
const Version uint16 = 1

const headerLen = 16

// ByteRange is a half-open [Start, End) byte range within the .bin file.
type ByteRange struct {
	Start uint64
	End   uint64
}

// Write writes a version-1 record store. records yields payloads in ID
// order (record i belongs to ID i). Produces the .idx and .bin streams.
func Write(idx, bin io.Writer, records iter.Seq[[]byte]) error {
	offsets := []uint64{0}
	var pos uint64
	for record := range records {
		if _, err := bin.Write(record); err != nil {
			return err
		}
		pos += uint64(len(record))
		offsets = append(offsets, pos)
	}
	count := len(offsets) - 1
	if uint64(count) > math.MaxUint32 {
		return fmt.Errorf("%w: record count %d exceeds the format's u32 capacity", ErrCorrupt, count)
	}

	head := make([]byte, 0, headerLen+len(offsets)*8)
	head = append(head, Magic[:]...)
	head = binary.LittleEndian.AppendUint16(head, Version)
	head = binary.LittleEndian.AppendUint16(head, 0) // reserved
	head = binary.LittleEndian.AppendUint32(head, uint32(count))
	head = binary.LittleEndian.AppendUint32(head, 0) // reserved2
	for _, off := range offsets {
		head = binary.LittleEndian.AppendUint64(head, off)
	}
	_, err := idx.Write(head)
	return err
}

// RecordIndex is a parsed .idx file: offsets into the .bin payload stream.
type RecordIndex struct {
	offsets []uint64
}

// Parse parses a version-1 .idx file. It never panics on malformed input;
// structural problems surface as errors wrapping ErrCorrupt (or
// ErrUnsupportedVersion for a newer format version).
func Parse(b []byte) (*RecordIndex, error) {
	if len(b) < headerLen {
		return nil, fmt.Errorf("%w: index shorter than its header", ErrCorrupt)
	}
	if [4]byte(b[:4]) != Magic {
		return nil, fmt.Errorf("%w: bad magic, not an RRSR index", ErrCorrupt)
	}
	version := binary.LittleEndian.Uint16(b[4:6])
	if version != Version {
		return nil, fmt.Errorf("%w: RRSR version %d", ErrUnsupportedVersion, version)
	}
	count := binary.LittleEndian.Uint32(b[8:12])
	// Compute the expected length in uint64: (count + 1) * 8 must not be
	// evaluated in a narrower type where a large file-declared count could
	// overflow and spuriously match a short buffer.
	expected := uint64(headerLen) + (uint64(count)+1)*8
	if uint64(len(b)) != expected {
		return nil, fmt.Errorf("%w: index length %d doesn't match count %d", ErrCorrupt, len(b), count)
	}
	offsets := make([]uint64, count+1)
	for i := range offsets {
		offsets[i] = binary.LittleEndian.Uint64(b[headerLen+i*8:])
	}
	for i := 1; i < len(offsets); i++ {
		if offsets[i-1] > offsets[i] {
			return nil, fmt.Errorf("%w: offsets not monotonic", ErrCorrupt)
		}
	}
	return &RecordIndex{offsets: offsets}, nil
}

// Len returns the number of records.
func (r *RecordIndex) Len() int {
	return len(r.offsets) - 1
}

// RecordRange returns the byte range of record id in the .bin file; ok is
// false when id is out of range.
func (r *RecordIndex) RecordRange(id uint32) (rng ByteRange, ok bool) {
	i := int(id)
	if i+1 >= len(r.offsets) {
		return ByteRange{}, false
	}
	return ByteRange{Start: r.offsets[i], End: r.offsets[i+1]}, true
}

// PlanRanges plans batched range reads for a set of record IDs: it sorts and
// deduplicates the IDs, then coalesces ranges whose gap is at most maxGap
// bytes into single reads. Returns byte ranges over the .bin file in
// ascending order. IDs out of range are ignored; empty records fetch
// nothing.
func (r *RecordIndex) PlanRanges(ids []uint32, maxGap uint64) []ByteRange {
	sorted := make([]uint32, 0, len(ids))
	for _, id := range ids {
		if int(id)+1 < len(r.offsets) {
			sorted = append(sorted, id)
		}
	}
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)

	var ranges []ByteRange
	for _, id := range sorted {
		start, end := r.offsets[id], r.offsets[id+1]
		if start == end {
			continue // empty record, nothing to fetch
		}
		if n := len(ranges); n > 0 && start <= ranges[n-1].End+maxGap {
			ranges[n-1].End = max(ranges[n-1].End, end)
		} else {
			ranges = append(ranges, ByteRange{Start: start, End: end})
		}
	}
	return ranges
}
