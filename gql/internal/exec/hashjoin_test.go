package exec

import (
	"fmt"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/internal/semantics"
)

// Coverage for the hash-join build table's per-key chain helpers, exercised
// directly rather than through a full join: mintChain/link build each key's
// row chain in insertion order, and headKey/headVal return the chain head
// (-1 when the key is absent).

// TestHJTableExpandKeyChain covers the expand-keyed (node-id) chains.
func TestHJTableExpandKeyChain(t *testing.T) {
	tbl := &hjTable{rows: make([]hjRow, 16)}
	add := func(key graph.NodeID, idx int32) {
		ci := tbl.byKey.GetOrCreate(uint64(key), tbl.mintChain)
		tbl.link(ci, idx)
	}
	add(7, 0)
	add(7, 3) // a second row for key 7 chains after row 0
	add(9, 1)

	if h := tbl.headKey(7); h != 0 {
		t.Fatalf("headKey(7) = %d, want 0 (first linked)", h)
	}
	if tbl.rows[0].next != 3 {
		t.Fatalf("row 0 next = %d, want 3 (insertion order preserved)", tbl.rows[0].next)
	}
	if h := tbl.headKey(9); h != 1 {
		t.Fatalf("headKey(9) = %d, want 1", h)
	}
	if h := tbl.headKey(123); h != -1 {
		t.Fatalf("headKey(absent) = %d, want -1", h)
	}
}

// TestHJTableValueKeyChain covers the value-keyed (encoded byte key) chains.
func TestHJTableValueKeyChain(t *testing.T) {
	tbl := &hjTable{rows: make([]hjRow, 16)}
	add := func(k string, idx int32) {
		ci := tbl.byVal.GetOrCreate([]byte(k), tbl.mintChain)
		tbl.link(ci, idx)
	}
	add("x", 2)
	add("x", 5) // a second row for key x chains after row 2
	add("y", 4)

	if h := tbl.headVal([]byte("x")); h != 2 {
		t.Fatalf("headVal(x) = %d, want 2 (first linked)", h)
	}
	if tbl.rows[2].next != 5 {
		t.Fatalf("row 2 next = %d, want 5 (insertion order preserved)", tbl.rows[2].next)
	}
	if h := tbl.headVal([]byte("y")); h != 4 {
		t.Fatalf("headVal(y) = %d, want 4", h)
	}
	if h := tbl.headVal([]byte("z")); h != -1 {
		t.Fatalf("headVal(absent) = %d, want -1", h)
	}
}

// TestHashJoinSinkExec drives the full hash-join sink pipeline (build,
// push, emitRows, close) through a value-keyed cross-pattern join. The
// plan-shape assertion keeps the differential honest: if the detector
// stops converting this shape, the test fails rather than silently
// covering the nested path.
func TestHashJoinSinkExec(t *testing.T) {
	bld := chickpeas.NewBuilder(64, 0)
	addNode := func(label, name, email string) {
		n, err := bld.AddNode(label)
		if err != nil {
			t.Fatal(err)
		}
		if err := bld.SetProp(n, "name", name); err != nil {
			t.Fatal(err)
		}
		if err := bld.SetProp(n, "email", email); err != nil {
			t.Fatal(err)
		}
	}
	addNode("Person", "p1", "a@x")
	addNode("Person", "p2", "b@x")
	addNode("Account", "a1", "a@x")
	addNode("Account", "a2", "b@x")
	// Filler accounts with distinct emails make the account scan the
	// fanning branch, which is what the cardinality-gated detector
	// converts (cf. valueJoinGraph in the public e2e suite).
	for i := range 30 {
		addNode("Account", fmt.Sprintf("f%d", i), fmt.Sprintf("f%d@x", i))
	}
	g := graph.New(bld.Finalize("name", "email"))
	ctx := &eval.Ctx{G: g}

	// The detector's thresholds are sized for real graphs; the exported
	// knobs exist so tests can force the rewrite onto small fixtures.
	mr, ff, ed := plan.HashJoinMinRows, plan.HashJoinFanFactor, plan.HashJoinExtDivisor
	plan.HashJoinMinRows, plan.HashJoinFanFactor, plan.HashJoinExtDivisor = 0, 2, 4
	defer func() {
		plan.HashJoinMinRows, plan.HashJoinFanFactor, plan.HashJoinExtDivisor = mr, ff, ed
	}()

	q, err := parser.Parse("MATCH (p:Person), (a:Account) WHERE p.email = a.email RETURN p.name AS pn, a.name AS an")
	if err == nil {
		err = semantics.Desugar(q)
	}
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(q, g)
	if err != nil {
		t.Fatal(err)
	}
	joined := false
	for _, br := range p.Branches {
		for _, seg := range br {
			for _, st := range seg.Stages {
				if _, ok := st.(*plan.HashJoinStage); ok {
					joined = true
				}
			}
		}
	}
	if !joined {
		t.Fatal("value-keyed cross-pattern join no longer plans a HashJoinStage; this test must drive the sink path")
	}
	rows, err := Execute(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		pn, _ := r[0].AsStr()
		an, _ := r[1].AsStr()
		got[pn+"/"+an] = true
	}
	if len(got) != 2 || !got["p1/a1"] || !got["p2/a2"] {
		t.Fatalf("joined pairs = %v, want {p1/a1, p2/a2}", got)
	}
}
