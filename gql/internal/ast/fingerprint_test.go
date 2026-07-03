// Fingerprint tests: structurally different queries must render
// differently (a collision would make the plan cache reuse the wrong
// plan); identical structure must render identically.
package ast_test

import (
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
)

func fp(t *testing.T, src string) string {
	t.Helper()
	q, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return ast.Fingerprint(q)
}

func TestFingerprintDistinguishesStructure(t *testing.T) {
	// Every pair below is structurally distinct and must fingerprint
	// differently; the list sweeps all clause kinds and the expression
	// surface.
	queries := []string{
		"MATCH (p:Person) RETURN p.name AS n",
		"MATCH (p:Person) RETURN p.name AS m",
		"MATCH (p:People) RETURN p.name AS n",
		"MATCH (q:Person) RETURN q.name AS n",
		"MATCH (p:Person {age: 30}) RETURN p.name AS n",
		"MATCH (p:Person) WHERE p.age > 30 RETURN p.name AS n",
		"MATCH (p:Person) WHERE p.age >= 30 RETURN p.name AS n",
		"MATCH (p:Person) WHERE NOT p.age > 30 RETURN p.name AS n",
		"MATCH (p:Person) RETURN DISTINCT p.name AS n",
		"MATCH (p:Person) RETURN p.name AS n ORDER BY n",
		"MATCH (p:Person) RETURN p.name AS n ORDER BY n DESC",
		"MATCH (p:Person) RETURN p.name AS n LIMIT 2",
		"MATCH (p:Person) RETURN p.name AS n LIMIT 3",
		"MATCH (p:Person) RETURN p.name AS n OFFSET 1",
		"MATCH (p:Person) RETURN *",
		"OPTIONAL MATCH (p:Person) RETURN p.name AS n",
		"MATCH (a)-[:KNOWS]->(b) RETURN b.name AS n",
		"MATCH (a)-[:KNOWS]-(b) RETURN b.name AS n",
		"MATCH (a)<-[:KNOWS]-(b) RETURN b.name AS n",
		"MATCH (a)-[:LIKES]->(b) RETURN b.name AS n",
		"MATCH (a)-[r:KNOWS]->(b) RETURN b.name AS n",
		"MATCH (a)-[:KNOWS]->{1,2}(b) RETURN b.name AS n",
		"MATCH (a)-[:KNOWS]->{1,3}(b) RETURN b.name AS n",
		"MATCH (a)-[:KNOWS]->*(b) RETURN b.name AS n",
		"MATCH (a)-[:KNOWS]->+(b) RETURN b.name AS n",
		"MATCH (a:A&B) RETURN a AS a",
		"MATCH (a:A|B) RETURN a AS a",
		"MATCH (a:!A) RETURN a AS a",
		"MATCH (a), (b) MATCH p = ANY SHORTEST (a)-[:KNOWS]->{1,}(b) RETURN length(p) AS l",
		"MATCH (a), (b) MATCH p = ALL SHORTEST (a)-[:KNOWS]->{1,}(b) RETURN length(p) AS l",
		"MATCH p = (a)-[:KNOWS]->(b) RETURN length(p) AS l",
		"CALL wcc('KNOWS') YIELD node, component RETURN component AS c",
		"CALL wcc('LIKES') YIELD node, component RETURN component AS c",
		"CALL algo.pagerank() YIELD node, value RETURN value AS v",
		"FOR x IN [1, 2] RETURN x AS x",
		"FOR y IN [1, 2] RETURN y AS y",
		"MATCH (p:Person) CALL (p) { MATCH (p)-[:KNOWS]->(f) RETURN count(f) AS c } RETURN c AS c",
		"MATCH (p:Person) CALL { MATCH (f:Person) RETURN count(f) AS c } RETURN c AS c",
		"MATCH (p:Person) RETURN count(*) AS n",
		"MATCH (p:Person) RETURN count(p) AS n",
		"MATCH (p:Person) RETURN count(DISTINCT p) AS n",
		"MATCH (p:Person) RETURN p.name AS n NEXT FILTER n = 'x' RETURN n",
		"MATCH (p:Person) LET a = p.age RETURN a AS a",
		"MATCH (p:Person) RETURN p.name AS n UNION MATCH (c:Co) RETURN c.name AS n",
		"MATCH (p:Person) RETURN p.name AS n UNION ALL MATCH (c:Co) RETURN c.name AS n",
		"MATCH (p) WHERE EXISTS { MATCH (p)-[:KNOWS]->(q) } RETURN p AS p",
		"MATCH (p) WHERE COUNT { MATCH (p)-[:KNOWS]->(q) } > 1 RETURN p AS p",
		"MATCH (p) WHERE p.name IN ['a', 'b'] RETURN p AS p",
		"MATCH (p) WHERE p.name IS NULL RETURN p AS p",
		"MATCH (p) WHERE p.name IS NOT NULL RETURN p AS p",
		"MATCH (p) WHERE p:Extra RETURN p AS p",
		"MATCH (p) RETURN CASE WHEN p.a > 1 THEN 'x' ELSE 'y' END AS c",
		"MATCH (p) RETURN CASE p.a WHEN 1 THEN 'x' END AS c",
		"MATCH (p) RETURN {a: p.x, b: 2} AS m",
		"MATCH (p) RETURN [p.a, p.b][0] AS i",
		"MATCH (p) RETURN [p.a, p.b][0..1] AS s",
		"MATCH (p) RETURN all(x IN p.xs WHERE x > 0) AS q",
		"MATCH (p) RETURN any(x IN p.xs WHERE x > 0) AS q",
		"MATCH (p) RETURN -p.a + 2 * p.b AS e",
		"MATCH (p) RETURN p.name STARTS WITH 'A' AS e",
		"MATCH (p) RETURN p.name ENDS WITH 'A' AS e",
		"MATCH (p:Person {name: $who}) RETURN p.age AS a",
		"EXPLAIN MATCH (p:Person) RETURN p.name AS n",
		"PROFILE MATCH (p:Person) RETURN p.name AS n",
	}
	seen := map[string]string{}
	for _, q := range queries {
		f := fp(t, q)
		if prev, dup := seen[f]; dup {
			t.Fatalf("fingerprint collision:\n  %s\n  %s\n  -> %s", prev, q, f)
		}
		seen[f] = q
	}
}

func TestFingerprintStableAcrossParses(t *testing.T) {
	for _, q := range []string{
		"MATCH (p:Person {name: 'Alice'})-[:KNOWS]->{1,2}(f) WHERE f.age > 30 RETURN DISTINCT f.name AS n ORDER BY n LIMIT 5",
		"MATCH (p:Person) CALL (p) { MATCH (p)-[:KNOWS]->(f) RETURN count(f) AS c } RETURN c AS c",
	} {
		// Whitespace-insensitive: the fingerprint reflects structure, not
		// the raw text.
		spaced := "  " + q + "  "
		if fp(t, q) != fp(t, spaced) {
			t.Fatalf("whitespace changed the fingerprint: %s", q)
		}
		if fp(t, q) != fp(t, q) {
			t.Fatalf("unstable fingerprint: %s", q)
		}
	}
}
