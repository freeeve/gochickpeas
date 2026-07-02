// Fuzz target: the parser must never panic -- every input returns an AST
// or a typed *Error.
package parser

import "testing"

func FuzzParseGQL(f *testing.F) {
	for _, seed := range []string{
		"MATCH (p:Person)-[:KNOWS]->(f:Person) WHERE p.age > 30 RETURN f.name AS name, count(*) AS c ORDER BY c DESC LIMIT 10",
		"MATCH (a)-[:KNOWS]->{1,3}(b) RETURN b",
		"MATCH (a), (b) MATCH p = ANY SHORTEST (a)-[:KNOWS]-{1,4}(b) RETURN length(p)",
		"MATCH (p:Person) LET a = p.age + 1 FILTER a > 2 RETURN a",
		"MATCH (p) RETURN p, count(*) AS c NEXT FILTER c > 1 RETURN p",
		"FOR x IN [1, 2, 3] RETURN x AS n UNION ALL FOR y IN [4] RETURN y AS n",
		"MATCH (p:Person) CALL (p) { MATCH (p)-[:KNOWS]->(f) RETURN count(f) AS c } RETURN c",
		"CALL fts.search('Person', 'name', 'alice') YIELD node RETURN node",
		"MATCH (n:Dog|Cat {name: 'Rex', age: $age}) WHERE n.x IS NOT NULL RETURN DISTINCT n OFFSET 1 LIMIT 2",
		"MATCH (m) WHERE m:Comment AND EXISTS { MATCH (m)-[:REPLY_OF]->(x) } RETURN CASE WHEN m.x THEN 1 ELSE 2 END",
		"RETURN {a: 1, b: [1.5, 'x', true, null]} AS m",
		"MATCH (a) WHERE all(x IN a.xs WHERE x > 0) AND a.name STARTS WITH 'A' RETURN a.xs[1..2]",
		"EXPLAIN MATCH (a) RETURN a",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, src string) {
		q, err := Parse(src)
		if (q == nil) == (err == nil) {
			t.Fatalf("exactly one of AST/error: q=%v err=%v", q, err)
		}
	})
}
