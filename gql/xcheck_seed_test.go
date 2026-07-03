// The seed-corpus generator: runs a representative query set covering
// every feature family and writes testdata/xcheck/seed/seed.json in the
// documented record schema (expected rows produced by this engine -- a
// regression pin until the Rust exporter's corpus supersedes it). The
// cypher field carries the Rust engine's spelling for each query so the
// corpus is ready for true differential use.
package gql

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// seedQuery is one generator entry.
type seedQuery struct {
	graph     string
	gql       string
	cypher    string
	params    map[string]value.Value
	unordered bool
}

// seedQueries covers scans, predicates, expands, quantified paths,
// OPTIONAL, named paths, path search, aggregation, FOR, subqueries, CALL
// procedures, UNION, params, temporal, and CASE.
var seedQueries = []seedQuery{
	{graph: "social",
		gql:    "MATCH (p:Person) WHERE p.age > 30 RETURN p.name AS name ORDER BY name",
		cypher: "MATCH (p:Person) WHERE p.age > 30 RETURN p.name AS name ORDER BY name"},
	{graph: "social",
		gql:    "MATCH (p:Person {name: 'Alice'}) RETURN p.age AS age",
		cypher: "MATCH (p:Person {name: 'Alice'}) RETURN p.age AS age"},
	{graph: "social",
		gql:    "MATCH (p:Person) WHERE p.name ENDS WITH 'e' RETURN p.name AS name ORDER BY name",
		cypher: "MATCH (p:Person) WHERE p.name ENDS WITH 'e' RETURN p.name AS name ORDER BY name"},
	{graph: "social",
		gql:    "MATCH (p:Person) WHERE p.age IN [30.0, 35] RETURN p.name AS name ORDER BY name",
		cypher: "MATCH (p:Person) WHERE p.age IN [30.0, 35] RETURN p.name AS name ORDER BY name"},
	{graph: "social",
		gql:    "MATCH (p:Person) WHERE p.city IS NULL RETURN p.name AS name ORDER BY name",
		cypher: "MATCH (p:Person) WHERE p.city IS NULL RETURN p.name AS name ORDER BY name"},
	{graph: "social",
		gql:    "MATCH (p) WHERE id(p) = 2 RETURN p.name AS name",
		cypher: "MATCH (p) WHERE id(p) = 2 RETURN p.name AS name"},
	{graph: "social",
		gql:    "MATCH (p:Person) RETURN DISTINCT toString(p.joined / 10000) AS y ORDER BY y",
		cypher: "MATCH (p:Person) RETURN DISTINCT toString(p.joined / 10000) AS y ORDER BY y"},
	{graph: "social",
		gql:    "MATCH (p:Person) RETURN p.name AS name ORDER BY p.age DESC OFFSET 1 LIMIT 2",
		cypher: "MATCH (p:Person) RETURN p.name AS name ORDER BY p.age DESC SKIP 1 LIMIT 2"},
	{graph: "social",
		gql:    "MATCH (p:Person {name: 'Alice'})-[:KNOWS]->(f) RETURN f.name AS name ORDER BY name",
		cypher: "MATCH (p:Person {name: 'Alice'})-[:KNOWS]->(f) RETURN f.name AS name ORDER BY name"},
	{graph: "social",
		gql:    "MATCH (a:Person {name: 'Alice'})-[:KNOWS]->(f)-[:WORKS_AT]->(c) RETURN c.name AS name ORDER BY name",
		cypher: "MATCH (a:Person {name: 'Alice'})-[:KNOWS]->(f)-[:WORKS_AT]->(c) RETURN c.name AS name ORDER BY name"},
	{graph: "social",
		gql:    "MATCH (p:Person {name: 'Alice'})-[:KNOWS]->{1,2}(f:Person) RETURN DISTINCT f.name AS name ORDER BY name",
		cypher: "MATCH (p:Person {name: 'Alice'})-[:KNOWS*1..2]->(f:Person) RETURN DISTINCT f.name AS name ORDER BY name"},
	{graph: "social",
		gql:    "MATCH (a:Person {name: 'Alice'})-[:KNOWS]->+(b) RETURN DISTINCT b.name AS name ORDER BY name",
		cypher: "MATCH (a:Person {name: 'Alice'})-[:KNOWS*]->(b) RETURN DISTINCT b.name AS name ORDER BY name"},
	{graph: "social",
		gql:    "MATCH (p:Person) OPTIONAL MATCH (p)-[:WORKS_AT]->(c) RETURN p.name AS name, c.name AS company ORDER BY name",
		cypher: "MATCH (p:Person) OPTIONAL MATCH (p)-[:WORKS_AT]->(c) RETURN p.name AS name, c.name AS company ORDER BY name"},
	{graph: "social",
		gql:    "MATCH p = (a:Person {name: 'Alice'})-[:KNOWS]->(b) RETURN length(p) AS l, b.name AS name ORDER BY name",
		cypher: "MATCH p = (a:Person {name: 'Alice'})-[:KNOWS]->(b) RETURN length(p) AS l, b.name AS name ORDER BY name"},
	{graph: "social",
		gql:    "MATCH (a:Person {name: 'Alice'}), (b:Person {name: 'Dave'}) MATCH p = ANY SHORTEST (a)-[:KNOWS]->{1,}(b) RETURN length(p) AS l",
		cypher: "MATCH (a:Person {name: 'Alice'}), (b:Person {name: 'Dave'}) MATCH p = shortestPath((a)-[:KNOWS*]->(b)) RETURN length(p) AS l"},
	{graph: "social",
		gql:       "MATCH (a:Person {name: 'Alice'}), (b:Person {name: 'Dave'}) MATCH p = ALL SHORTEST (a)-[:KNOWS]->{1,}(b) RETURN length(p) AS l, size(nodes(p)) AS n",
		cypher:    "MATCH (a:Person {name: 'Alice'}), (b:Person {name: 'Dave'}) MATCH p = allShortestPaths((a)-[:KNOWS*]->(b)) RETURN length(p) AS l, size(nodes(p)) AS n",
		unordered: true},
	{graph: "social",
		gql:    "MATCH (c:Company)<-[:WORKS_AT]-(p:Person) RETURN c.name AS company, count(*) AS n ORDER BY n DESC",
		cypher: "MATCH (c:Company)<-[:WORKS_AT]-(p:Person) RETURN c.name AS company, count(*) AS n ORDER BY n DESC"},
	{graph: "social",
		gql:    "MATCH (p:Person) RETURN sum(p.age) AS s, avg(p.age) AS a, min(p.age) AS lo, max(p.age) AS hi",
		cypher: "MATCH (p:Person) RETURN sum(p.age) AS s, avg(p.age) AS a, min(p.age) AS lo, max(p.age) AS hi"},
	{graph: "social",
		gql:    "MATCH (p:Person) WHERE p.age > 30 RETURN collect(p.name) AS names NEXT FOR n IN names RETURN n AS n ORDER BY n",
		cypher: "MATCH (p:Person) WHERE p.age > 30 WITH collect(p.name) AS names UNWIND names AS n RETURN n AS n ORDER BY n"},
	{graph: "social",
		gql:    "MATCH (c:Company)<-[:WORKS_AT]-(p:Person) RETURN c.name AS name, count(*) AS n NEXT FILTER n > 1 RETURN name",
		cypher: "MATCH (c:Company)<-[:WORKS_AT]-(p:Person) WITH c.name AS name, count(*) AS n WHERE n > 1 RETURN name"},
	{graph: "social",
		gql:    "MATCH (p:Person) WHERE EXISTS { MATCH (p)-[:KNOWS]->(q:Person) WHERE q.age > 30 } RETURN p.name AS name ORDER BY name",
		cypher: "MATCH (p:Person) WHERE EXISTS { MATCH (p)-[:KNOWS]->(q:Person) WHERE q.age > 30 } RETURN p.name AS name ORDER BY name"},
	{graph: "social",
		gql:    "MATCH (p:Person) RETURN p.name AS name, COUNT { MATCH (p)-[:KNOWS]->(q) } AS friends ORDER BY name",
		cypher: "MATCH (p:Person) RETURN p.name AS name, COUNT { MATCH (p)-[:KNOWS]->(q) } AS friends ORDER BY name"},
	{graph: "social",
		gql:    "MATCH (p:Person) CALL (p) { MATCH (p)-[:KNOWS]->(f) RETURN count(f) AS friends } RETURN p.name AS name, friends ORDER BY name",
		cypher: "MATCH (p:Person) CALL { WITH p MATCH (p)-[:KNOWS]->(f) RETURN count(f) AS friends } RETURN p.name AS name, friends ORDER BY name"},
	{graph: "social",
		gql:    "MATCH (p:Person {name: 'Alice'}) RETURN p.name AS n UNION MATCH (c:Company) RETURN c.name AS n",
		cypher: "MATCH (p:Person {name: 'Alice'}) RETURN p.name AS n UNION MATCH (c:Company) RETURN c.name AS n"},
	{graph: "social",
		gql:    "MATCH (p:Person {name: $who}) RETURN p.age AS age",
		cypher: "MATCH (p:Person {name: $who}) RETURN p.age AS age",
		params: map[string]value.Value{"who": value.Str("Carol")}},
	{graph: "social",
		gql:    "MATCH (p:Person) RETURN p.name AS name, CASE WHEN p.joined > 20110000 THEN 'new' ELSE 'old' END AS era ORDER BY name",
		cypher: "MATCH (p:Person) RETURN p.name AS name, CASE WHEN p.joined > 20110000 THEN 'new' ELSE 'old' END AS era ORDER BY name"},
	{graph: "social",
		gql:    "RETURN datetime('2020-03-15T10:30:00').year AS y, date('2020-03-15') AS d, datetime({epochMillis: 0}) AS t",
		cypher: "RETURN datetime('2020-03-15T10:30:00').year AS y, date('2020-03-15') AS d, datetime({epochMillis: 0}) AS t"},
	{graph: "social",
		gql:    "CALL wcc('KNOWS') YIELD node, component RETURN count(DISTINCT component) AS n",
		cypher: "CALL wcc('KNOWS') YIELD node, component RETURN count(DISTINCT component) AS n"},
	{graph: "social",
		gql:    "CALL fts.search('Person', 'name', 'alice') YIELD node RETURN node.name AS name",
		cypher: "CALL fts.search('Person', 'name', 'alice') YIELD node RETURN node.name AS name"},
	{graph: "replies",
		gql:    "MATCH (c:Msg {name: 'c'})-[:replyOf]->*(x) RETURN x.name AS name ORDER BY name",
		cypher: "MATCH (c:Msg {name: 'c'})-[:replyOf*0..]->(x) RETURN x.name AS name ORDER BY name"},
	{graph: "weighted",
		gql:    "MATCH (a:N {name: 's'})-[r:E]->(b) RETURN b.name AS name, r.w AS w ORDER BY w",
		cypher: "MATCH (a:N {name: 's'})-[r:E]->(b) RETURN b.name AS name, r.w AS w ORDER BY w"},
	{graph: "geo",
		gql:    "CALL geo.withinRadius('Place', 'lat', 'lon', 48.85, 2.35, 30.0) YIELD node RETURN node.name AS name ORDER BY name",
		cypher: "CALL geo.withinRadius('Place', 'lat', 'lon', 48.85, 2.35, 30.0) YIELD node RETURN node.name AS name ORDER BY name"},
}

// regenSeedCorpus runs every seed query and rewrites seed/seed.json with
// this engine's rows as the expected values.
func regenSeedCorpus(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	graphs := map[string]*chickpeas.Snapshot{}
	var records []map[string]any
	for _, sq := range seedQueries {
		g, ok := graphs[sq.graph]
		if !ok {
			g = xcheckBuilders[sq.graph](t)
			graphs[sq.graph] = g
		}
		rows, err := RunWithParams(g, sq.gql, sq.params)
		if err != nil {
			t.Fatalf("seed query failed: %s\n%v", sq.gql, err)
		}
		encRows := make([]any, 0, 8)
		for r := range rows.All() {
			enc := make([]any, len(r.Values()))
			for i, v := range r.Values() {
				enc[i] = encodeValue(v)
			}
			encRows = append(encRows, enc)
		}
		rec := map[string]any{
			"cypher":  sq.cypher,
			"gql":     sq.gql,
			"graph":   sq.graph,
			"columns": rows.Columns(),
			"rows":    encRows,
		}
		if sq.unordered {
			rec["unordered"] = true
		}
		if len(sq.params) > 0 {
			enc := map[string]any{}
			for k, v := range sq.params {
				enc[k] = encodeValue(v)
			}
			rec["params"] = enc
		}
		records = append(records, rec)
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "seed.json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s (%d records)", path, len(records))
}
