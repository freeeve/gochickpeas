// Default-cache contract (task 200): Run routes through the snapshot's
// implicit plan cache -- repeats hit L1, distinct snapshots stay
// isolated, and results are identical to the uncached path.
package gql

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

func dcFixture(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(8, 8)
	n, err := b.AddNode("P")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SetProp(n, "name", "x"); err != nil {
		t.Fatal(err)
	}
	return b.Finalize("dc")
}

func TestDefaultCacheRunContract(t *testing.T) {
	g := dcFixture(t)
	q := "MATCH (p:P) RETURN p.name AS n"
	count := func(run func() (*Rows, error)) int {
		t.Helper()
		rows, err := run()
		if err != nil {
			t.Fatal(err)
		}
		k := 0
		for range rows.All() {
			k++
		}
		return k
	}
	c := DefaultCacheFor(g)
	h1 := c.hitsL1
	if count(func() (*Rows, error) { return Run(g, q) }) != 1 {
		t.Fatal("first run wrong rows")
	}
	if count(func() (*Rows, error) { return Run(g, q) }) != 1 {
		t.Fatal("second run wrong rows")
	}
	h2 := c.hitsL1
	if h2 == h1 {
		t.Fatalf("second Run did not hit the default cache (l1 hits %d -> %d)", h1, h2)
	}
	if DefaultCacheFor(g) != c {
		t.Fatal("DefaultCacheFor not stable per snapshot")
	}
	// A distinct snapshot gets its own cache.
	g2 := dcFixture(t)
	if DefaultCacheFor(g2) == c {
		t.Fatal("distinct snapshots share a default cache")
	}
	// Uncached path still answers identically.
	if count(func() (*Rows, error) { return RunUncached(g, q) }) != 1 {
		t.Fatal("uncached run wrong rows")
	}
}
