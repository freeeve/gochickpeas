// EXPLAIN-rendering goldens: the renderer is the planner's testable
// surface, so these assert operator lines, estimates, and anchor notes.
package explain

import (
	"strings"
	"testing"
	"time"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// fixture builds the same skewed graph the plan tests use.
func fixture(t *testing.T) graph.Graph {
	t.Helper()
	b := chickpeas.NewBuilder(80, 200)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var persons, messages, tags []chickpeas.NodeID
	for i := range 40 {
		p, err := b.AddNode("Person")
		must(err)
		must(b.SetProp(p, "pid", int64(i)))
		persons = append(persons, p)
	}
	for i := range 20 {
		m, err := b.AddNode("Message")
		must(err)
		must(b.SetProp(m, "len", int64(i*10)))
		messages = append(messages, m)
	}
	for i := range 4 {
		tg, err := b.AddNode("Tag")
		must(err)
		must(b.SetProp(tg, "name", "tag"+string(rune('A'+i))))
		tags = append(tags, tg)
	}
	for i, m := range messages {
		_, err := b.AddRel(m, persons[i%len(persons)], "HAS_CREATOR")
		must(err)
		_, err = b.AddRel(m, tags[i%len(tags)], "HAS_TAG")
		must(err)
	}
	for i := range persons {
		_, err := b.AddRel(persons[i], persons[(i+1)%len(persons)], "KNOWS")
		must(err)
	}
	return graph.New(b.Finalize())
}

// explain parses, plans, estimates, renders.
func explainQuery(t *testing.T, g graph.Graph, src string) string {
	t.Helper()
	q, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p, err := plan.Build(q, g)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	lines := Render(p, nil, 1500*time.Microsecond, plan.Estimate(p, g))
	return strings.Join(lines, "\n")
}

func wantContains(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Fatalf("missing %q in:\n%s", w, text)
		}
	}
}

func TestRenderSeekExpandFilterProject(t *testing.T) {
	g := fixture(t)
	out := explainQuery(t, g, "MATCH (m:Message)-[:HAS_TAG]->(tg:Tag {name: 'tagA'}) WHERE m.len > 20 RETURN m.len AS len ORDER BY len OFFSET 1 LIMIT 5")
	wantContains(t, out,
		"EXPLAIN",
		"Planning:",
		"NodeByProperty (tg:Tag {name = 'tagA'})",
		"Expand (tg)<-[:HAS_TAG]-(m)",
		"Filter (m.len > 20)",
		"Project [len]",
		"OrderBy [len]",
		"Offset 1",
		"Limit 5",
		"est", // estimates always render
	)
}

func TestRenderAnchorNote(t *testing.T) {
	g := fixture(t)
	out := explainQuery(t, g, "MATCH (m:Message)-[:HAS_TAG]->(tg:Tag {name: 'tagA'}) RETURN m.len")
	wantContains(t, out, "[anchor:", "card=", "fan-out", "smaller leaf cardinality")
}

func TestRenderAggregateAndUnion(t *testing.T) {
	g := fixture(t)
	out := explainQuery(t, g, "MATCH (m:Message)-[:HAS_TAG]->(tg:Tag) RETURN tg.name AS name, count(m) AS n UNION ALL MATCH (tg:Tag) RETURN tg.name AS name, 0 AS n")
	wantContains(t, out,
		"Branch 0",
		"UNION ALL",
		"Branch 1",
		"Aggregate (group=[name]; count(m))",
	)
}

func TestRenderVarExpandMonoAndSeek(t *testing.T) {
	g := fixture(t)
	out := explainQuery(t, g, "MATCH (a:Person)-[e:KNOWS]->{1,3}(b:Person) WHERE id(a) = 3 AND all(i IN range(0, size(rels(e)) - 2) WHERE rels(e)[i].ts > rels(e)[i+1].ts) RETURN b.pid")
	wantContains(t, out,
		"NodeBySeek (a = id 3)",
		"VarExpand (a)-[:KNOWS*1..3]->(b) [mono ts desc]",
	)
}

func TestRenderCallAndSubqueryAndFor(t *testing.T) {
	g := fixture(t)
	out := explainQuery(t, g, "CALL wcc('KNOWS') YIELD node, component RETURN node, component")
	wantContains(t, out, "Call wcc('KNOWS')")

	out = explainQuery(t, g, "MATCH (p:Person) CALL (p) { MATCH (p)-[:KNOWS]->(q) RETURN q.pid AS qp } RETURN p.pid, qp")
	wantContains(t, out, "CallSubquery [qp]", "Expand")

	out = explainQuery(t, g, "FOR x IN [1, 2, 3] RETURN x * 2 AS y")
	wantContains(t, out, "Unwind ([1, 2, 3] AS x)")
}

func TestRenderShortestAndTextIndex(t *testing.T) {
	g := fixture(t)
	out := explainQuery(t, g, "MATCH (a:Person {pid: 1}) MATCH (b:Person {pid: 5}) MATCH pth = ANY SHORTEST (a)-[:KNOWS]->{1,6}(b) RETURN pth")
	wantContains(t, out, "ShortestPath (a)-[*]-(b)")

	out = explainQuery(t, g, "MATCH (tg:Tag) WHERE tg.name CONTAINS 'ag' RETURN tg")
	wantContains(t, out, "NodeByTextIndex (tg:Tag {name CONTAINS 'ag'})")
}

func TestRenderProfileZipSeam(t *testing.T) {
	g := fixture(t)
	q, err := parser.Parse("MATCH (tg:Tag {name: 'tagA'}) RETURN tg.name")
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(q, g)
	if err != nil {
		t.Fatal(err)
	}
	one := uint64(1)
	prof := &Profile{Segs: []SegProf{{Stages: []StageProf{{Match: []uint64{1}}}, ProjRows: one}}}
	lines := Render(p, prof, time.Millisecond, plan.Estimate(p, g))
	text := strings.Join(lines, "\n")
	wantContains(t, text, "PROFILE", "est", "rows")
}
