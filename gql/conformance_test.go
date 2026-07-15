// GQL conformance corpus (task 119): pins the engine's answer for every
// probed ISO GQL construct -- supported constructs pin their result shape,
// unsupported ones pin a clean rejection fragment. The corpus is the
// audit's durable artifact: widening the surface (or changing an error)
// must edit the matching row consciously, and a construct silently
// changing category (OK <-> reject, or either -> panic) fails the suite.
// The ISO-vs-engine gap analysis lives in gql/CONFORMANCE.md.
package gql_test

import (
	"fmt"
	"strings"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql"
	"github.com/freeeve/gochickpeas/gql/value"
)

// conformanceFixture: Alice(30)/Bob(25)/Carol(35):Person, Paris:City;
// KNOWS Alice->Bob, Bob->Carol, Alice->Carol; LIVES_IN Alice->Paris.
func conformanceFixture(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(16, 16)
	type person struct {
		name  string
		age   int64
		score float64
	}
	var ids []chickpeas.NodeID
	for _, p := range []person{{"Alice", 30, 1.5}, {"Bob", 25, 2.5}, {"Carol", 35, 3.5}} {
		n, err := b.AddNode("Person")
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range map[string]any{"name": p.name, "age": p.age, "score": p.score} {
			if err := b.SetProp(n, k, v); err != nil {
				t.Fatal(err)
			}
		}
		ids = append(ids, n)
	}
	city, err := b.AddNode("City")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SetProp(city, "name", "Paris"); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		u, v chickpeas.NodeID
		t    string
	}{{ids[0], ids[1], "KNOWS"}, {ids[1], ids[2], "KNOWS"}, {ids[0], ids[2], "KNOWS"}, {ids[0], city, "LIVES_IN"}} {
		if _, err := b.AddRel(r.u, r.v, r.t); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize("name")
}

// renderVal renders a value for row comparison (no Stringer on Value).
func renderVal(v value.Value) string {
	switch v.Kind() {
	case value.KindNull:
		return "null"
	case value.KindBool:
		b, _ := v.AsBool()
		return fmt.Sprint(b)
	case value.KindInt:
		i, _ := v.AsInt()
		return fmt.Sprint(i)
	case value.KindFloat:
		f, _ := v.AsFloat()
		return fmt.Sprint(f)
	case value.KindStr:
		s, _ := v.AsStr()
		return s
	case value.KindList:
		l, _ := v.AsList()
		parts := make([]string, len(l))
		for i, x := range l {
			parts[i] = renderVal(x)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case value.KindMap:
		es, _ := v.AsMap()
		parts := make([]string, len(es))
		for i, e := range es {
			parts[i] = e.Key + ":" + renderVal(e.Val)
		}
		return "{" + strings.Join(parts, ",") + "}"
	case value.KindNode:
		id, _ := v.AsNode()
		return fmt.Sprintf("node(%d)", id)
	case value.KindRel:
		p, _ := v.AsRel()
		return fmt.Sprintf("rel(%d)", p)
	default:
		return fmt.Sprintf("kind%d", v.Kind())
	}
}

// conformanceProbe: rows >= 0 pins a supported construct (row count, and
// firstRow when non-empty); rows == -1 pins a rejection whose error must
// contain errFrag.
type conformanceProbe struct {
	cat, name, q string
	rows         int
	firstRow     string
	errFrag      string
	params       map[string]value.Value
}

func TestGQLConformanceCorpus(t *testing.T) {
	g := conformanceFixture(t)
	for _, p := range conformanceCorpus() {
		t.Run(p.cat+"/"+p.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic: %v\nquery: %s", r, p.q)
				}
			}()
			var rows *gql.Rows
			var err error
			if p.params != nil {
				rows, err = gql.RunWithParams(g, p.q, p.params)
			} else {
				rows, err = gql.Run(g, p.q)
			}
			if p.rows < 0 {
				if err == nil {
					t.Fatalf("expected rejection containing %q, but the query ran\nquery: %s", p.errFrag, p.q)
				}
				if !strings.Contains(err.Error(), p.errFrag) {
					t.Fatalf("error %q does not contain pinned fragment %q", err.Error(), p.errFrag)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected %d rows, got error: %v\nquery: %s", p.rows, err, p.q)
			}
			n, first := 0, ""
			for r := range rows.All() {
				if n == 0 {
					parts := make([]string, len(r.Values()))
					for i, v := range r.Values() {
						parts[i] = renderVal(v)
					}
					first = "[" + strings.Join(parts, " ") + "]"
				}
				n++
			}
			if n != p.rows {
				t.Fatalf("got %d rows, want %d\nquery: %s", n, p.rows, p.q)
			}
			if p.firstRow != "" && first != p.firstRow {
				t.Fatalf("first row %s, want %s\nquery: %s", first, p.firstRow, p.q)
			}
		})
	}
}

func conformanceCorpus() []conformanceProbe {
	return []conformanceProbe{
		{"A composition", "match-return", "MATCH (p:Person) RETURN p.name ORDER BY p.name", 3, "[Alice]", "", nil},
		{"A composition", "multi-match", "MATCH (a:Person) MATCH (a)-[:KNOWS]->(b) RETURN count(*) AS n", 1, "[3]", "", nil},
		{"A composition", "optional-match", "MATCH (c:City) OPTIONAL MATCH (c)-[:KNOWS]->(x) RETURN c.name, x", 1, "", "", nil},
		{"A composition", "match-mode-repeatable", "MATCH REPEATABLE ELEMENTS (a)-[:KNOWS]->(b) RETURN count(*) AS n", 1, "[3]", "", nil},
		{"A composition", "match-mode-different", "MATCH DIFFERENT EDGES (a)-[:KNOWS]->(b) RETURN count(*) AS n", 1, "[3]", "", nil},
		{"A composition", "filter", "MATCH (p:Person) FILTER p.age > 26 RETURN count(*) AS n", 1, "[2]", "", nil},
		{"A composition", "filter-where-form", "MATCH (p:Person) FILTER WHERE p.age > 26 RETURN count(*) AS n", -1, "", "reserved word \"WHERE\" cannot start an expression", nil},
		{"A composition", "let", "MATCH (p:Person) LET x = p.age * 2 RETURN max(x) AS m", 1, "[70]", "", nil},
		{"A composition", "let-multi", "MATCH (p:Person) LET x = 1, y = 2 RETURN x + y AS s LIMIT 1", 1, "[3]", "", nil},
		{"A composition", "for-in", "FOR x IN [1,2,3] RETURN sum(x) AS s", 1, "[6]", "", nil},
		{"A composition", "for-with-ordinality", "FOR x IN [10,20] WITH ORDINALITY o RETURN x, o", -1, "", "WITH is not GQL: use RETURN ... NEXT (projection boundary), ", nil},
		{"A composition", "for-with-offset", "FOR x IN [10,20] WITH OFFSET o RETURN x, o", -1, "", "WITH is not GQL: use RETURN ... NEXT (projection boundary), ", nil},
		{"A composition", "order-standalone", "MATCH (p:Person) ORDER BY p.age DESC LIMIT 1 RETURN p.name", 1, "[Carol]", "", nil},
		{"A composition", "return-next", "MATCH (p:Person) RETURN p.age AS a NEXT FILTER a > 26 RETURN count(*) AS n", 1, "[2]", "", nil},
		{"A composition", "return-distinct", "MATCH (a:Person)-[:KNOWS]->(b) RETURN DISTINCT a.name ORDER BY a.name", 2, "[Alice]", "", nil},
		{"A composition", "return-star", "MATCH (p:Person) RETURN * LIMIT 1", 1, "", "", nil},
		{"A composition", "offset-skip", "MATCH (p:Person) RETURN p.name ORDER BY p.name OFFSET 1 LIMIT 1", 1, "[Bob]", "", nil},
		{"A composition", "union", "MATCH (p:Person) RETURN p.name AS n UNION MATCH (c:City) RETURN c.name AS n", 4, "", "", nil},
		{"A composition", "union-all", "MATCH (p:Person) RETURN p.name AS n UNION ALL MATCH (p:Person) RETURN p.name AS n", 6, "", "", nil},
		{"A composition", "union-distinct-kw", "MATCH (p:Person) RETURN p.name UNION DISTINCT MATCH (c:City) RETURN c.name", -1, "", "expected a statement (MATCH, FILTER, LET, FOR, CALL, ORDER B", nil},
		{"A composition", "except", "MATCH (p:Person) RETURN p.name EXCEPT MATCH (c:City) RETURN c.name", -1, "", "unexpected trailing input \"EXCEPT\"", nil},
		{"A composition", "intersect", "MATCH (p:Person) RETURN p.name INTERSECT MATCH (c:City) RETURN c.name", -1, "", "unexpected trailing input \"INTERSECT\"", nil},
		{"A composition", "otherwise", "MATCH (p:Person) RETURN p.name OTHERWISE MATCH (c:City) RETURN c.name", -1, "", "unexpected trailing input \"OTHERWISE\"", nil},
		{"A composition", "call-subquery", "MATCH (p:Person) CALL { MATCH (c:City) RETURN c.name AS cn } RETURN count(*) AS n", 1, "[3]", "", nil},
		{"A composition", "call-subquery-import", "MATCH (p:Person) CALL (p) { RETURN p.age * 2 AS d } RETURN max(d) AS m", 1, "[70]", "", nil},
		{"A composition", "call-proc-yield", "CALL algo.bfs(0, 'KNOWS') YIELD node, depth RETURN count(*) AS n", -1, "", "algo.bfs argument `directed` (position 1) must be a boolean", nil},
		{"A composition", "call-proc-unknown", "CALL no.such.proc() YIELD x RETURN x", -1, "", "unknown procedure `no.such.proc` (supported: wcc, algo.bfs, ", nil},
		{"A composition", "use-graph", "USE g MATCH (p:Person) RETURN p.name", -1, "", "expected a statement (MATCH, FILTER, LET, FOR, CALL, ORDER B", nil},
		{"A composition", "explain-prefix", "EXPLAIN MATCH (p:Person) RETURN p.name", 5, "", "", nil},
		{"A composition", "with-rejected", "MATCH (p:Person) WITH p RETURN p.name", -1, "", "WITH is not GQL: use RETURN ... NEXT (projection boundary), ", nil},
		{"A composition", "unwind-rejected", "UNWIND [1,2] AS x RETURN x", -1, "", "UNWIND is not GQL: use FOR x IN <list>", nil},
		{"B patterns", "node-bare", "MATCH () RETURN count(*) AS n", 1, "[4]", "", nil},
		{"B patterns", "node-var-only", "MATCH (v) RETURN count(*) AS n", 1, "[4]", "", nil},
		{"B patterns", "node-label-only", "MATCH (:Person) RETURN count(*) AS n", 1, "[3]", "", nil},
		{"B patterns", "node-props", "MATCH (p {name: 'Alice'}) RETURN p.age", 1, "[30]", "", nil},
		{"B patterns", "node-inline-where", "MATCH (p:Person WHERE p.age > 26) RETURN count(*) AS n", 1, "[2]", "", nil},
		{"B patterns", "label-or", "MATCH (x:Person|City) RETURN count(*) AS n", 1, "[4]", "", nil},
		{"B patterns", "label-and", "MATCH (x:Person&City) RETURN count(*) AS n", 1, "[0]", "", nil},
		{"B patterns", "label-not", "MATCH (x:!City) RETURN count(*) AS n", 1, "[3]", "", nil},
		{"B patterns", "label-parens", "MATCH (x:(Person|City)&!City) RETURN count(*) AS n", 1, "[3]", "", nil},
		{"B patterns", "label-wildcard-pct", "MATCH (x:%) RETURN count(*) AS n", 1, "[4]", "", nil},
		{"B patterns", "edge-out", "MATCH (a)-[:KNOWS]->(b) RETURN count(*) AS n", 1, "[3]", "", nil},
		{"B patterns", "edge-in", "MATCH (a)<-[:KNOWS]-(b) RETURN count(*) AS n", 1, "[3]", "", nil},
		{"B patterns", "edge-undirected-dash", "MATCH (a)-[:KNOWS]-(b) RETURN count(*) AS n", 1, "[6]", "", nil},
		{"B patterns", "edge-abbrev-out", "MATCH (a:Person)-->(b) RETURN count(*) AS n", 1, "[4]", "", nil},
		{"B patterns", "edge-abbrev-in", "MATCH (a:Person)<--(b) RETURN count(*) AS n", 1, "[3]", "", nil},
		{"B patterns", "edge-abbrev-both", "MATCH (a:City)--(b) RETURN count(*) AS n", 1, "[1]", "", nil},
		{"B patterns", "edge-tilde-undirected", "MATCH (a)~[:KNOWS]~(b) RETURN count(*) AS n", -1, "", "unexpected character \"~\"", nil},
		{"B patterns", "edge-any-directed", "MATCH (a)<-[:KNOWS]->(b) RETURN count(*) AS n", -1, "", "expected '(' starting a node pattern, found \">\"", nil},
		{"B patterns", "rel-type-alternation", "MATCH (a)-[:KNOWS|LIVES_IN]->(b) RETURN count(*) AS n", 1, "[4]", "", nil},
		{"B patterns", "rel-type-negation", "MATCH (a)-[:!KNOWS]->(b) RETURN count(*) AS n", -1, "", "expected a relationship type, found \"!\"", nil},
		{"B patterns", "rel-inline-props", "MATCH (a)-[r:KNOWS {since: 1}]->(b) RETURN count(*) AS n", -1, "", "inline relationship properties are not supported (Tier 1)", nil},
		{"B patterns", "rel-inline-where", "MATCH (a)-[r:KNOWS WHERE a.age > 0]->(b) RETURN count(*) AS n", 1, "[3]", "", nil},
		{"B patterns", "quant-star", "MATCH (a {name:'Alice'})-[:KNOWS]->*(b) RETURN count(*) AS n", 1, "", "", nil},
		{"B patterns", "quant-plus", "MATCH (a {name:'Alice'})-[:KNOWS]->+(b) RETURN count(*) AS n", 1, "", "", nil},
		{"B patterns", "quant-mn", "MATCH (a {name:'Alice'})-[:KNOWS]->{1,2}(b) RETURN count(*) AS n", 1, "[3]", "", nil},
		{"B patterns", "quant-m-open", "MATCH (a {name:'Alice'})-[:KNOWS]->{1,}(b) RETURN count(*) AS n", 1, "", "", nil},
		{"B patterns", "quant-n-only", "MATCH (a {name:'Alice'})-[:KNOWS]->{,2}(b) RETURN count(*) AS n", 1, "", "", nil},
		{"B patterns", "quant-exact", "MATCH (a {name:'Alice'})-[:KNOWS]->{2}(b) RETURN count(*) AS n", 1, "[1]", "", nil},
		{"B patterns", "quant-question", "MATCH (a {name:'Alice'})-[:KNOWS]->?(b) RETURN count(*) AS n", 1, "[3]", "", nil},
		{"B patterns", "paren-path-quant", "MATCH ((a)-[:KNOWS]->(b)){1,2} RETURN count(*) AS n", -1, "", "expected ')' closing a node pattern, found \"(\"", nil},
		{"B patterns", "named-path", "MATCH p = (a {name:'Alice'})-[:KNOWS]->(b) RETURN length(p) AS l LIMIT 1", 1, "[1]", "", nil},
		{"B patterns", "path-mode-trail", "MATCH TRAIL (a)-[:KNOWS]->{1,2}(b) RETURN count(*) AS n", 1, "", "", nil},
		{"B patterns", "path-mode-acyclic", "MATCH ACYCLIC (a)-[:KNOWS]->{1,2}(b) RETURN count(*) AS n", 1, "", "", nil},
		{"B patterns", "path-mode-walk", "MATCH WALK (a)-[:KNOWS]->{1,2}(b) RETURN count(*) AS n", -1, "", "the WALK path mode is not supported: traversal is TRAIL (no ", nil},
		{"B patterns", "path-mode-simple", "MATCH SIMPLE (a)-[:KNOWS]->{1,2}(b) RETURN count(*) AS n", -1, "", "the SIMPLE path mode is not supported (TRAIL and ACYCLIC are", nil},
		{"B patterns", "any-shortest", "MATCH (a {name:'Alice'}), (c {name:'Carol'}) MATCH p = ANY SHORTEST (a)-[:KNOWS]->{1,3}(c) RETURN length(p) AS l", 1, "[1]", "", nil},
		{"B patterns", "all-shortest", "MATCH (a {name:'Alice'}), (c {name:'Carol'}) MATCH p = ALL SHORTEST (a)-[:KNOWS]->{1,3}(c) RETURN count(*) AS n", 1, "[1]", "", nil},
		{"B patterns", "shortest-bare", "MATCH p = SHORTEST (a)-[:KNOWS]->{1,3}(b) RETURN count(*) AS n", -1, "", "bare SHORTEST is not supported: use ANY SHORTEST or ALL SHOR", nil},
		{"B patterns", "shortest-k", "MATCH p = SHORTEST 2 (a)-[:KNOWS]->{1,3}(b) RETURN count(*) AS n", -1, "", "bare SHORTEST is not supported: use ANY SHORTEST or ALL SHOR", nil},
		{"B patterns", "any-k", "MATCH p = ANY 2 (a)-[:KNOWS]->{1,3}(b) RETURN count(*) AS n", -1, "", "expected '(' starting a node pattern, found \"ANY\"", nil},
		{"B patterns", "comma-patterns", "MATCH (a:Person), (c:City) RETURN count(*) AS n", 1, "[3]", "", nil},
		{"B patterns", "equijoin-var", "MATCH (a)-[:KNOWS]->(b), (b)-[:KNOWS]->(c) RETURN count(*) AS n", 1, "[1]", "", nil},
		{"C expressions", "arith-precedence", "RETURN 2 + 3 * 4 AS x", 1, "[14]", "", nil},
		{"C expressions", "unary-minus", "RETURN -(2 + 3) AS x", 1, "[-5]", "", nil},
		{"C expressions", "division", "RETURN 7 / 2 AS x", 1, "", "", nil},
		{"C expressions", "modulo-pct", "RETURN 7 % 2 AS x", 1, "[1]", "", nil},
		{"C expressions", "power-caret", "RETURN 2 ^ 3 AS x", -1, "", "unexpected character \"^\"", nil},
		{"C expressions", "concat-pipes", "RETURN 'a' || 'b' AS x", -1, "", "unexpected trailing input \"|\"", nil},
		{"C expressions", "concat-plus", "RETURN 'a' + 'b' AS x", 1, "[ab]", "", nil},
		{"C expressions", "cmp-chain", "RETURN 1 < 2 AS x", 1, "[true]", "", nil},
		{"C expressions", "neq", "RETURN 1 <> 2 AS x", 1, "[true]", "", nil},
		{"C expressions", "bool-ops", "RETURN (true AND NOT false) OR false AS x", 1, "[true]", "", nil},
		{"C expressions", "xor", "RETURN true XOR false AS x", -1, "", "unexpected trailing input \"XOR\"", nil},
		{"C expressions", "is-null", "MATCH (c:City) RETURN c.age IS NULL AS x", 1, "[true]", "", nil},
		{"C expressions", "is-not-null", "MATCH (c:City) RETURN c.name IS NOT NULL AS x", 1, "[true]", "", nil},
		{"C expressions", "is-true", "RETURN (1 < 2) IS TRUE AS x", -1, "", "expected NULL or LABELED after IS", nil},
		{"C expressions", "is-unknown", "RETURN (null = 1) IS UNKNOWN AS x", -1, "", "expected NULL or LABELED after IS", nil},
		{"C expressions", "in-list", "RETURN 2 IN [1,2,3] AS x", 1, "[true]", "", nil},
		{"C expressions", "starts-with", "MATCH (p {name:'Alice'}) RETURN p.name STARTS WITH 'Al' AS x", 1, "[true]", "", nil},
		{"C expressions", "ends-with", "MATCH (p {name:'Alice'}) RETURN p.name ENDS WITH 'ce' AS x", 1, "[true]", "", nil},
		{"C expressions", "contains", "MATCH (p {name:'Alice'}) RETURN p.name CONTAINS 'lic' AS x", 1, "[true]", "", nil},
		{"C expressions", "regex-tilde", "RETURN 'abc' =~ 'a.*' AS x", -1, "", "unexpected character \"~\"", nil},
		{"C expressions", "exists-pattern", "MATCH (p:Person) RETURN count(*) AS n, sum(CASE WHEN EXISTS { (p)-[:LIVES_IN]->(:City) } THEN 1 ELSE 0 END) AS l", 1, "[3 1]", "", nil},
		{"C expressions", "exists-match-where", "MATCH (p:Person) FILTER EXISTS { MATCH (p)-[:KNOWS]->(q) WHERE q.age > 30 } RETURN count(*) AS n", 1, "[2]", "", nil},
		{"C expressions", "count-subquery", "MATCH (p {name:'Alice'}) RETURN COUNT { (p)-[:KNOWS]->(x) } AS n", 1, "[2]", "", nil},
		{"C expressions", "value-subquery", "RETURN VALUE { MATCH (c:City) RETURN c.name } AS x", -1, "", "map projections (var{.key}) are not in the GQL subset: proje", nil},
		{"C expressions", "is-labeled", "MATCH (x) FILTER x IS LABELED Person RETURN count(*) AS n", 1, "[3]", "", nil},
		{"C expressions", "postfix-label-pred", "MATCH (x) FILTER x:Person RETURN count(*) AS n", 1, "[3]", "", nil},
		{"C expressions", "is-typed", "MATCH (x) RETURN x IS TYPED INTEGER AS t LIMIT 1", -1, "", "expected NULL or LABELED after IS", nil},
		{"C expressions", "same-fn", "MATCH (a), (b) FILTER SAME(a, b) RETURN count(*) AS n", -1, "", "unknown function `SAME`", nil},
		{"C expressions", "all-different", "MATCH (a)-[:KNOWS]->(b) FILTER ALL_DIFFERENT(a, b) RETURN count(*) AS n", -1, "", "unknown function `ALL_DIFFERENT`", nil},
		{"C expressions", "property-exists", "MATCH (x) FILTER PROPERTY_EXISTS(x, age) RETURN count(*) AS n", -1, "", "unknown function `PROPERTY_EXISTS`", nil},
		{"C expressions", "case-searched", "RETURN CASE WHEN 1 < 2 THEN 'y' ELSE 'n' END AS x", 1, "[y]", "", nil},
		{"C expressions", "case-simple", "RETURN CASE 2 WHEN 1 THEN 'a' WHEN 2 THEN 'b' END AS x", 1, "[b]", "", nil},
		{"C expressions", "cast-int", "RETURN CAST('42' AS INT) AS x", 1, "[42]", "", nil},
		{"C expressions", "cast-string", "RETURN CAST(42 AS STRING) AS x", 1, "[42]", "", nil},
		{"C expressions", "cast-date-target", "RETURN CAST('2024-01-01' AS DATE) AS x", -1, "", "CAST target \"DATE\" is not supported (FLOAT, INTEGER, STRING,", nil},
		{"C expressions", "coalesce", "RETURN coalesce(null, 3) AS x", 1, "[3]", "", nil},
		{"C expressions", "nullif", "RETURN NULLIF(1, 1) AS x", 1, "[null]", "", nil},
		{"C expressions", "param-named", "RETURN $n + 1 AS x", 1, "[42]", "", map[string]value.Value{"n": value.Int(41)}},
		{"C expressions", "param-in-props", "MATCH (p {name: $who}) RETURN p.age", 1, "[30]", "", map[string]value.Value{"who": value.Str("Alice")}},
		{"C expressions", "string-single-quote", "RETURN 'it''s' AS x", 1, "[it's]", "", nil},
		{"C expressions", "string-backslash-escape", "RETURN 'a\\nb' AS x", 1, "[a\nb]", "", nil},
		{"C expressions", "sci-float", "RETURN 1.5e2 AS x", 1, "[150]", "", nil},
		{"C expressions", "temporal-date-literal", "RETURN DATE '2024-01-01' AS x", -1, "", "unexpected trailing input \"2024-01-01\"", nil},
		{"C expressions", "temporal-duration-literal", "RETURN DURATION 'PT1H' AS x", -1, "", "unexpected trailing input \"PT1H\"", nil},
		{"C expressions", "date-func", "RETURN date('2024-01-02').year AS y", 1, "", "", nil},
		{"C expressions", "duration-func", "RETURN duration('PT2H').hours AS h", 1, "[2]", "", nil},
		{"C expressions", "list-literal", "RETURN [1, 2, 3] AS x", 1, "", "", nil},
		{"C expressions", "map-literal", "RETURN {a: 1, b: 'x'} AS m", 1, "", "", nil},
		{"C expressions", "list-index", "RETURN [10,20,30][1] AS x", 1, "[20]", "", nil},
		{"C expressions", "list-slice", "RETURN [10,20,30][1..3] AS x", 1, "", "", nil},
		{"C expressions", "list-comp", "RETURN [x IN [1,2,3] WHERE x > 1 | x * 10] AS l", 1, "", "", nil},
		{"C expressions", "list-pred-all", "RETURN all(x IN [1,2,3] WHERE x > 0) AS b", 1, "[true]", "", nil},
		{"C expressions", "pattern-comp", "MATCH (p {name:'Alice'}) RETURN [ (p)-[:KNOWS]->(x) | x.name ] AS l", -1, "", "pattern comprehensions are not in the GQL subset: rewrite si", nil},
		{"C expressions", "reduce", "RETURN reduce(s = 0, x IN [1,2,3] | s + x) AS t", -1, "", "reduce(...) is not in the GQL subset", nil},
		{"D functions", "count-star", "MATCH (p:Person) RETURN count(*) AS n", 1, "[3]", "", nil},
		{"D functions", "count-distinct", "MATCH (a)-[:KNOWS]->(b) RETURN count(DISTINCT a) AS n", 1, "[2]", "", nil},
		{"D functions", "sum-avg-min-max", "MATCH (p:Person) RETURN sum(p.age) AS s, avg(p.age) AS a, min(p.age) AS lo, max(p.age) AS hi", 1, "[90 30 25 35]", "", nil},
		{"D functions", "collect", "MATCH (p:Person) RETURN collect(p.name) AS l", 1, "", "", nil},
		{"D functions", "collect-list-alias", "MATCH (p:Person) RETURN collect_list(p.name) AS l", 1, "", "", nil},
		{"D functions", "stddev", "MATCH (p:Person) RETURN stddev_samp(p.age) AS s", 1, "[5]", "", nil},
		{"D functions", "percentile-cont", "MATCH (p:Person) RETURN percentile_cont(p.age, 0.5) AS m", -1, "", "unknown function `percentile_cont`", nil},
		{"D functions", "char-length", "RETURN char_length('abc') AS n", 1, "[3]", "", nil},
		{"D functions", "size-string", "RETURN size('abc') AS n", 1, "[3]", "", nil},
		{"D functions", "cardinality", "RETURN cardinality([1,2]) AS n", 1, "[2]", "", nil},
		{"D functions", "upper-lower", "RETURN upper('ab') + lower('CD') AS x", 1, "[ABcd]", "", nil},
		{"D functions", "trim", "RETURN trim('  x  ') AS x", 1, "[x]", "", nil},
		{"D functions", "ltrim-rtrim", "RETURN ltrim('  x') + rtrim('y  ') AS x", 1, "[xy]", "", nil},
		{"D functions", "substring", "RETURN substring('hello', 1, 3) AS x", 1, "", "", nil},
		{"D functions", "left-right", "RETURN left('hello', 2) + right('hello', 2) AS x", 1, "[helo]", "", nil},
		{"D functions", "normalize", "RETURN normalize('abc') AS x", -1, "", "unknown function `normalize`", nil},
		{"D functions", "abs-mod", "RETURN abs(-3) AS a, mod(7, 2) AS m", 1, "[3 1]", "", nil},
		{"D functions", "power-fn", "RETURN power(2, 10) AS x", 1, "[1024]", "", nil},
		{"D functions", "exp-ln-log", "RETURN exp(0) AS e, ln(1) AS l, log10(100) AS g", 1, "[1 0 2]", "", nil},
		{"D functions", "trig", "RETURN sin(0) AS s, cos(0) AS c, tan(0) AS t", 1, "[0 1 0]", "", nil},
		{"D functions", "floor-ceil-round-sign-sqrt", "RETURN floor(1.5) AS f, ceil(1.5) AS c, round(1.4) AS r, sign(-2) AS s, sqrt(4.0) AS q", 1, "", "", nil},
		{"D functions", "element-id", "MATCH (p {name:'Alice'}) RETURN element_id(p) AS x", 1, "", "", nil},
		{"D functions", "id-fn", "MATCH (p {name:'Alice'}) RETURN id(p) AS x", 1, "", "", nil},
		{"D functions", "labels-fn", "MATCH (p {name:'Alice'}) RETURN labels(p) AS l", 1, "[[Person]]", "", nil},
		{"D functions", "type-fn", "MATCH (a)-[r:KNOWS]->(b) RETURN type(r) AS t LIMIT 1", 1, "[KNOWS]", "", nil},
		{"D functions", "properties-fn", "MATCH (p {name:'Alice'}) RETURN properties(p) AS m", -1, "", "unknown function `properties`", nil},
		{"D functions", "startnode-endnode", "MATCH (a)-[r:LIVES_IN]->(b) RETURN startNode(r) = a AS s, endNode(r) = b AS e", 1, "[true true]", "", nil},
		{"D functions", "nodes-rels-path", "MATCH p = (a {name:'Alice'})-[:KNOWS]->(b) RETURN size(nodes(p)) AS n, size(relationships(p)) AS r LIMIT 1", 1, "[2 1]", "", nil},
		{"D functions", "range-fn", "RETURN size(range(1, 5)) AS n", 1, "[5]", "", nil},
		{"D functions", "to-conversions", "RETURN toInteger('4') + toFloat('0.5') AS x, toString(9) AS s, toBoolean('true') AS b", 1, "", "", nil},
		{"D functions", "head-last-tail", "RETURN head([1,2,3]) AS h", 1, "[1]", "", nil},
		{"E out-of-scope", "insert", "INSERT (:Person {name: 'X'})", -1, "", "INSERT is not supported: this is a read-only engine", nil},
		{"E out-of-scope", "set", "MATCH (p:Person) SET p.age = 1", -1, "", "SET is not supported: this is a read-only engine", nil},
		{"E out-of-scope", "delete", "MATCH (p:Person) DELETE p", -1, "", "DELETE is not supported: this is a read-only engine", nil},
		{"E out-of-scope", "detach-delete", "MATCH (p:Person) DETACH DELETE p", -1, "", "DETACH is not supported: this is a read-only engine", nil},
		{"E out-of-scope", "create", "CREATE (:Person)", -1, "", "CREATE is not supported: this is a read-only engine", nil},
		{"E out-of-scope", "merge", "MERGE (p:Person {name:'X'}) RETURN p", -1, "", "MERGE is not supported: this is a read-only engine", nil},
		{"E out-of-scope", "create-graph-ddl", "CREATE GRAPH g ANY", -1, "", "CREATE is not supported: this is a read-only engine", nil},
		{"E out-of-scope", "start-transaction", "START TRANSACTION MATCH (p) RETURN p", -1, "", "expected a statement (MATCH, FILTER, LET, FOR, CALL, ORDER B", nil},
		{"E out-of-scope", "commit", "COMMIT", -1, "", "COMMIT is not supported: this is a read-only engine", nil},
		{"E out-of-scope", "session-set", "SESSION SET SCHEMA s", -1, "", "SESSION is not supported: this is a read-only engine", nil},
		{"F robustness", "empty-input", "", -1, "", "expected a statement, found \"\"", nil},
		{"F robustness", "whitespace-only", "   \n\t  ", -1, "", "expected a statement, found \"\"", nil},
		{"F robustness", "line-comment", "RETURN 1 AS x // trailing comment", 1, "[1]", "", nil},
		{"F robustness", "block-comment", "RETURN /* mid */ 1 AS x", 1, "[1]", "", nil},
		{"F robustness", "deep-nesting", "RETURN " + strings.Repeat("(", 200) + "1" + strings.Repeat(")", 200) + " AS x", 1, "", "", nil},
		{"F robustness", "deep-bool-chain", "RETURN 1=1" + strings.Repeat(" AND 1=1", 300) + " AS x", 1, "", "", nil},
		{"F robustness", "unterminated-string", "RETURN 'abc AS x", -1, "", "unterminated string", nil},
		{"F robustness", "near-miss-keyword", "MTCH (p) RETURN p", -1, "", "expected a statement (MATCH, FILTER, LET, FOR, CALL, ORDER B", nil},
		{"F robustness", "trailing-garbage", "RETURN 1 AS x ;", -1, "", "unexpected character \";\"", nil},
		{"F robustness", "unicode-string", "RETURN 'héllo→世界' AS x", 1, "[héllo→世界]", "", nil},
		{"F robustness", "unicode-identifier", "MATCH (héllo:Person) RETURN count(*) AS n", 1, "[3]", "", nil},
		{"F robustness", "quoted-identifier", "MATCH (p:`Weird Label`) RETURN count(*) AS n", 1, "[0]", "", nil},
		{"F robustness", "reserved-as-var", "MATCH (match:Person) RETURN count(*) AS n", -1, "", "reserved word \"match\" cannot be a node variable", nil},
		{"F robustness", "huge-int-literal", "RETURN 9223372036854775807 AS x", 1, "[9223372036854775807]", "", nil},
		{"F robustness", "int-overflow-literal", "RETURN 99999999999999999999999 AS x", -1, "", "bad integer \"99999999999999999999999\"", nil},
		{"F robustness", "division-by-zero", "RETURN 1 / 0 AS x", 1, "", "", nil},
	}
}
