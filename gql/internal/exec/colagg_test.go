// Differential tests for the fused columnar aggregation: every fusable
// chain shape must produce exactly the general pipeline's rows, pinned by
// running each query both ways via the disableColAgg knob -- absent
// properties (null keys), CASE without ELSE, empty labels, carried
// columns from an earlier segment, and post-aggregate wrappers included.
package exec

import (
	"fmt"
	"sort"
	"testing"

	"github.com/freeeve/gochickpeas/gql/internal/ast"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

func colAggFixture(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(64, 8)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// Messages with varied dates/lengths; some are Comments; some lack
	// props entirely (null keys, absent sum args).
	day := int64(86_400_000)
	for i := 0; i < 30; i++ {
		label := "Message"
		n, _ := b.AddNode(label)
		if i%3 == 0 {
			// A second label via a fresh builder API? Labels are set at
			// AddNode; emulate Comment via a distinct co-label node set.
			_ = n
		}
		if i%7 != 6 { // node 6, 13, 20, 27 lack creationDate
			must(b.SetProp(n, "creationDate", int64(1_300_000_000_000)+int64(i)*37*day))
		}
		if i%5 != 4 { // some lack length
			must(b.SetProp(n, "length", int64(i*13%200)))
		}
		if i%4 == 0 {
			must(b.SetProp(n, "flag", i%8 == 0))
		}
	}
	// Comments carry both labels: rebuild with AddNode multi-label form.
	for i := 0; i < 10; i++ {
		n, _ := b.AddNode("Message", "Comment")
		must(b.SetProp(n, "creationDate", int64(1_310_000_000_000)+int64(i)*53*day))
		must(b.SetProp(n, "length", int64(i*29%200)))
	}
	return b.Finalize("colagg")
}

// runBoth runs q fused and general, returning both row sets rendered.
func runBoth(t *testing.T, g *chickpeas.Snapshot, q string) (fused, general []string) {
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
	disableColAgg = false
	fused = run()
	disableColAgg = true
	general = run()
	disableColAgg = false
	return fused, general
}

func TestColumnarAggMatchesGeneral(t *testing.T) {
	g := colAggFixture(t)
	queries := []string{
		// Bare count with a range filter (the fused single segment).
		`MATCH (m:Message) WHERE m.creationDate < zoned_datetime('2012-01-01')
		 RETURN count(m) AS c`,
		// The full Q1 chain shape: carried column, LET boundaries with
		// component/label/CASE keys, hidden aggregates + post wrapper.
		`MATCH (m:Message) WHERE m.creationDate < zoned_datetime('2013-01-01')
		 RETURN count(m) AS totalInt
		 NEXT LET total = CAST(totalInt AS FLOAT)
		 NEXT MATCH (m:Message) WHERE m.creationDate < zoned_datetime('2013-01-01') AND m.length IS NOT NULL
		 LET year = m.creationDate.year
		 LET isComment = m IS LABELED Comment
		 LET cat = CASE WHEN m.length < 40 THEN 0 WHEN m.length < 80 THEN 1 WHEN m.length < 160 THEN 2 ELSE 3 END
		 RETURN total, year, isComment, cat, count(m) AS n,
		        sum(m.length) / CAST(count(m) AS FLOAT) AS avgLen,
		        sum(m.length) AS sumLen
		 NEXT RETURN year, isComment, cat, n, avgLen, sumLen, n / total AS pct
		 ORDER BY year DESC, isComment ASC, cat ASC`,
		// Null group keys: nodes lacking creationDate group under null.
		`MATCH (m:Message) LET year = m.creationDate.year
		 RETURN year, count(m) AS n NEXT RETURN year, n ORDER BY n, year`,
		// CASE without ELSE: unmatched rows take the null key.
		`MATCH (m:Message)
		 LET cat = CASE WHEN m.length < 40 THEN 0 WHEN m.length < 80 THEN 1 END
		 RETURN cat, count(m) AS n NEXT RETURN cat, n ORDER BY n, cat`,
		// Bool property key + sum over sparse arg.
		`MATCH (m:Message) RETURN m.flag AS f, sum(m.length) AS s, count(m) AS n
		 NEXT RETURN f, s, n ORDER BY n`,
		// Unknown label: zero groups (keyless emits one zeroed row).
		`MATCH (m:NoSuchLabel) RETURN count(m) AS c`,
		// Unknown property key in group: everything under one null key.
		`MATCH (m:Message) RETURN m.nope AS k, count(m) AS n`,
		// Selective range: the window enumeration must agree with the
		// label sweep (narrow band, exclusive/inclusive mix).
		`MATCH (m:Message) WHERE m.creationDate >= zoned_datetime('1970-01-01') AND m.creationDate < zoned_datetime('1970-01-02')
		 RETURN count(m) AS c`,
		`MATCH (m:Message) WHERE m.length > 190 RETURN m.length AS l, count(m) AS n
		 NEXT RETURN l, n ORDER BY l`,
	}
	for i, q := range queries {
		before := colAggFired
		fused, general := runBoth(t, g, q)
		if fmt.Sprint(fused) != fmt.Sprint(general) {
			t.Errorf("query %d diverged:\nfused:   %v\ngeneral: %v", i, fused, general)
		}
		if colAggFired == before {
			t.Errorf("query %d never took the fused path (vacuous differential)", i)
		}
	}
}

// TestColumnarAggOptionalMatchesGeneral pins the OPTIONAL spelling on the
// same fused kernel (task 101's lesson inverted: this gate declined
// OPTIONAL where rustychickpeas' declined plain -- either way the walk is
// identical and the difference is one null-fill flag). Every query runs
// fused vs general AND asserts the kernel fired -- a duration cannot be
// asserted on a shared box, a counter can. The empty-scan shapes pin the
// divergence table: grouped plain MATCH emits zero rows, grouped OPTIONAL
// one null-key row; global star counts the fill row (1), value aggregates
// read their identity from it.
func TestColumnarAggOptionalMatchesGeneral(t *testing.T) {
	g := colAggFixture(t)
	queries := []string{
		// Non-empty: both spellings identical, both fused.
		`OPTIONAL MATCH (m:Message) RETURN count(m) AS c`,
		`OPTIONAL MATCH (m:Message) LET year = m.creationDate.year
		 RETURN year, count(m) AS n NEXT RETURN year, n ORDER BY n, year`,
		// Empty scan, global: count(m) identity 0, count(*) counts the fill row.
		`OPTIONAL MATCH (m:NoSuchLabel) RETURN count(m) AS c`,
		`OPTIONAL MATCH (m:NoSuchLabel) RETURN count(*) AS c`,
		`OPTIONAL MATCH (m:NoSuchLabel) RETURN sum(m.length) AS s`,
		// Empty scan, grouped: one null-key row (plain MATCH: zero rows).
		`OPTIONAL MATCH (m:NoSuchLabel) RETURN m.length AS l, count(m) AS n`,
		`OPTIONAL MATCH (m:NoSuchLabel)
		 RETURN m.length AS l, count(*) AS c, count(m) AS n, sum(m.length) AS s`,
		// Stage WHERE killing every candidate behaves as an empty scan.
		`OPTIONAL MATCH (m:Message) WHERE m.length > 100000
		 RETURN m.length AS l, count(m) AS n`,
		`OPTIONAL MATCH (m:Message) WHERE m.length > 100000 RETURN count(*) AS c`,
		// Plain-MATCH grouped empty stays zero rows through the fused path.
		`MATCH (m:NoSuchLabel) RETURN m.length AS l, count(m) AS n`,
	}
	for i, q := range queries {
		before := colAggFired
		fused, general := runBoth(t, g, q)
		if fmt.Sprint(fused) != fmt.Sprint(general) {
			t.Errorf("query %d diverged:\nfused:   %v\ngeneral: %v", i, fused, general)
		}
		if colAggFired == before {
			t.Errorf("query %d never took the fused path (vacuous differential)", i)
		}
	}
}

var _ = value.Null

// TestFlipCmp covers mirroring a comparison across swapped operands, with
// symmetric operators left unchanged and the flip an involution.
func TestFlipCmp(t *testing.T) {
	for in, want := range map[ast.BinOp]ast.BinOp{
		ast.OpLt:  ast.OpGt,
		ast.OpGt:  ast.OpLt,
		ast.OpLte: ast.OpGte,
		ast.OpGte: ast.OpLte,
		ast.OpEq:  ast.OpEq,  // symmetric: unchanged
		ast.OpNeq: ast.OpNeq, // symmetric: unchanged
	} {
		if got := flipCmp(in); got != want {
			t.Fatalf("flipCmp(%v) = %v, want %v", in, got, want)
		}
	}
	// Flipping an ordering comparison twice is the identity.
	for _, op := range []ast.BinOp{ast.OpLt, ast.OpGte, ast.OpGt, ast.OpLte} {
		if flipCmp(flipCmp(op)) != op {
			t.Fatalf("flipCmp not an involution at %v", op)
		}
	}
}

// TestColAggPropValue covers the boxed typed column reader for aggregate
// arguments: an i64 column reads Int (Null for an absent value), an f64
// column reads Float, a non-numeric column is not aggregatable (nil, false),
// and an unindexed key folds every id to Null.
func TestColAggPropValue(t *testing.T) {
	b := chickpeas.NewBuilder(8, 0)
	n0, _ := b.AddNode("N")
	_ = b.SetProp(n0, "age", int64(30))
	_ = b.SetProp(n0, "score", 2.5)
	_ = b.SetProp(n0, "name", "alice")
	n1, _ := b.AddNode("N") // no properties set
	g := b.Finalize("age", "score", "name")

	// An i64 column reads Int for a set value and Null for an absent one.
	rInt, ok := colAggPropValue(g, "age")
	if !ok {
		t.Fatal("an i64 column should resolve")
	}
	if v, _ := rInt(uint32(n0)).AsInt(); v != 30 {
		t.Fatalf("age(n0) = %v, want 30", rInt(uint32(n0)))
	}
	if !rInt(uint32(n1)).IsNull() {
		t.Fatalf("age(n1) = %v, want Null (absent)", rInt(uint32(n1)))
	}

	// An f64 column reads Float.
	rF, ok := colAggPropValue(g, "score")
	if !ok {
		t.Fatal("an f64 column should resolve")
	}
	if v, _ := rF(uint32(n0)).AsFloat(); v != 2.5 {
		t.Fatalf("score(n0) = %v, want 2.5", rF(uint32(n0)))
	}

	// A non-numeric (string) column is not an aggregatable numeric read.
	if r, ok := colAggPropValue(g, "name"); ok || r != nil {
		t.Fatalf("string column: r-nil=%v ok=%v, want (nil, false)", r == nil, ok)
	}

	// An unindexed key resolves to a reader that folds every id to Null.
	rMiss, ok := colAggPropValue(g, "missing")
	if !ok || rMiss == nil || !rMiss(uint32(n0)).IsNull() {
		t.Fatalf("unindexed key ok=%v, want a non-nil Null-folding reader", ok)
	}
}
