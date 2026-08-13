package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
)

// semiredFixture: India(0), Zed(1); cities c1(2), c2(3) in India, c3(4)
// in Zed; persons p1(5)@c1, p2(6)@c2, p3(7)@c3, p4(8)@{c1,c2} -- p4's two
// India cities make chain multiplicity observable; KNOWS p1-p2, p1-p3,
// p1-p4.
func semiredFixture(t *testing.T) graph.Graph {
	t.Helper()
	bld := chickpeas.NewBuilder(16, 32)
	add := func(label, name string) chickpeas.NodeID {
		t.Helper()
		id, err := bld.AddNode(label)
		if err != nil {
			t.Fatal(err)
		}
		if name != "" {
			if err := bld.SetProp(id, "name", name); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	india := add("Country", "India")
	zed := add("Country", "Zed")
	c1, c2, c3 := add("City", ""), add("City", ""), add("City", "")
	p1, p2, p3, p4 := add("Person", ""), add("Person", ""), add("Person", ""), add("Person", "")
	rel := func(a, b chickpeas.NodeID, ty string) {
		t.Helper()
		if _, err := bld.AddRel(a, b, ty); err != nil {
			t.Fatal(err)
		}
	}
	rel(c1, india, "PART")
	rel(c2, india, "PART")
	rel(c3, zed, "PART")
	rel(p1, c1, "LOC")
	rel(p2, c2, "LOC")
	rel(p3, c3, "LOC")
	rel(p4, c1, "LOC")
	rel(p4, c2, "LOC")
	rel(p1, p2, "KNOWS")
	rel(p1, p3, "KNOWS")
	rel(p1, p4, "KNOWS")
	return graph.New(bld.Finalize())
}

// TestConstChainAbsorb pins the semijoin reduction (task 205 round 8):
// results must match the un-absorbed evaluation exactly, and the
// engagement counter must climb on the carried bound-bound chain shape.
func TestConstChainAbsorb(t *testing.T) {
	g := semiredFixture(t)
	// The chain sits alone in a later segment with b and x both carried
	// -- the Q11 seam: the planner anchors the carried fan side and
	// into-bounds the constant, and the absorber replaces the per-row
	// walk with one membership set.
	q := "MATCH (x:Country {name: 'India'}) RETURN x NEXT " +
		"MATCH (a:Person)-[:KNOWS]-(b:Person) RETURN DISTINCT a, b, x NEXT " +
		"MATCH (b)-[:LOC]->(:City)-[:PART]->(x) RETURN DISTINCT a, b"
	before := constChainAbsorbs
	got := runQuery(t, g, q)
	if constChainAbsorbs == before {
		t.Fatal("the absorber did not fire on the qualifying shape")
	}
	if got != 5 {
		t.Fatalf("absorbed rows = %d, want 5", got)
	}
	disableConstChainAbsorb = true
	defer func() { disableConstChainAbsorb = false }()
	if general := runQuery(t, g, q); general != got {
		t.Fatalf("general evaluation kept %d rows, absorbed kept %d", general, got)
	}
}

// TestConstChainAbsorbCommaIdentity pins result identity on the comma
// form, where the planner is free to reorder the chain ahead of the
// KNOWS pattern (generating b from the constant) -- whichever plan it
// picks, absorbed and general evaluation must agree.
func TestConstChainAbsorbCommaIdentity(t *testing.T) {
	g := semiredFixture(t)
	q := "MATCH (x:Country {name: 'India'}) RETURN x NEXT " +
		"MATCH (a:Person)-[:KNOWS]-(b:Person), (b)-[:LOC]->(:City)-[:PART]->(x) RETURN DISTINCT a, b"
	got := runQuery(t, g, q)
	disableConstChainAbsorb = true
	defer func() { disableConstChainAbsorb = false }()
	if general := runQuery(t, g, q); general != got || got != 5 {
		t.Fatalf("comma form: absorbed=%d general=%d, want 5=5", got, general)
	}
}

// TestConstChainAbsorbRefusals pins the fail-closed gates with LIVE
// counterexamples: a non-DISTINCT boundary must keep the chain's match
// multiplicity (p4's two India cities emit two rows), and a NAMED
// intermediate must keep its enumeration.
func TestConstChainAbsorbRefusals(t *testing.T) {
	g := semiredFixture(t)
	cases := []struct {
		name, q string
		rows    int
	}{
		{"multiplicity", "MATCH (x:Country {name: 'India'}) RETURN x NEXT " +
			"MATCH (a:Person)-[:KNOWS]-(b:Person), (b)-[:LOC]->(:City)-[:PART]->(x) RETURN a, b", 6},
		{"named intermediate", "MATCH (x:Country {name: 'India'}) RETURN x NEXT " +
			"MATCH (a:Person)-[:KNOWS]-(b:Person), (b)-[:LOC]->(ci:City)-[:PART]->(x) RETURN DISTINCT a, b", 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := constChainAbsorbs
			if got := runQuery(t, g, tc.q); got != tc.rows {
				t.Fatalf("%d rows, want %d", got, tc.rows)
			}
			if constChainAbsorbs != before {
				t.Fatal("the absorber fired on a shape its gates must refuse")
			}
		})
	}
}
