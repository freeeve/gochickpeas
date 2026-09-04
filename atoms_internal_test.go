// Internal pins for atoms.go: the reverse index's laziness and its
// safety under concurrent first lookups (white-box; the black-box suite
// lives in atoms_test.go).
package chickpeas

import (
	"sync"
	"testing"
)

// TestAtomsLazySortedIndex pins the reverse index's laziness (a
// loaded-but-never-queried table must not pay the sort -- the LOAD/BI
// 2.4x regression, fixed at 64089f7) and its safety under concurrent
// first lookups.
func TestAtomsLazySortedIndex(t *testing.T) {
	a := NewAtoms([]string{"", "b", "a", "c"})
	if a.sorted != nil {
		t.Fatal("NewAtoms built the reverse index eagerly")
	}
	if _, ok := a.Resolve(2); !ok || a.sorted != nil {
		t.Fatal("Resolve must not build the reverse index")
	}
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, want := range []struct {
				s  string
				id uint32
			}{{"a", 2}, {"b", 1}, {"c", 3}, {"", 0}} {
				if id, ok := a.ID(want.s); !ok || id != want.id {
					errs <- want.s
				}
			}
			if _, ok := a.ID("zz"); ok {
				errs <- "zz-hit"
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent first lookup wrong for %q", e)
	}
}
