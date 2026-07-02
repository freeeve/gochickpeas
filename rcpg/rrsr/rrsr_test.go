package rrsr_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/freeeve/gochickpeas/rcpg/rrsr"
)

func write(t *testing.T, records ...[]byte) (idx, bin []byte) {
	t.Helper()
	var ib, bb bytes.Buffer
	if err := rrsr.Write(&ib, &bb, slices.Values(records)); err != nil {
		t.Fatal(err)
	}
	return ib.Bytes(), bb.Bytes()
}

func TestRoundTripAndRanges(t *testing.T) {
	records := [][]byte{[]byte("alpha"), {}, []byte("beta"), []byte("g")}
	idx, bin := write(t, records...)

	ri, err := rrsr.Parse(idx)
	if err != nil {
		t.Fatal(err)
	}
	if ri.Len() != len(records) {
		t.Fatalf("len: got %d, want %d", ri.Len(), len(records))
	}
	for i, want := range records {
		rng, ok := ri.RecordRange(uint32(i))
		if !ok {
			t.Fatalf("record %d out of range", i)
		}
		if got := bin[rng.Start:rng.End]; !bytes.Equal(got, want) {
			t.Fatalf("record %d: got %q, want %q", i, got, want)
		}
	}
	if _, ok := ri.RecordRange(uint32(len(records))); ok {
		t.Fatal("out-of-range id returned a range")
	}
}

func TestPlanRangesCoalesces(t *testing.T) {
	// Records of 10 bytes each: 0..9, 10..19, 20..29, 30..39.
	idx, _ := write(t, make([]byte, 10), make([]byte, 10), make([]byte, 10), make([]byte, 10))
	ri, err := rrsr.Parse(idx)
	if err != nil {
		t.Fatal(err)
	}
	// A gap of exactly maxGap coalesces (start <= end + maxGap), so the
	// duplicate, unsorted, and out-of-range ids collapse to one read.
	got := ri.PlanRanges([]uint32{3, 0, 1, 0, 99}, 10)
	want := []rrsr.ByteRange{{Start: 0, End: 40}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// One byte under the gap splits the read.
	got = ri.PlanRanges([]uint32{3, 0, 1, 0, 99}, 9)
	want = []rrsr.ByteRange{{Start: 0, End: 20}, {Start: 30, End: 40}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// Zero gap tolerance splits the non-adjacent read.
	got = ri.PlanRanges([]uint32{0, 2}, 0)
	want = []rrsr.ByteRange{{Start: 0, End: 10}, {Start: 20, End: 30}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	idx, _ := write(t, []byte("x"))

	corrupt := func(mutate func([]byte)) []byte {
		b := bytes.Clone(idx)
		mutate(b)
		return b
	}
	cases := map[string]struct {
		input []byte
		want  error
	}{
		"truncated":     {idx[:8], rrsr.ErrCorrupt},
		"bad magic":     {corrupt(func(b []byte) { copy(b, "NOPE") }), rrsr.ErrCorrupt},
		"version 2":     {corrupt(func(b []byte) { binary.LittleEndian.PutUint16(b[4:], 2) }), rrsr.ErrUnsupportedVersion},
		"length lie":    {corrupt(func(b []byte) { binary.LittleEndian.PutUint32(b[8:], 7) }), rrsr.ErrCorrupt},
		"non-monotonic": {corrupt(func(b []byte) { binary.LittleEndian.PutUint64(b[16:], 99) }), rrsr.ErrCorrupt},
	}
	for name, tc := range cases {
		if _, err := rrsr.Parse(tc.input); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
}

// FuzzParse: Parse must never panic, and any success must expose only
// in-bounds, monotonic record ranges.
func FuzzParse(f *testing.F) {
	var ib, bb bytes.Buffer
	records := [][]byte{[]byte("alpha"), {}, []byte("beta")}
	if err := rrsr.Write(&ib, &bb, slices.Values(records)); err != nil {
		f.Fatal(err)
	}
	f.Add(ib.Bytes())
	f.Fuzz(func(t *testing.T, b []byte) {
		ri, err := rrsr.Parse(b)
		if err != nil {
			if !errors.Is(err, rrsr.ErrCorrupt) && !errors.Is(err, rrsr.ErrUnsupportedVersion) {
				t.Fatalf("untyped parse error: %v", err)
			}
			return
		}
		var prev uint64
		for i := range ri.Len() {
			rng, ok := ri.RecordRange(uint32(i))
			if !ok {
				t.Fatalf("record %d unexpectedly out of range", i)
			}
			if rng.Start > rng.End || rng.Start < prev {
				t.Fatalf("record %d range %v not monotonic", i, rng)
			}
			prev = rng.End
		}
	})
}
