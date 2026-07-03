package ldbc

import (
	"fmt"
	"os"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// TestDebugNativeQuery diffs one kernel's rows against its ref file --
// a debugging harness, active only with the env vars set:
//
//	GOCHICKPEAS_DEBUG_RCPG=<graph> GOCHICKPEAS_DEBUG_QUERY=FinBench/CR8 \
//	GOCHICKPEAS_DEBUG_REF=<ref.json> go test ./internal/ldbc -run TestDebugNativeQuery -v
func TestDebugNativeQuery(t *testing.T) {
	rcpg := os.Getenv("GOCHICKPEAS_DEBUG_RCPG")
	query := os.Getenv("GOCHICKPEAS_DEBUG_QUERY")
	ref := os.Getenv("GOCHICKPEAS_DEBUG_REF")
	if rcpg == "" || query == "" || ref == "" {
		t.Skip("debug env vars unset")
	}
	g, err := chickpeas.ReadRCPGFile(rcpg)
	if err != nil {
		t.Fatal(err)
	}
	var family, q string
	fmt.Sscanf(query, "%9s/%9s", &family, &q)
	for i, c := range query {
		if c == '/' {
			family, q = query[:i], query[i+1:]
		}
	}
	kernel, ok := NativeKernelFor(family, q)
	if !ok {
		t.Fatalf("no kernel %s", query)
	}
	run, err := kernel(g)
	if err != nil {
		t.Fatal(err)
	}
	got, err := run()
	if err != nil {
		t.Fatal(err)
	}
	want, err := LoadRefRows(ref)
	if err != nil {
		t.Fatal(err)
	}
	enc := func(rows [][]any) map[string]int {
		m := map[string]int{}
		for _, r := range rows {
			s, err := CanonCell(any([]any(r)))
			if err != nil {
				t.Fatal(err)
			}
			m[s]++
		}
		return m
	}
	gm, wm := enc(got), enc(want)
	for s, n := range gm {
		if wm[s] != n {
			t.Errorf("got %dx %s (want %dx)", n, s, wm[s])
		}
	}
	for s, n := range wm {
		if gm[s] != n {
			t.Errorf("want %dx %s (got %dx)", n, s, gm[s])
		}
	}
	t.Logf("%d got rows, %d want rows", len(got), len(want))
}

// BenchmarkDebugNativeQuery times/profiles one kernel's runnable -- the
// alloc-pass harness (task 028), active only with the env vars set:
//
//	GOCHICKPEAS_DEBUG_RCPG=<graph> GOCHICKPEAS_DEBUG_QUERY=IC/IC6 \
//	go test ./internal/ldbc -run xxx -bench BenchmarkDebugNativeQuery -benchmem -memprofile mem.out
func BenchmarkDebugNativeQuery(b *testing.B) {
	rcpg := os.Getenv("GOCHICKPEAS_DEBUG_RCPG")
	query := os.Getenv("GOCHICKPEAS_DEBUG_QUERY")
	if rcpg == "" || query == "" {
		b.Skip("debug env vars unset")
	}
	g, err := chickpeas.ReadRCPGFile(rcpg)
	if err != nil {
		b.Fatal(err)
	}
	var family, q string
	for i, c := range query {
		if c == '/' {
			family, q = query[:i], query[i+1:]
		}
	}
	kernel, ok := NativeKernelFor(family, q)
	if !ok {
		b.Fatalf("no kernel %s", query)
	}
	run, err := kernel(g)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := run(); err != nil { // warm lazy indexes outside the loop
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := run(); err != nil {
			b.Fatal(err)
		}
	}
}
