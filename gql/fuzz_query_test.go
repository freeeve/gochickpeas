// FuzzQuery: whole-query invariant fuzzing. Any input must either fail
// with a typed error (ErrParse/ErrBind/ErrPlan/ErrEval) or produce rows
// that are deterministic, identical across the dual eval paths, and
// consistent with the query's own DISTINCT / ORDER BY / LIMIT clauses.
package gql

import (
	"errors"
	"regexp"
	"strconv"
	"testing"

	"github.com/freeeve/gochickpeas/gql/value"
)

var (
	// fuzzOrderBy matches a trailing "ORDER BY <bare column> [DESC]"
	// followed only by pagination -- the shape whose sortedness we can
	// verify without re-implementing the planner.
	fuzzOrderBy = regexp.MustCompile(`(?is)order\s+by\s+([a-z_][a-z0-9_]*)\s*(desc)?\s*((offset|skip)\s+\d+\s*)?(limit\s+\d+\s*)?$`)
	// fuzzLimit matches a trailing LIMIT bound.
	fuzzLimit = regexp.MustCompile(`(?is)limit\s+(\d+)\s*$`)
	// fuzzMultiPart disables the single-projection checks: with NEXT or
	// UNION the final row set is not governed by one clause.
	fuzzMultiPart = regexp.MustCompile(`(?i)\b(next|union)\b`)
	// fuzzDistinct marks a RETURN DISTINCT projection.
	fuzzDistinct = regexp.MustCompile(`(?i)return\s+distinct`)
	// fuzzPlanMode marks EXPLAIN/PROFILE queries, which return a rendered
	// plan (with a wall-clock planning-time header) rather than query rows
	// -- the row invariants don't apply.
	fuzzPlanMode = regexp.MustCompile(`(?i)^\s*(explain|profile)\b`)
)

// fuzzRows collects a result's canonical row encodings and per-column
// values.
func fuzzRows(rows *Rows) (keys []string, vals [][]value.Value) {
	for r := range rows.All() {
		var k []byte
		row := make([]value.Value, len(r.Values()))
		copy(row, r.Values())
		for _, v := range row {
			k = value.AppendKey(k, v)
		}
		keys = append(keys, string(k))
		vals = append(vals, row)
	}
	return keys, vals
}

func FuzzQuery(f *testing.F) {
	for _, seed := range []string{
		"MATCH (p:Person) RETURN p.name AS name",
		"MATCH (p:Person) WHERE p.age > 30 RETURN p.name AS name ORDER BY name",
		"MATCH (p:Person {name: 'Alice'}) RETURN p.age AS age",
		"MATCH (p:Person) RETURN DISTINCT p.city AS city",
		"MATCH (p:Person) RETURN p.name AS name ORDER BY p.age DESC LIMIT 2",
		"MATCH (p:Person) RETURN p.name AS name ORDER BY name OFFSET 1 LIMIT 2",
		"MATCH (p) WHERE id(p) = 3 RETURN p.name AS name",
		"MATCH (p:Person) WHERE p.name STARTS WITH 'A' RETURN p.name AS n",
		"MATCH (p:Person) WHERE p.age IN [25, 30.0] RETURN p.name AS n",
		"MATCH (p:Person) WHERE p.city IS NULL RETURN p.name AS n",
		"MATCH (a:Person {name: 'Alice'})-[:KNOWS]->(f) RETURN f.name AS n",
		"MATCH (a:Person)-[:KNOWS]->(b)-[:WORKS_AT]->(c) RETURN c.name AS n",
		"MATCH (a:Person {name: 'Alice'})-[:KNOWS]->{1,2}(f) RETURN DISTINCT f.name AS n",
		"MATCH (a:Person {name: 'Alice'})-[:KNOWS]->+(b) RETURN DISTINCT b.name AS n",
		"MATCH (c:Msg)-[:replyOf]->*(x) RETURN x.name AS n",
		"MATCH (p:Person) OPTIONAL MATCH (p)-[:WORKS_AT]->(c) RETURN p.name AS a, c.name AS b",
		"MATCH p = (a:Person {name: 'Alice'})-[:KNOWS]->(b) RETURN length(p) AS l",
		"MATCH (a:Person {name: 'Alice'}), (b:Person {name: 'Dave'}) MATCH p = ANY SHORTEST (a)-[:KNOWS]->{1,}(b) RETURN length(p) AS l",
		"MATCH (c:Company)<-[:WORKS_AT]-(p) RETURN c.name AS c, count(*) AS n ORDER BY n DESC",
		"MATCH (p:Person) RETURN sum(p.age) AS s, avg(p.age) AS a",
		"MATCH (p:Person) RETURN collect(p.name) AS ns NEXT FOR n IN ns RETURN n AS n",
		"MATCH (p:Person) CALL (p) { MATCH (p)-[:KNOWS]->(f) RETURN count(f) AS c } RETURN p.name AS n, c",
		"MATCH (p:Person) WHERE EXISTS { MATCH (p)-[:KNOWS]->(q) } RETURN p.name AS n",
		"MATCH (p:Person) RETURN p.name AS n, COUNT { MATCH (p)-[:KNOWS]->(q) } AS c",
		"MATCH (p:Person {name: 'Alice'}) RETURN p.name AS n UNION MATCH (c:Company) RETURN c.name AS n",
		"MATCH (p:Person {name: $who}) RETURN p.age AS age",
		"RETURN datetime('2020-03-15').year AS y",
		"MATCH (p:Person) RETURN CASE WHEN p.age > 30 THEN 'a' ELSE 'b' END AS x",
		"CALL wcc('KNOWS') YIELD node, component RETURN count(DISTINCT component) AS n",
		"FOR x IN [1, 2, 3] RETURN x AS x ORDER BY x DESC",
		"EXPLAIN MATCH (p:Person) RETURN p.name AS n",
		"MATCH (p:Person RETURN p",
		"INSERT (n:Person) RETURN n",
		"RETURN reduce(s = 0, x IN [1] | s + x) AS r",
		"MATCH (p:Person) RETURN q.name AS n",
	} {
		f.Add(seed)
	}
	g := socialGraph(f)
	f.Fuzz(func(t *testing.T, q string) {
		rows, err := Run(g, q)
		if err != nil {
			if !errors.Is(err, ErrParse) && !errors.Is(err, ErrBind) &&
				!errors.Is(err, ErrPlan) && !errors.Is(err, ErrEval) {
				t.Fatalf("untyped error for %q: %v", q, err)
			}
			return
		}
		if fuzzPlanMode.MatchString(q) {
			return
		}
		keys, vals := fuzzRows(rows)

		// Determinism: a second run produces identical rows.
		again, err := Run(g, q)
		if err != nil {
			t.Fatalf("second run failed where first succeeded: %q: %v", q, err)
		}
		keys2, _ := fuzzRows(again)
		if len(keys) != len(keys2) {
			t.Fatalf("nondeterministic row count for %q: %d vs %d", q, len(keys), len(keys2))
		}
		for i := range keys {
			if keys[i] != keys2[i] {
				t.Fatalf("nondeterministic row %d for %q", i, q)
			}
		}

		// Dual-path equality.
		forceInterp = true
		interp, ierr := Run(g, q)
		forceInterp = false
		if ierr != nil {
			t.Fatalf("interpreted path failed where compiled succeeded: %q: %v", q, ierr)
		}
		keys3, _ := fuzzRows(interp)
		if len(keys) != len(keys3) {
			t.Fatalf("dual-path row count divergence for %q", q)
		}
		for i := range keys {
			if keys[i] != keys3[i] {
				t.Fatalf("dual-path divergence at row %d for %q", i, q)
			}
		}

		multiPart := fuzzMultiPart.MatchString(q)

		// DISTINCT: no duplicate rows.
		if !multiPart && fuzzDistinct.MatchString(q) {
			seen := map[string]struct{}{}
			for i, k := range keys {
				if _, dup := seen[k]; dup {
					t.Fatalf("DISTINCT produced duplicate row %d for %q", i, q)
				}
				seen[k] = struct{}{}
			}
		}

		// ORDER BY a bare output column: monotone per OrderCmp.
		if m := fuzzOrderBy.FindStringSubmatch(q); m != nil && !multiPart {
			col, desc := m[1], m[2] != ""
			idx := -1
			for i, c := range rows.Columns() {
				if c == col {
					idx = i
				}
			}
			if idx >= 0 {
				for i := 1; i < len(vals); i++ {
					c := value.OrderCmp(vals[i-1][idx], vals[i][idx])
					if (desc && c < 0) || (!desc && c > 0) {
						t.Fatalf("ORDER BY %s violated between rows %d and %d for %q", col, i-1, i, q)
					}
				}
			}
		}

		// LIMIT bounds the row count.
		if m := fuzzLimit.FindStringSubmatch(q); m != nil && !multiPart {
			n, _ := strconv.Atoi(m[1])
			if len(keys) > n {
				t.Fatalf("LIMIT %d yielded %d rows for %q", n, len(keys), q)
			}
		}
	})
}
