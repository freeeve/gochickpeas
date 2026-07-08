package rcpg_test

import (
	"errors"
	"testing"

	"github.com/freeeve/gochickpeas/rcpg"
)

// rangeRecordingFetch records every range requested through it.
type rangeRecordingFetch struct {
	m      rcpg.MemoryFetch
	ranges []rcpg.ByteRange
}

func (r *rangeRecordingFetch) Fetch(off, length uint64) ([]byte, error) {
	r.ranges = append(r.ranges, rcpg.ByteRange{Start: off, End: off + length})
	return r.m.Fetch(off, length)
}

// atomsEntry finds the atoms section (id 1) in a directory.
func atomsEntry(t *testing.T, dir *rcpg.Directory) rcpg.DirEntry {
	t.Helper()
	for _, e := range dir.Entries() {
		if e.ID == 1 {
			return e
		}
	}
	t.Fatal("file has no atoms section")
	return rcpg.DirEntry{}
}

// TestSkeletonParseSkipsAtoms checks the headline behavior across the
// corpus: a skeleton ParseLazy never requests a byte of the atoms section,
// keeps the topology, and an AtomReader over the same fetch then resolves
// every atom to the eager table.
func TestSkeletonParseSkipsAtoms(t *testing.T) {
	for _, f := range loadManifest(t).Files {
		if f.Expect != "ok" {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			raw := readCorpusFile(t, f.Name)
			eager, err := rcpg.ParseWith(raw, rcpg.TopologyOnlyParseOptions())
			if err != nil {
				t.Fatal(err)
			}
			dir, err := rcpg.ParseDirectory(raw)
			if err != nil {
				t.Fatal(err)
			}
			atoms := atomsEntry(t, dir)

			rec := &rangeRecordingFetch{m: rcpg.MemoryFetch(raw)}
			g, err := rcpg.ParseLazy(rec, rcpg.SkeletonParseOptions())
			if err != nil {
				t.Fatal(err)
			}
			if g.Atoms != nil {
				t.Fatalf("skeleton parse materialized %d atoms", len(g.Atoms))
			}
			if g.NNodes != eager.NNodes || g.NRels != eager.NRels {
				t.Fatalf("skeleton parse lost topology: %d/%d nodes, %d/%d rels",
					g.NNodes, eager.NNodes, g.NRels, eager.NRels)
			}
			for _, br := range rec.ranges {
				if br.Start < atoms.Offset+atoms.Length && br.End > atoms.Offset {
					t.Fatalf("skeleton parse fetched atoms bytes [%d, %d)", br.Start, br.End)
				}
			}

			r, err := rcpg.NewAtomReader(rcpg.MemoryFetch(raw), dir)
			if err != nil {
				t.Fatal(err)
			}
			if r.Count() != uint32(len(eager.Atoms)) {
				t.Fatalf("AtomReader.Count() = %d, want %d", r.Count(), len(eager.Atoms))
			}
			for i, want := range eager.Atoms {
				got, err := r.Atom(uint32(i))
				if err != nil {
					t.Fatalf("Atom(%d): %v", i, err)
				}
				if got != want {
					t.Fatalf("Atom(%d) = %q, want %q", i, got, want)
				}
			}
		})
	}
}

// TestSkeletonPlanExcludesAtoms checks the directory planner drops the atoms
// range under skeleton options.
func TestSkeletonPlanExcludesAtoms(t *testing.T) {
	raw := readCorpusFile(t, "all_columns.rcpg")
	dir, err := rcpg.ParseDirectory(raw)
	if err != nil {
		t.Fatal(err)
	}
	topo := dir.PlanSections(rcpg.TopologyOnlyParseOptions())
	skel := dir.PlanSections(rcpg.SkeletonParseOptions())
	if len(skel) != len(topo)-1 {
		t.Fatalf("plan sizes: skeleton=%d topology=%d, want one fewer", len(skel), len(topo))
	}
	atoms := atomsEntry(t, dir)
	for _, br := range skel {
		if br.Start < atoms.Offset+atoms.Length && br.End > atoms.Offset {
			t.Fatalf("skeleton plan includes atoms bytes [%d, %d)", br.Start, br.End)
		}
	}
}

// FuzzAtomReader: the lazy atom path must never panic, must fail only with
// typed errors, and whenever the eager parser accepts a file it must resolve
// every atom to the eager table.
func FuzzAtomReader(f *testing.F) {
	addCorpusSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		dir, err := rcpg.ParseDirectory(b)
		if err != nil {
			if !errors.Is(err, rcpg.ErrCorrupt) && !errors.Is(err, rcpg.ErrUnsupportedVersion) {
				t.Fatalf("untyped directory error: %v", err)
			}
			return
		}
		r, err := rcpg.NewAtomReader(rcpg.MemoryFetch(b), dir)
		if err != nil {
			if !errors.Is(err, rcpg.ErrCorrupt) {
				t.Fatalf("untyped open error: %v", err)
			}
			return
		}
		eager, eagerErr := rcpg.Parse(b)
		if eagerErr != nil {
			for _, id := range []uint32{0, r.Count() / 2, r.Count()} {
				if _, err := r.Atom(id); err != nil && !errors.Is(err, rcpg.ErrCorrupt) {
					t.Fatalf("untyped lookup error: %v", err)
				}
			}
			return
		}
		if r.Count() != uint32(len(eager.Atoms)) {
			t.Fatalf("Count() = %d, eager table has %d", r.Count(), len(eager.Atoms))
		}
		for i, want := range eager.Atoms {
			got, err := r.Atom(uint32(i))
			if err != nil {
				t.Fatalf("eager parse succeeded but Atom(%d) failed: %v", i, err)
			}
			if got != want {
				t.Fatalf("Atom(%d) = %q, eager %q", i, got, want)
			}
		}
	})
}
