package chickpeas_test

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// TestAvgDegreeByLabel pins the label-conditional degree statistic against
// a hand-countable graph: two Persons and one City, one KNOWS between the
// persons, both persons LIVES_IN the city. Every expectation is an exact
// count over the label's full membership (zero-degree members in the
// denominator), and the global AvgDegree over the same type demonstrably
// disagrees with the conditional view where the labels differ.
func TestAvgDegreeByLabel(t *testing.T) {
	b := chickpeas.NewBuilder(8, 8)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	p1, err := b.AddNode("Person")
	must(err)
	p2, err := b.AddNode("Person")
	must(err)
	city, err := b.AddNode("City")
	must(err)
	_, err = b.AddRel(p1, p2, "KNOWS")
	must(err)
	_, err = b.AddRel(p1, city, "LIVES_IN")
	must(err)
	_, err = b.AddRel(p2, city, "LIVES_IN")
	must(err)
	g := b.Finalize("labelstats")

	check := func(label, relType string, dir chickpeas.Direction, want float64) {
		t.Helper()
		got, ok := g.AvgDegreeByLabel(label, relType, dir)
		if !ok {
			t.Fatalf("AvgDegreeByLabel(%s, %s, %v) not ok, want %v", label, relType, dir, want)
		}
		if got != want {
			t.Fatalf("AvgDegreeByLabel(%s, %s, %v) = %v, want %v", label, relType, dir, got, want)
		}
	}
	// One KNOWS over two persons, each direction.
	check("Person", "KNOWS", chickpeas.Outgoing, 0.5)
	check("Person", "KNOWS", chickpeas.Incoming, 0.5)
	check("Person", "KNOWS", chickpeas.Both, 1.0)
	// Two LIVES_IN leave persons, none arrive.
	check("Person", "LIVES_IN", chickpeas.Outgoing, 1.0)
	check("Person", "LIVES_IN", chickpeas.Incoming, 0.0)
	// The single city receives both -- the conditional fan-out the global
	// statistic cannot express (AvgDegree averages over the type's own
	// endpoint sets, not over a label).
	check("City", "LIVES_IN", chickpeas.Incoming, 2.0)
	check("City", "LIVES_IN", chickpeas.Outgoing, 0.0)
	// Known label, type it never touches: a real zero, not a missing stat.
	check("City", "KNOWS", chickpeas.Both, 0.0)
	// Known label, absent type: also (0, true).
	check("Person", "NOPE", chickpeas.Outgoing, 0.0)
	// Unknown label abstains so the caller falls back to global stats.
	if _, ok := g.AvgDegreeByLabel("Ghost", "KNOWS", chickpeas.Outgoing); ok {
		t.Fatal("unknown label should not report a conditional degree")
	}
}
