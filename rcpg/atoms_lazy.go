// Block-lazy atom resolution over a SectionFetch: the first consumer-shaped
// slice of the working-set machinery lazy.go defers. An AtomReader keeps a
// small resident router (one byte offset per block of atoms) and range-fetches
// one block per lookup, so a skeleton parse (SkeletonParseOptions) plus an
// AtomReader serves traversal with a hot atom set without ever holding the
// atom table resident. RCPG v1 stores atoms as contiguous length-prefixed
// strings with no in-file offset index, so the router is discovered by
// scanning length prefixes forward, incrementally, at most once per file.
package rcpg

import (
	"encoding/binary"
	"sync"
)

const (
	// atomBlockLen is the number of atoms per router block. LCSH-scale
	// tables (~500k atoms, ~90B mean) route with ~4KB resident and decode
	// ~90KB per block fetch.
	atomBlockLen = 1024
	// atomScanChunk is the fetch granularity of the forward prefix scan
	// that discovers block boundaries.
	atomScanChunk = 512 << 10
	// atomCacheBlocks bounds the decoded blocks kept resident (FIFO).
	atomCacheBlocks = 32
)

// AtomReader resolves atom ids on demand through a SectionFetch, without
// materializing the atom table. Lookups route through a resident block
// index (built incrementally by scanning the section's length prefixes up
// to the highest block touched so far) and fetch + decode one block of
// atoms, kept in a small FIFO cache. Structural bounds are validated during
// the scan; UTF-8 validity is checked per decoded block, like the eager
// decoder but only for atoms actually read.
//
// An AtomReader is safe for concurrent use; lookups that miss the cache
// serialize on the underlying fetch.
type AtomReader struct {
	f    SectionFetch
	off  uint64 // file offset of the atoms section body
	size uint64 // atoms section length
	n    uint32 // atom count (the section's leading u32)

	blockLen  uint32
	scanChunk uint64
	cacheCap  int

	mu       sync.Mutex
	starts   []uint64 // starts[b]: body-relative offset of atom b*blockLen
	frontier uint64   // body-relative offset of the first unscanned atom
	scanned  uint32   // atoms whose extents are known
	cache    map[uint32][]string
	fifo     []uint32
}

// NewAtomReader opens block-lazy atom resolution over f, locating the atoms
// section through dir and fetching only its 4-byte count header. Errors wrap
// ErrCorrupt when the directory has no atoms section or the count cannot fit
// the section.
func NewAtomReader(f SectionFetch, dir *Directory) (*AtomReader, error) {
	return newAtomReader(f, dir, atomBlockLen, atomScanChunk, atomCacheBlocks)
}

// newAtomReader is NewAtomReader with the tuning knobs exposed for tests
// (small blocks and chunks exercise routing, spanning, and eviction on
// small inputs).
func newAtomReader(f SectionFetch, dir *Directory, blockLen uint32, scanChunk uint64, cacheCap int) (*AtomReader, error) {
	// Last matching entry wins, mirroring the eager path (each decode
	// overwrites, so a duplicate section's final occurrence is the one a
	// full parse materializes).
	var entry DirEntry
	found := false
	for _, e := range dir.Entries() {
		if e.ID == sectionAtoms {
			entry, found = e, true
		}
	}
	if !found {
		return nil, corruptf("no atoms section in directory")
	}
	if entry.Length < 4 {
		return nil, corruptf("atoms section too short for its count (%d bytes)", entry.Length)
	}
	head, err := f.Fetch(entry.Offset, 4)
	if err != nil {
		return nil, err
	}
	count := binary.LittleEndian.Uint32(head)
	if uint64(count) > entry.Length-4 {
		return nil, corruptf("declared atom count %d exceeds section body (%d bytes)",
			count, entry.Length-4)
	}
	// Floors keep the machinery sound at any tuning: a chunk must hold at
	// least a length prefix (the stall detector relies on it) and the FIFO
	// needs one slot to bound residency.
	return &AtomReader{
		f:         f,
		off:       entry.Offset,
		size:      entry.Length,
		n:         count,
		blockLen:  max(blockLen, 1),
		scanChunk: max(scanChunk, 16),
		cacheCap:  max(cacheCap, 1),
		starts:    []uint64{4},
		frontier:  4,
		cache:     map[uint32][]string{},
	}, nil
}

// Count is the number of atoms in the table.
func (r *AtomReader) Count() uint32 {
	return r.n
}

// Atom resolves one atom id, fetching and decoding its block on a cache
// miss. Ids at or past Count error (a conforming file never references
// them), wrapping ErrCorrupt.
func (r *AtomReader) Atom(id uint32) (string, error) {
	if id >= r.n {
		return "", corruptf("atom id %d out of range (%d atoms)", id, r.n)
	}
	b := id / r.blockLen
	r.mu.Lock()
	defer r.mu.Unlock()
	blk, ok := r.cache[b]
	if !ok {
		var err error
		if blk, err = r.loadBlock(b); err != nil {
			return "", err
		}
	}
	return blk[id%r.blockLen], nil
}

// loadBlock fetches and decodes block b, caching it under the FIFO bound.
// Caller holds r.mu.
func (r *AtomReader) loadBlock(b uint32) ([]string, error) {
	last := uint32(min(uint64(b+1)*uint64(r.blockLen), uint64(r.n)))
	if err := r.ensureScanned(last); err != nil {
		return nil, err
	}
	start := r.starts[b]
	end := r.frontier
	if int(b)+1 < len(r.starts) {
		end = r.starts[b+1]
	}
	body, err := r.f.Fetch(r.off+start, end-start)
	if err != nil {
		return nil, err
	}
	c := newCursor(body)
	blk := make([]string, last-b*r.blockLen)
	for i := range blk {
		if blk[i], err = c.str(); err != nil {
			return nil, err
		}
	}
	if c.remaining() != 0 {
		return nil, corruptf("atom block %d decoded short: %d trailing bytes", b, c.remaining())
	}
	if len(r.cache) >= r.cacheCap {
		delete(r.cache, r.fifo[0])
		r.fifo = r.fifo[1:]
	}
	r.cache[b] = blk
	r.fifo = append(r.fifo, b)
	return blk, nil
}

// ensureScanned advances the forward prefix scan until the extents of the
// first target atoms are known, recording a router entry at every block
// boundary it crosses. A string spanning the current chunk raises the next
// fetch to cover it whole, so every round either consumes at least one atom
// or errors -- the scan cannot stall. Caller holds r.mu.
func (r *AtomReader) ensureScanned(target uint32) error {
	need := r.scanChunk
	for r.scanned < target {
		remain := r.size - r.frontier
		if remain == 0 {
			return corruptf("atoms section truncated: %d of %d atoms present", r.scanned, r.n)
		}
		buf, err := r.f.Fetch(r.off+r.frontier, min(need, remain))
		if err != nil {
			return err
		}
		need = r.scanChunk
		pos := uint64(0)
		for r.scanned < target && pos+4 <= uint64(len(buf)) {
			slen := uint64(binary.LittleEndian.Uint32(buf[pos:]))
			if r.frontier+pos+4+slen > r.size {
				return corruptf("atom %d (len %d) extends past its section (%d bytes)",
					r.scanned, slen, r.size)
			}
			if pos+4+slen > uint64(len(buf)) {
				need = 4 + slen
				break
			}
			pos += 4 + slen
			r.scanned++
			if r.scanned%r.blockLen == 0 {
				r.starts = append(r.starts, r.frontier+pos)
			}
		}
		if pos == 0 && need == r.scanChunk {
			return corruptf("atoms section truncated: %d of %d atoms present", r.scanned, r.n)
		}
		r.frontier += pos
	}
	return nil
}
