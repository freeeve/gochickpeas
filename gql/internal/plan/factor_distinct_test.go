// Marking matrix for distinct-aggregate factorization: the qualifying
// shape (an OPTIONAL suffix chain off one bound variable, interior hops
// functional toward it, count(DISTINCT lastRel) the only aggregate)
// rewrites, and every disqualifying feature refuses -- pinned at the
// plan so the exec identity test cannot pass vacuously.
package plan

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// factorQ is the fixture's Q6 analog: HAS_TAG is functional from
// Message (each message tags exactly one tag), so the suffix partitions
// by tg and count(DISTINCT c) factors into per-tg creator-edge counts.
const factorQ = "MATCH (m1:Message)-[:HAS_CREATOR]->(p1:Person) OPTIONAL MATCH (m1)-[:HAS_TAG]->(tg:Tag) OPTIONAL MATCH (tg)<-[:HAS_TAG]-(m2:Message)-[c:HAS_CREATOR]->(p3:Person) RETURN p1.pid AS pid, count(DISTINCT c) AS score ORDER BY score DESC, pid ASC"

func TestFactorDistinctRewrites(t *testing.T) {
	g := buildFixture(t)
	before := factorDistinctFired
	p := mustPlan(t, g, factorQ)
	if factorDistinctFired != before+1 {
		t.Fatalf("factorDistinctFired moved %d, want 1", factorDistinctFired-before)
	}
	segs := p.Branches[0]
	if len(segs) < 3 {
		t.Fatalf("factored plan has %d segments, want >= 3 (distinct phase, count phase, final aggregate)", len(segs))
	}
	// The final projection aggregates a sum (of the per-partition
	// counts), not a distinct count.
	last := segs[len(segs)-1].Proj
	if !last.Aggregated || len(last.Aggs) != 1 || last.Aggs[0].Kind != AggSum || last.Aggs[0].Distinct {
		t.Fatalf("final projection aggregates %+v, want a single plain sum", last.Aggs)
	}
}

func TestFactorDistinctDeclines(t *testing.T) {
	g := buildFixture(t)
	for _, tc := range []struct {
		name, src string
	}{
		{"non-optional-suffix",
			"MATCH (m1:Message)-[:HAS_CREATOR]->(p1:Person) OPTIONAL MATCH (m1)-[:HAS_TAG]->(tg:Tag) MATCH (tg)<-[:HAS_TAG]-(m2:Message)-[c:HAS_CREATOR]->(p3:Person) RETURN p1.pid AS pid, count(DISTINCT c) AS score ORDER BY score DESC, pid ASC"},
		{"suffix-where",
			"MATCH (m1:Message)-[:HAS_CREATOR]->(p1:Person) OPTIONAL MATCH (m1)-[:HAS_TAG]->(tg:Tag) OPTIONAL MATCH (tg)<-[:HAS_TAG]-(m2:Message)-[c:HAS_CREATOR]->(p3:Person) WHERE m2.len > 0 RETURN p1.pid AS pid, count(DISTINCT c) AS score ORDER BY score DESC, pid ASC"},
		{"distinct-arg-not-last-rel",
			"MATCH (m1:Message)-[:HAS_CREATOR]->(p1:Person) OPTIONAL MATCH (m1)-[:HAS_TAG]->(tg:Tag) OPTIONAL MATCH (tg)<-[ht:HAS_TAG]-(m2:Message)-[c:HAS_CREATOR]->(p3:Person) RETURN p1.pid AS pid, count(DISTINCT ht) AS score ORDER BY score DESC, pid ASC"},
		{"non-distinct-count",
			"MATCH (m1:Message)-[:HAS_CREATOR]->(p1:Person) OPTIONAL MATCH (m1)-[:HAS_TAG]->(tg:Tag) OPTIONAL MATCH (tg)<-[:HAS_TAG]-(m2:Message)-[c:HAS_CREATOR]->(p3:Person) RETURN p1.pid AS pid, count(c) AS score ORDER BY score DESC, pid ASC"},
		{"second-aggregate",
			"MATCH (m1:Message)-[:HAS_CREATOR]->(p1:Person) OPTIONAL MATCH (m1)-[:HAS_TAG]->(tg:Tag) OPTIONAL MATCH (tg)<-[:HAS_TAG]-(m2:Message)-[c:HAS_CREATOR]->(p3:Person) RETURN p1.pid AS pid, count(DISTINCT c) AS score, count(m2) AS extra ORDER BY score DESC, pid ASC"},
		{"suffix-var-in-group-key",
			"MATCH (m1:Message)-[:HAS_CREATOR]->(p1:Person) OPTIONAL MATCH (m1)-[:HAS_TAG]->(tg:Tag) OPTIONAL MATCH (tg)<-[:HAS_TAG]-(m2:Message)-[c:HAS_CREATOR]->(p3:Person) RETURN p3.pid AS pid, count(DISTINCT c) AS score ORDER BY score DESC, pid ASC"},
		{"undirected-hop",
			"MATCH (m1:Message)-[:HAS_CREATOR]->(p1:Person) OPTIONAL MATCH (m1)-[:HAS_TAG]->(tg:Tag) OPTIONAL MATCH (tg)-[:HAS_TAG]-(m2:Message)-[c:HAS_CREATOR]->(p3:Person) RETURN p1.pid AS pid, count(DISTINCT c) AS score ORDER BY score DESC, pid ASC"},
		{"var-length-interior-hop",
			"MATCH (p1:Person) OPTIONAL MATCH (p1)-[:KNOWS]->(p2:Person) OPTIONAL MATCH (p2)<-[:KNOWS]-{1,2}(px:Person)<-[k2:KNOWS]-(p3:Person) RETURN p1.pid AS pid, count(DISTINCT k2) AS score ORDER BY score DESC, pid ASC"},
		{"unbound-partition-var",
			"MATCH (m1:Message)-[:HAS_CREATOR]->(p1:Person) OPTIONAL MATCH (other)<-[:HAS_TAG]-(m2:Message)-[c:HAS_CREATOR]->(p3:Person) RETURN p1.pid AS pid, count(DISTINCT c) AS score ORDER BY score DESC, pid ASC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := factorDistinctFired
			mustPlan(t, g, tc.src)
			if factorDistinctFired != before {
				t.Fatalf("factorDistinctFired moved on %s", tc.name)
			}
		})
	}
}

// A genuinely non-functional interior hop must decline: F carries TWO
// T-edges out (to X1 and X2), so a distinct final-hop edge under F
// would be summed under both partition values.
func TestFactorDistinctDeclinesNonFunctionalHop(t *testing.T) {
	b := chickpeas.NewBuilder(16, 16)
	x1, _ := b.AddNode("X")
	x2, _ := b.AddNode("X")
	f, _ := b.AddNode("F")
	z, _ := b.AddNode("Z")
	if _, err := b.AddRel(f, x1, "T"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddRel(f, x2, "T"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddRel(f, z, "U"); err != nil {
		t.Fatal(err)
	}
	g := graphNew(b.Finalize("factor-nonfunc"))
	before := factorDistinctFired
	mustPlan(t, g, "MATCH (x:X) OPTIONAL MATCH (x)<-[:T]-(ff:F)-[u:U]->(zz:Z) RETURN x.nope AS xn, count(DISTINCT u) AS score ORDER BY score DESC, xn ASC")
	if factorDistinctFired != before {
		t.Fatal("rewrite fired across a non-functional interior hop")
	}
}

// The single-hop suffix needs no functionality at all: each edge
// determines its pv endpoint, so per-pv edge sets are disjoint by
// construction.
func TestFactorDistinctSingleHopSuffix(t *testing.T) {
	g := buildFixture(t)
	before := factorDistinctFired
	mustPlan(t, g, "MATCH (m1:Message)-[:HAS_TAG]->(tg:Tag) OPTIONAL MATCH (tg)<-[ht2:HAS_TAG]-(m2:Message) RETURN m1.len AS l, count(DISTINCT ht2) AS score ORDER BY score DESC, l ASC")
	if factorDistinctFired != before+1 {
		t.Fatalf("single-hop suffix did not rewrite (fired %d)", factorDistinctFired-before)
	}
}

func TestFactorDistinctDisableSwitch(t *testing.T) {
	g := buildFixture(t)
	DisableFactorDistinct = true
	defer func() { DisableFactorDistinct = false }()
	before := factorDistinctFired
	mustPlan(t, g, factorQ)
	if factorDistinctFired != before {
		t.Fatal("rewrite fired under DisableFactorDistinct")
	}
}
