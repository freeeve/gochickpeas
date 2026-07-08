// Lazy, range-fetched RCPG loading: parse the small front matter (header +
// directory), plan the byte ranges a given ParseOptions needs, and fetch only
// those — so a topology-only load over a range-capable transport never
// transfers the property columns. Most of the CSR-skeleton/working-set
// machinery the Rust codec layers on top of this is deliberately deferred;
// SectionFetch and the directory planner lock the interface it builds on,
// and atoms_lazy.go's block-lazy AtomReader is its first landed slice.

package rcpg

// SectionFetch is a byte-range provider for lazy RCPG loading: it returns
// [offset, offset+length) of an RCPG file. Implementations must return
// exactly length bytes or an error. Callers plug in files (io.ReaderAt),
// HTTP Range requests, or object-store reads.
type SectionFetch interface {
	Fetch(offset, length uint64) ([]byte, error)
}

// MemoryFetch is a SectionFetch over an in-memory RCPG slice, returning
// sub-slices with no copy — the lazy-load equivalent of eager Parse, useful
// for embedding and for parity tests of the lazy path.
type MemoryFetch []byte

// Fetch implements SectionFetch.
func (m MemoryFetch) Fetch(offset, length uint64) ([]byte, error) {
	end := offset + length
	if end < offset || end > uint64(len(m)) {
		return nil, corruptf("range %d+%d past end of file (%d)", offset, length, len(m))
	}
	return m[offset:end], nil
}

// DirEntry is one section directory entry: (id, offset, length) in file
// order.
type DirEntry struct {
	ID     uint32
	Offset uint64
	Length uint64
}

// ByteRange is a half-open [Start, End) byte range within a file.
type ByteRange struct {
	Start uint64
	End   uint64
}

// Directory is the RCPG header + section directory — the small, resident
// front matter of a file. Parse it from the leading bytes (16-byte header +
// section_count x 24-byte entries), then PlanSections to get the byte ranges
// a given ParseOptions needs, so the bodies can be range-fetched without
// transferring the whole file.
type Directory struct {
	entries []DirEntry
}

// FrontMatterLen is the number of front-matter bytes (header + directory)
// for a file with sectionCount sections — what to fetch before the directory
// can be parsed.
func FrontMatterLen(sectionCount uint32) uint64 {
	return headerLen + uint64(sectionCount)*dirEntryLen
}

// SectionCount reads the section count from a header prefix (at least 16
// bytes), validating the magic and version. Use with FrontMatterLen to size
// the directory fetch.
func SectionCount(header []byte) (uint32, error) {
	c := newCursor(header)
	magic, err := c.take(4)
	if err != nil {
		return 0, err
	}
	if [4]byte(magic) != Magic {
		return 0, corruptf("bad magic, not an RCPG file")
	}
	version, err := c.u16()
	if err != nil {
		return 0, err
	}
	// Only v1 exists; reject anything else (including the invalid v0),
	// matching RRSR's strict != Version check. Widen this to a range when a
	// v2 layout is actually defined.
	if version != Version {
		return 0, unsupportedVersionf("RCPG", version)
	}
	if _, err := c.u16(); err != nil { // flags
		return 0, err
	}
	count, err := c.u32()
	if err != nil {
		return 0, err
	}
	if count > 1024 {
		return 0, corruptf("implausible section count %d", count)
	}
	return count, nil
}

// ParseDirectory parses the header + full directory from front (which must
// hold at least FrontMatterLen bytes). Body offsets are recorded but not
// validated against any file length — the bodies live elsewhere and are
// checked when fetched.
func ParseDirectory(front []byte) (*Directory, error) {
	count, err := SectionCount(front)
	if err != nil {
		return nil, err
	}
	c := newCursor(front)
	if _, err := c.take(headerLen); err != nil {
		return nil, err
	}
	entries := make([]DirEntry, 0, preallocCap(int(count)))
	for range count {
		id, err := c.u32()
		if err != nil {
			return nil, err
		}
		if _, err := c.u32(); err != nil { // reserved
			return nil, err
		}
		offset, err := c.u64()
		if err != nil {
			return nil, err
		}
		length, err := c.u64()
		if err != nil {
			return nil, err
		}
		entries = append(entries, DirEntry{ID: id, Offset: offset, Length: length})
	}
	return &Directory{entries: entries}, nil
}

// Entries returns the directory entries in file order.
func (d *Directory) Entries() []DirEntry {
	return d.entries
}

// PlanSections returns the byte ranges to fetch for opts: the topology
// sections always, the property columns only when their option is set.
// Unknown sections are skipped. Order follows the directory, so the ranges
// are ascending and can be coalesced for batched fetches.
func (d *Directory) PlanSections(opts ParseOptions) []ByteRange {
	var ranges []ByteRange
	for _, e := range d.entries {
		if sectionWanted(e.ID, opts) {
			ranges = append(ranges, ByteRange{Start: e.Offset, End: e.Offset + e.Length})
		}
	}
	return ranges
}

// LoadPrefixLen is the smallest prefix length [0, len) that contains every
// section needed under opts — fetch this many bytes and ParseLazy over a
// MemoryFetch of them to load without the trailing (unwanted) sections.
// Because the topology sections precede the property columns in the file, a
// topology-only prefix omits the columns entirely; a full load returns the
// file length.
func (d *Directory) LoadPrefixLen(opts ParseOptions) uint64 {
	var max uint64
	found := false
	for _, e := range d.entries {
		if !sectionWanted(e.ID, opts) {
			continue
		}
		found = true
		if end := e.Offset + e.Length; end > max {
			max = end
		}
	}
	if !found {
		return FrontMatterLen(uint32(len(d.entries)))
	}
	return max
}

// ParseLazy parses an RCPG file through a SectionFetch, transferring only
// the sections opts needs. It reads the header, then the directory, then
// fetches and decodes each wanted section body — so a topology-only load
// over a range-capable transport never transfers the property columns.
// Produces a GraphSection identical to ParseWith for the same opts.
func ParseLazy(f SectionFetch, opts ParseOptions) (*GraphSection, error) {
	header, err := f.Fetch(0, headerLen)
	if err != nil {
		return nil, err
	}
	count, err := SectionCount(header)
	if err != nil {
		return nil, err
	}
	front, err := f.Fetch(0, FrontMatterLen(count))
	if err != nil {
		return nil, err
	}
	dir, err := ParseDirectory(front)
	if err != nil {
		return nil, err
	}
	g := &GraphSection{}
	for _, e := range dir.Entries() {
		if !sectionWanted(e.ID, opts) {
			continue
		}
		body, err := f.Fetch(e.Offset, e.Length)
		if err != nil {
			return nil, err
		}
		if uint64(len(body)) != e.Length {
			return nil, corruptf("section %d fetch returned %d bytes, expected %d",
				e.ID, len(body), e.Length)
		}
		if err := decodeSection(e.ID, body, opts, g); err != nil {
			return nil, err
		}
	}
	return g, nil
}
