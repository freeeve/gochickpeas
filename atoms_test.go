package chickpeas_test

import (
	"fmt"
	"sync"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

func TestAtoms(t *testing.T) {
	a := chickpeas.NewAtoms([]string{"", "Person", "KNOWS", "Person"})
	if a.Len() != 4 {
		t.Fatalf("len: got %d", a.Len())
	}
	if s, ok := a.Resolve(1); !ok || s != "Person" {
		t.Fatalf("resolve(1): got %q/%v", s, ok)
	}
	if _, ok := a.Resolve(4); ok {
		t.Fatal("out-of-range resolve succeeded")
	}
	if id, ok := a.ID("KNOWS"); !ok || id != 2 {
		t.Fatalf("id(KNOWS): got %d/%v", id, ok)
	}
	// Duplicate strings: smallest id wins reverse lookup.
	if id, _ := a.ID("Person"); id != 1 {
		t.Fatalf("duplicate reverse lookup: got %d, want 1", id)
	}
	if id, ok := a.ID(""); !ok || id != 0 {
		t.Fatal("atom 0 must be the empty string")
	}
	if _, ok := a.ID("missing"); ok {
		t.Fatal("unknown string resolved")
	}
}

func TestInterner(t *testing.T) {
	in := chickpeas.NewInterner()
	// Atom 0 is pre-interned as "".
	if id := in.GetOrIntern(""); id != 0 {
		t.Fatalf("empty string: got atom %d, want 0", id)
	}
	a := in.GetOrIntern("alpha")
	b := in.GetOrIntern("beta")
	if a == b || in.GetOrIntern("alpha") != a {
		t.Fatal("interning not stable")
	}
	if s, ok := in.Resolve(b); !ok || s != "beta" {
		t.Fatalf("resolve: got %q/%v", s, ok)
	}
	if _, ok := in.Get("gamma"); ok {
		t.Fatal("Get interned a new string")
	}
	atoms := in.Atoms()
	if atoms.Len() != in.Len() {
		t.Fatal("snapshot length mismatch")
	}
	if id, ok := atoms.ID("alpha"); !ok || id != a {
		t.Fatal("snapshot lost an atom")
	}
}

func TestInternerConcurrent(t *testing.T) {
	// Concurrent interning of overlapping strings must yield one stable id
	// per string (exercised under -race).
	in := chickpeas.NewInterner()
	const workers = 8
	const n = 200
	ids := make([][]uint32, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[w] = make([]uint32, n)
			for i := range n {
				ids[w][i] = in.GetOrIntern(fmt.Sprintf("s%d", i))
			}
		}()
	}
	wg.Wait()
	for w := 1; w < workers; w++ {
		for i := range n {
			if ids[w][i] != ids[0][i] {
				t.Fatalf("worker %d got id %d for s%d, worker 0 got %d",
					w, ids[w][i], i, ids[0][i])
			}
		}
	}
	if in.Len() != n+1 { // + atom 0
		t.Fatalf("interner holds %d strings, want %d", in.Len(), n+1)
	}
}
