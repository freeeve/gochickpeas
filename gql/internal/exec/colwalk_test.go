// Differential tests for the fused columnar walk-aggregate: every
// fusable chain shape must produce exactly the general pipeline's rows,
// pinned by running each query both ways via the disableColWalk knob --
// pair and single and keyless group keys, count(*) and count(var),
// mid-chain and last-level filters, parallel relationships (per-rel
// multiplicity), and the order-observability decline.
package exec

import (
	"fmt"
	"sort"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

func colWalkFixture(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(256, 512)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	day := int64(86_400_000)
	base := int64(1_264_723_200_000) // 2010-01-29
	forum := 0
	for c := 0; c < 3; c++ {
		cn, _ := b.AddNode("Country")
		must(b.SetProp(cn, "name", fmt.Sprintf("C%d", c)))
		for ci := 0; ci < 2; ci++ {
			cin, _ := b.AddNode("City")
			must(b.SetProp(cin, "pop", int64(ci*7+c)))
			if _, err := b.AddRel(cin, cn, "IS_PART_OF"); err != nil {
				t.Fatal(err)
			}
			for p := 0; p < 3; p++ {
				pn, _ := b.AddNode("Person")
				if _, err := b.AddRel(pn, cin, "IS_LOCATED_IN"); err != nil {
					t.Fatal(err)
				}
				for f := 0; f < (c+ci+p)%4; f++ {
					fn, _ := b.AddNode("Forum")
					must(b.SetProp(fn, "name", fmt.Sprintf("F%d", forum)))
					forum++
					// Dates straddle the filter cutoff.
					must(b.SetProp(fn, "creationDate", base+int64(f*40-30)*day))
					if _, err := b.AddRel(fn, pn, "HAS_MEMBER"); err != nil {
						t.Fatal(err)
					}
					if f == 1 {
						// A parallel membership: counting is per relationship.
						if _, err := b.AddRel(fn, pn, "HAS_MEMBER"); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
		}
	}
	return b.Finalize("colwalk")
}

// runBothWalk runs q fused and general, returning both row sets rendered.
func runBothWalk(t *testing.T, g *chickpeas.Snapshot, q string) (fused, general []string) {
	t.Helper()
	run := func() []string {
		t.Helper()
		qq, err := parser.Parse(q)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		p, err := plan.Build(qq, graph.New(g))
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		ctx := &eval.Ctx{G: graph.New(g)}
		rows, err := Execute(ctx, p)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, fmt.Sprint(r))
		}
		sort.Strings(out)
		return out
	}
	disableColWalk = false
	fused = run()
	disableColWalk = true
	general = run()
	disableColWalk = false
	return fused, general
}

func TestColumnarWalkAggMatchesGeneral(t *testing.T) {
	g := colWalkFixture(t)
	queries := []string{
		// The Q4 shape: pair entity keys, last-level date filter.
		`MATCH (c:Country)<-[:IS_PART_OF]-(ci)<-[:IS_LOCATED_IN]-(p)<-[:HAS_MEMBER]-(f)
		 WHERE f.creationDate > zoned_datetime('2010-01-29')
		 RETURN c, f, count(p) AS n
		 NEXT RETURN c.name AS cn, f.name AS fn, n ORDER BY n DESC, fn, cn`,
		// Single entity key, count(*), no filter.
		`MATCH (c:Country)<-[:IS_PART_OF]-(ci)<-[:IS_LOCATED_IN]-(p)
		 RETURN c, count(*) AS n
		 NEXT RETURN c.name AS cn, n ORDER BY n DESC, cn`,
		// Keyless over the whole walk.
		`MATCH (c:Country)<-[:IS_PART_OF]-(ci)<-[:IS_LOCATED_IN]-(p)<-[:HAS_MEMBER]-(f)
		 RETURN count(f) AS n
		 NEXT RETURN n ORDER BY n`,
		// Mid-chain filter (city property) plus anchor-key grouping.
		`MATCH (c:Country)<-[:IS_PART_OF]-(ci)<-[:IS_LOCATED_IN]-(p)
		 WHERE ci.pop > 2
		 RETURN c, count(p) AS n
		 NEXT RETURN c.name AS cn, n ORDER BY n, cn`,
		// ORDER BY on the aggregated boundary itself, with LIMIT.
		`MATCH (c:Country)<-[:IS_PART_OF]-(ci)<-[:IS_LOCATED_IN]-(p)<-[:HAS_MEMBER]-(f)
		 RETURN c, f, count(p) AS n ORDER BY n DESC, c.name, f.name LIMIT 5
		 NEXT RETURN c.name AS cn, f.name AS fn, n ORDER BY n DESC, fn, cn`,
		// Labeled intermediate hop targets.
		`MATCH (c:Country)<-[:IS_PART_OF]-(ci:City)<-[:IS_LOCATED_IN]-(p:Person)
		 RETURN c, count(p) AS n
		 NEXT RETURN c.name AS cn, n ORDER BY n, cn`,
	}
	for i, q := range queries {
		before := colWalkFired
		fused, general := runBothWalk(t, g, q)
		if fmt.Sprint(fused) != fmt.Sprint(general) {
			t.Errorf("query %d diverged:\nfused:   %v\ngeneral: %v", i, fused, general)
		}
		if colWalkFired == before {
			t.Errorf("query %d did not take the fused path", i)
		}
	}
}

// TestColumnarWalkAggDeclines pins shapes that must fall back: no
// downstream ORDER BY (group encounter order observable), a DISTINCT
// aggregate, and a non-count aggregate.
func TestColumnarWalkAggDeclines(t *testing.T) {
	g := colWalkFixture(t)
	queries := []string{
		`MATCH (c:Country)<-[:IS_PART_OF]-(ci)<-[:IS_LOCATED_IN]-(p)
		 RETURN c, count(p) AS n`,
		`MATCH (c:Country)<-[:IS_PART_OF]-(ci)<-[:IS_LOCATED_IN]-(p)
		 RETURN c, count(DISTINCT p) AS n
		 NEXT RETURN c.name AS cn, n ORDER BY n, cn`,
		`MATCH (c:Country)<-[:IS_PART_OF]-(ci)<-[:IS_LOCATED_IN]-(p)
		 RETURN c, sum(ci.pop) AS s
		 NEXT RETURN c.name AS cn, s ORDER BY s, cn`,
	}
	for i, q := range queries {
		before := colWalkFired
		fused, general := runBothWalk(t, g, q)
		if fmt.Sprint(fused) != fmt.Sprint(general) {
			t.Errorf("query %d diverged:\nfused:   %v\ngeneral: %v", i, fused, general)
		}
		if colWalkFired != before {
			t.Errorf("query %d unexpectedly fused", i)
		}
	}
}
