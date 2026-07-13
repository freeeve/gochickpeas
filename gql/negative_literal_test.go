// Negative numeric literals fold at parse time (rcp b6a17c8), so they take
// the same constant-matching paths as their positive twins: a negative
// inline property plans a seek (not a label scan + post-filter), and queries
// differing only in a negative WHERE bound collapse to one cached template.
package gql

import (
	"slices"
	"strings"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// acctGraph builds Acct nodes carrying signed balances, with balance indexed
// so a concrete inline prop can seek.
func acctGraph(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(8, 1)
	for _, bal := range []int64{-50, -5, 0, 5, 50} {
		id, _ := b.AddNode("Acct")
		if err := b.SetProp(id, "balance", bal); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize("balance")
}

func TestNegativeInlinePropPlansSeek(t *testing.T) {
	g := acctGraph(t)
	// A negative inline prop plans a property seek, exactly like the positive
	// twin -- no label scan + desugared filter.
	for _, q := range []string{
		"MATCH (a:Acct {balance: -50}) RETURN a.balance AS b",
		"MATCH (a:Acct {balance: 50}) RETURN a.balance AS b",
	} {
		plan, err := Explain(g, q)
		if err != nil {
			t.Fatalf("explain %q: %v", q, err)
		}
		if !strings.Contains(plan, "NodeByProperty") {
			t.Fatalf("%q did not plan a property seek:\n%s", q, plan)
		}
		if strings.Contains(plan, "NodeScan") {
			t.Fatalf("%q fell back to a label scan:\n%s", q, plan)
		}
	}
	// Result identity: the negative seek finds its one row.
	if got := intCol(t, g, "MATCH (a:Acct {balance: -50}) RETURN a.balance AS b", "b"); !slices.Equal(got, []int64{-50}) {
		t.Fatalf("negative seek rows = %v", got)
	}
}

func TestNegativeWhereBoundSharesPlan(t *testing.T) {
	g := acctGraph(t)
	c := NewPlanCache(0)
	bal := func(q string) []int64 { return cachedInts(t, c, g, q, "b") }
	// Three queries differing only in a negative WHERE bound: each resolves
	// its own lifted constant, all collapse to one cached template.
	if got := bal("MATCH (a:Acct) WHERE a.balance = -50 RETURN a.balance AS b"); !slices.Equal(got, []int64{-50}) {
		t.Fatalf("=-50 => %v", got)
	}
	if got := bal("MATCH (a:Acct) WHERE a.balance = -5 RETURN a.balance AS b"); !slices.Equal(got, []int64{-5}) {
		t.Fatalf("=-5 => %v", got)
	}
	if got := bal("MATCH (a:Acct) WHERE a.balance = 0 RETURN a.balance AS b"); !slices.Equal(got, []int64{0}) {
		t.Fatalf("=0 => %v", got)
	}
	if c.Len() != 1 {
		t.Fatalf("three negative/zero bounds should share one plan, Len = %d", c.Len())
	}
}
