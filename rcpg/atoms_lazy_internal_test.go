// White-box AtomReader tests: small blocks, chunks, and caches exercise the
// router scan, chunk-spanning strings, FIFO eviction, and the corrupt paths
// on inputs small enough to reason about byte by byte.
package rcpg

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// atomsFixture writes a graph whose atom table is atoms, returning the file
// bytes and parsed directory.
func atomsFixture(t *testing.T, atoms []string) ([]byte, *Directory) {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&GraphSection{Atoms: atoms}, &buf); err != nil {
		t.Fatal(err)
	}
	dir, err := ParseDirectory(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), dir
}

// variedAtoms builds n atoms of varied lengths, atom 0 empty per convention.
func variedAtoms(n int) []string {
	atoms := make([]string, n)
	for i := 1; i < n; i++ {
		atoms[i] = fmt.Sprintf("atom-%d-%s", i, strings.Repeat("x", i%97))
	}
	return atoms
}

// countingFetch counts calls and bytes through an in-memory fetch.
type countingFetch struct {
	m     MemoryFetch
	calls int
	bytes uint64
}

func (c *countingFetch) Fetch(off, length uint64) ([]byte, error) {
	c.calls++
	c.bytes += length
	return c.m.Fetch(off, length)
}

// atomsSection returns the atoms directory entry of a fixture.
func atomsSection(t *testing.T, dir *Directory) DirEntry {
	t.Helper()
	for _, e := range dir.Entries() {
		if e.ID == sectionAtoms {
			return e
		}
	}
	t.Fatal("fixture has no atoms section")
	return DirEntry{}
}

// TestAtomReaderParityTuned sweeps and random-probes a multi-block table
// under a cache far smaller than the block count, against the eager table.
func TestAtomReaderParityTuned(t *testing.T) {
	atoms := variedAtoms(1000)
	raw, dir := atomsFixture(t, atoms)
	r, err := newAtomReader(MemoryFetch(raw), dir, 16, 256, 3)
	if err != nil {
		t.Fatal(err)
	}
	if r.Count() != uint32(len(atoms)) {
		t.Fatalf("Count() = %d, want %d", r.Count(), len(atoms))
	}
	rng := rand.New(rand.NewSource(1))
	for range 5000 {
		id := uint32(rng.Intn(len(atoms)))
		got, err := r.Atom(id)
		if err != nil {
			t.Fatalf("Atom(%d): %v", id, err)
		}
		if got != atoms[id] {
			t.Fatalf("Atom(%d) = %q, want %q", id, got, atoms[id])
		}
	}
	for id := range atoms {
		got, err := r.Atom(uint32(id))
		if err != nil || got != atoms[id] {
			t.Fatalf("sweep Atom(%d) = %q, %v; want %q", id, got, err, atoms[id])
		}
	}
	if len(r.cache) > 3 {
		t.Fatalf("cache holds %d blocks, cap 3", len(r.cache))
	}
}

// TestAtomReaderSpanningString covers an atom much longer than the scan
// chunk: the scan must raise its fetch to cover the atom whole and continue.
func TestAtomReaderSpanningString(t *testing.T) {
	atoms := []string{"", "small", strings.Repeat("y", 5000), "after"}
	raw, dir := atomsFixture(t, atoms)
	r, err := newAtomReader(MemoryFetch(raw), dir, 2, 64, 2)
	if err != nil {
		t.Fatal(err)
	}
	for id := len(atoms) - 1; id >= 0; id-- {
		got, err := r.Atom(uint32(id))
		if err != nil || got != atoms[id] {
			t.Fatalf("Atom(%d) len %d, err %v; want len %d", id, len(got), err, len(atoms[id]))
		}
	}
}

// TestAtomReaderFetchLocality confirms the point of the exercise: a low-id
// lookup transfers a small prefix of the section, not the table, and a
// cached re-read transfers nothing.
func TestAtomReaderFetchLocality(t *testing.T) {
	raw, dir := atomsFixture(t, variedAtoms(1000))
	section := atomsSection(t, dir)
	cf := &countingFetch{m: MemoryFetch(raw)}
	r, err := newAtomReader(cf, dir, 16, 256, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Atom(3); err != nil {
		t.Fatal(err)
	}
	if cf.bytes > section.Length/4 {
		t.Fatalf("low-id lookup fetched %d of %d section bytes", cf.bytes, section.Length)
	}
	before := cf.calls
	if _, err := r.Atom(4); err != nil {
		t.Fatal(err)
	}
	if cf.calls != before {
		t.Fatalf("cached block re-read issued %d extra fetches", cf.calls-before)
	}
}

// TestAtomReaderCorrupt covers the typed failure paths: an id past the
// table, a count that cannot fit the section, a string length running past
// the section, and a section that ends before its declared count.
func TestAtomReaderCorrupt(t *testing.T) {
	raw, dir := atomsFixture(t, variedAtoms(64))
	section := atomsSection(t, dir)
	r, err := newAtomReader(MemoryFetch(raw), dir, 8, 64, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Atom(64); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("out-of-range id: got %v, want ErrCorrupt", err)
	}

	badCount := bytes.Clone(raw)
	binary.LittleEndian.PutUint32(badCount[section.Offset:], ^uint32(0))
	if _, err := newAtomReader(MemoryFetch(badCount), dir, 8, 64, 2); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("oversized count: got %v, want ErrCorrupt", err)
	}

	badLen := bytes.Clone(raw)
	binary.LittleEndian.PutUint32(badLen[section.Offset+4:], ^uint32(0))
	r2, err := newAtomReader(MemoryFetch(badLen), dir, 8, 64, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Atom(0); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("oversized string length: got %v, want ErrCorrupt", err)
	}

	// Hand-built section declaring 3 atoms but containing 2.
	var body bytes.Buffer
	wU32(&body, 3)
	for _, s := range []string{"a", "b"} {
		if err := wString(&body, s); err != nil {
			t.Fatal(err)
		}
	}
	truncDir := &Directory{entries: []DirEntry{
		{ID: sectionAtoms, Offset: 0, Length: uint64(body.Len())},
	}}
	r3, err := newAtomReader(MemoryFetch(body.Bytes()), truncDir, 2, 16, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r3.Atom(2); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("truncated section: got %v, want ErrCorrupt", err)
	}
}

// TestAtomReaderNoAtomsSection covers a directory with no atoms entry.
func TestAtomReaderNoAtomsSection(t *testing.T) {
	dir := &Directory{entries: []DirEntry{{ID: sectionMeta, Offset: 0, Length: 1}}}
	if _, err := NewAtomReader(MemoryFetch{0}, dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("missing atoms section: got %v, want ErrCorrupt", err)
	}
}

// indexedAtomsFixture writes a graph carrying the section-7 atom index.
func indexedAtomsFixture(t *testing.T, atoms []string) ([]byte, *Directory) {
	t.Helper()
	var buf bytes.Buffer
	opts := DefaultWriteOptions()
	opts.AtomIndex = true
	if err := WriteWith(&GraphSection{Atoms: atoms}, &buf, opts); err != nil {
		t.Fatal(err)
	}
	dir, err := ParseDirectory(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), dir
}

// TestAtomReaderAdoptsIndex pins the section-7 consumption: a reader over an
// indexed file resolves every atom identically to the scan path, never runs
// the forward prefix scan (no fetch beyond the count header, the index body,
// and the probed blocks), and adopts the FILE's block length.
func TestAtomReaderAdoptsIndex(t *testing.T) {
	atoms := variedAtoms(3000) // ~3 blocks at the writer's 1024
	raw, dir := indexedAtomsFixture(t, atoms)
	cf := &countingFetch{m: MemoryFetch(raw)}
	r, err := NewAtomReader(cf, dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.scanned != uint32(len(atoms)) || len(r.starts) != 3 || r.blockLen != atomIndexBlockLen {
		t.Fatalf("index not adopted: scanned=%d starts=%d blockLen=%d", r.scanned, len(r.starts), r.blockLen)
	}
	// A late atom must cost exactly one further fetch (its block), with no
	// scan fetches in between.
	before := cf.calls
	got, err := r.Atom(uint32(len(atoms) - 1))
	if err != nil {
		t.Fatal(err)
	}
	if got != atoms[len(atoms)-1] {
		t.Fatalf("late atom: got %q, want %q", got, atoms[len(atoms)-1])
	}
	if cf.calls != before+1 {
		t.Fatalf("late atom cost %d fetches, want 1", cf.calls-before)
	}
	// Full parity across all atoms.
	for i, want := range atoms {
		s, err := r.Atom(uint32(i))
		if err != nil {
			t.Fatalf("atom %d: %v", i, err)
		}
		if s != want {
			t.Fatalf("atom %d: got %q, want %q", i, s, want)
		}
	}
}

// TestAtomReaderRejectsBadIndex pins the no-silent-fallback contract: a
// present index that fails validation is corrupt at reader construction.
func TestAtomReaderRejectsBadIndex(t *testing.T) {
	atoms := variedAtoms(2500)
	raw, dir := indexedAtomsFixture(t, atoms)
	var idx DirEntry
	for _, e := range dir.Entries() {
		if e.ID == sectionAtomIndex {
			idx = e
		}
	}
	corrupt := func(mutate func(b []byte)) error {
		bad := append([]byte(nil), raw...)
		mutate(bad[idx.Offset : idx.Offset+idx.Length])
		badDir, err := ParseDirectory(bad)
		if err != nil {
			t.Fatal(err)
		}
		_, err = NewAtomReader(MemoryFetch(bad), badDir)
		return err
	}
	for name, mutate := range map[string]func(b []byte){
		"zero block_len":  func(b []byte) { binary.LittleEndian.PutUint32(b[0:], 0) },
		"wrong count":     func(b []byte) { binary.LittleEndian.PutUint32(b[8:], 1) },
		"entry0 not 4":    func(b []byte) { binary.LittleEndian.PutUint64(b[12:], 5) },
		"offset past end": func(b []byte) { binary.LittleEndian.PutUint64(b[12+16:], 1<<40) },
	} {
		if err := corrupt(mutate); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("%s: got %v, want ErrCorrupt", name, err)
		}
	}
}
