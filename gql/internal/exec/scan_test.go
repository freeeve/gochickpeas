package exec

import (
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/parser"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// TestNodeIDSeekValue covers the id-seek value resolution: a non-negative
// integer within the CSR id space resolves (including a sparse high id with
// no node present -- the id space, not the node count, is the bound), an
// integral float resolves through its int twin, while a negative id, an
// out-of-space id, a non-integral float, and a non-number all decline.
func TestNodeIDSeekValue(t *testing.T) {
	// One node at id 10 -> the id space spans 0..10 with a single node, so
	// the space exceeds the node count.
	b := chickpeas.NewBuilder(16, 0)
	if _, err := b.AddNodeWithID(10, "N"); err != nil {
		t.Fatal(err)
	}
	sg := graph.New(b.Finalize("idseek"))
	ctx := &eval.Ctx{G: sg}
	space := int64(sg.IDSpace())
	if space <= 10 {
		t.Fatalf("id space = %d, want > 10", space)
	}

	// In-space ids resolve, including a sparse id with no node behind it
	// (existence is the caller's job; the seek only bounds-checks).
	for _, id := range []int64{0, 5, 10, space - 1} {
		got, ok := nodeIDSeekValue(ctx, value.Int(id))
		if !ok || got != graph.NodeID(id) {
			t.Fatalf("id %d = %d,%v, want %d,true", id, got, ok, id)
		}
	}

	// An integral float resolves through its int twin (params spell ids
	// as floats; equality coerces, so the seek must too).
	if got, ok := nodeIDSeekValue(ctx, value.Float(3.0)); !ok || got != graph.NodeID(3) {
		t.Fatalf("float 3.0 = %d,%v, want 3,true", got, ok)
	}

	// A negative id, an id at/beyond the space, a non-integral float, and
	// a non-number decline.
	for _, v := range []value.Value{
		value.Int(-1),
		value.Int(space),
		value.Int(space + 1000),
		value.Str("7"),
		value.Float(3.5),
		value.Float(-1.0),
		value.Null(),
	} {
		if _, ok := nodeIDSeekValue(ctx, v); ok {
			t.Fatalf("%+v should not resolve to an id seek", v)
		}
	}
}

// TestExistsSeedCandidates covers the EXISTS-seed backward candidate walk:
// from the bound anchor it enumerates the nodes reachable over each seed
// chain's hops, filtered by the per-level and final matchers, and reports
// success. Fixture: anchor 0 with outgoing R edges to 1, 2, 3.
func TestExistsSeedCandidates(t *testing.T) {
	bld := chickpeas.NewBuilder(8, 8)
	for range 4 {
		if _, err := bld.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []int{1, 2, 3} {
		if _, err := bld.AddRel(graph.NodeID(0), graph.NodeID(d), "R"); err != nil {
			t.Fatal(err)
		}
	}
	sg := graph.New(bld.Finalize())
	ctx := &eval.Ctx{G: sg}
	rmR := sg.CompileRelMatcher([]string{"R"})
	mAll := sg.CompileNodeMatcher(nil, nil)

	op := &plan.BindOp{Source: plan.ScanSource{
		Kind:  plan.ScanExistsSeed,
		Seeds: []plan.SeedChain{{AnchorSlot: 0, Hops: []plan.SeedHop{{Dir: graph.Outgoing}}}},
	}}
	seedRel := [][]*graph.RelMatcher{{rmR}}
	seedNode := [][]*graph.NodeMatcher{{mAll}}
	row := []value.Value{value.Node(graph.NodeID(0))}

	var cand []graph.NodeID
	var scr genScratch
	if ok := existsSeedCandidates(ctx, op, mAll, seedRel, seedNode, row, &cand, &scr); !ok {
		t.Fatal("existsSeedCandidates should succeed under the fan-out cap")
	}
	got := map[graph.NodeID]bool{}
	for _, n := range cand {
		got[n] = true
	}
	if len(got) != 3 || !got[1] || !got[2] || !got[3] {
		t.Fatalf("seed candidates = %v, want {1,2,3}", cand)
	}
}

// TestFreshScanKinds drives the scan-source variants end-to-end: an inline
// property anchors on the value index (ScanProperty), an unlabeled pattern
// scans every node (ScanAll), id(n) = k seeks a single node (ScanNodeID), and
// a substring predicate anchors via the text path (ScanTextMatch).
func TestFreshScanKinds(t *testing.T) {
	bld := chickpeas.NewBuilder(8, 0)
	a0, _ := bld.AddNode("A")
	_ = bld.SetProp(a0, "v", int64(10))
	_ = bld.SetProp(a0, "name", "alice")
	a1, _ := bld.AddNode("A")
	_ = bld.SetProp(a1, "v", int64(20))
	_ = bld.SetProp(a1, "name", "bob")
	g := graph.New(bld.Finalize("v", "name"))
	ctx := &eval.Ctx{G: g}

	run := func(src string) [][]value.Value {
		t.Helper()
		q, err := parser.Parse(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		p, err := plan.Build(q, g)
		if err != nil {
			t.Fatalf("plan %q: %v", src, err)
		}
		rows, err := Execute(ctx, p)
		if err != nil {
			t.Fatalf("exec %q: %v", src, err)
		}
		return rows
	}

	if rows := run("MATCH (a:A {v: 20}) RETURN a"); len(rows) != 1 {
		t.Fatalf("property scan rows = %d, want 1", len(rows))
	}
	if rows := run("MATCH (n) RETURN n"); len(rows) != 2 {
		t.Fatalf("all scan rows = %d, want 2", len(rows))
	}
	if rows := run("MATCH (n) WHERE id(n) = 0 RETURN n"); len(rows) != 1 {
		t.Fatalf("id-seek rows = %d, want 1", len(rows))
	}
	if rows := run("MATCH (a:A) WHERE a.name CONTAINS 'li' RETURN a"); len(rows) != 1 {
		t.Fatalf("text-match rows = %d, want 1 (alice)", len(rows))
	}
}

// TestExistsSeedScanExec drives an EXISTS-seeded scan end-to-end: with `a`
// bound to a concrete node, the scan of `b` is seeded backward through the
// EXISTS pattern's hop rather than scanning the whole label, so compileStage
// builds the seed matchers and the executor walks them.
func TestExistsSeedScanExec(t *testing.T) {
	bld := chickpeas.NewBuilder(8, 8)
	var ps []graph.NodeID
	for i := 0; i < 4; i++ {
		p, err := bld.AddNode("Person")
		if err != nil {
			t.Fatal(err)
		}
		_ = bld.SetProp(p, "pid", int64(i))
		ps = append(ps, p)
	}
	// p3 KNOWS p1: the only KNOWS edge out of the pid=3 anchor.
	if _, err := bld.AddRel(ps[3], ps[1], "KNOWS"); err != nil {
		t.Fatal(err)
	}
	g := graph.New(bld.Finalize("pid"))
	ctx := &eval.Ctx{G: g}

	q, err := parser.Parse("MATCH (b:Person) MATCH (a:Person {pid: 3}) WHERE EXISTS { MATCH (a)-[:KNOWS]->(b) } RETURN b")
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(q, g)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Execute(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	// b is seeded from a=p3's single KNOWS neighbor, p1.
	if len(rows) != 1 {
		t.Fatalf("exists-seed rows = %d, want 1", len(rows))
	}
	if bv, _ := rows[0][0].AsNode(); uint32(bv) != uint32(ps[1]) {
		t.Fatalf("b = %v, want p1", bv)
	}
}

// numericSeekFixture stores age as an int and score as a float on separate
// Person nodes, plus a noise node with near-miss values on both keys, so a
// seek that returns everything (or nothing) is caught either way.
func numericSeekFixture(t *testing.T) graph.Graph {
	t.Helper()
	b := chickpeas.NewBuilder(8, 0)
	intNode, err := b.AddNode("Person")
	if err != nil {
		t.Fatal(err)
	}
	b.SetProp(intNode, "age", int64(30))
	floatNode, err := b.AddNode("Person")
	if err != nil {
		t.Fatal(err)
	}
	b.SetProp(floatNode, "score", 7.0)
	noise, err := b.AddNode("Person")
	if err != nil {
		t.Fatal(err)
	}
	b.SetProp(noise, "age", int64(31))
	b.SetProp(noise, "score", 7.5)
	return graph.New(b.Finalize("numseek"))
}

// TestPropSeekNumericTwin locks the numeric-twin probe on the single-value
// property seek (task 309): the property index matches stored values
// exactly while equality coerces int against float, so without the twin a
// float literal over an int-stored property (and the reverse) silently
// lost every row. The unlabeled spelling never seeks (matcher-only), so it
// is the reference the two seek spellings must agree with, both directions.
func TestPropSeekNumericTwin(t *testing.T) {
	g := numericSeekFixture(t)

	// Engagement: the labeled inline spelling anchors on the property seek
	// (the shape under test -- if this drifts, the test is testing nothing).
	qq, err := parser.Parse("MATCH (p:Person {age: 30.0}) RETURN p")
	if err != nil {
		t.Fatal(err)
	}
	pl, err := plan.Build(qq, g)
	if err != nil {
		t.Fatal(err)
	}
	ms := pl.Branches[0][0].Stages[0].(*plan.MatchStage)
	if ms.Ops[0].Source.Kind != plan.ScanProperty {
		t.Fatalf("plan anchors on %v, want ScanProperty", ms.Ops[0].Source.Kind)
	}

	cases := []struct {
		name     string
		qs       []string
		want     int
		distinct bool // qs are different queries, not spellings of one
	}{
		{"float literal over int-stored", []string{
			"MATCH (p:Person {age: 30.0}) RETURN p",
			"MATCH (p:Person) WHERE p.age = 30.0 RETURN p",
			"MATCH (p {age: 30.0}) RETURN p",
		}, 1, false},
		{"int literal over float-stored", []string{
			"MATCH (p:Person {score: 7}) RETURN p",
			"MATCH (p:Person) WHERE p.score = 7 RETURN p",
			"MATCH (p {score: 7}) RETURN p",
		}, 1, false},
		// Controls: near-miss literals return nothing, so the agreement
		// above cannot be satisfied by a seek that returns everything.
		{"non-integral float misses int-stored", []string{
			"MATCH (p:Person {age: 30.5}) RETURN p",
			"MATCH (p:Person) WHERE p.age = 30.5 RETURN p",
			"MATCH (p {age: 30.5}) RETURN p",
		}, 0, false},
		{"same-type exact still works", []string{
			"MATCH (p:Person {age: 30}) RETURN p",
			"MATCH (p:Person {score: 7.5}) RETURN p",
		}, 1, true},
	}
	for _, tc := range cases {
		var ref []uint32
		for i, q := range tc.qs {
			ids := runIDs(t, g, q)
			if len(ids) != tc.want {
				t.Errorf("%s: %q = %d rows, want %d", tc.name, q, len(ids), tc.want)
			}
			if i == 0 || tc.distinct {
				ref = ids
				continue
			}
			if len(ids) != len(ref) {
				continue // already reported above
			}
			for j := range ids {
				if ids[j] != ref[j] {
					t.Errorf("%s: %q row %d = node %d, spelling 0 has %d", tc.name, q, j, ids[j], ref[j])
				}
			}
		}
	}
}

// TestIDSeekFloatParam locks the id-seek twin resolution end-to-end (task
// 309): a parameter spelling an id as a float must find the node an int
// parameter finds -- the seek used to decline non-int values outright,
// silently losing the row while the coercing equality would have matched.
func TestIDSeekFloatParam(t *testing.T) {
	b := chickpeas.NewBuilder(8, 0)
	b.AddNode("N")
	b.AddNode("N")
	g := graph.New(b.Finalize("idtwin"))
	q, err := parser.Parse("MATCH (n) WHERE id(n) = $p RETURN n")
	if err != nil {
		t.Fatal(err)
	}
	pl, err := plan.Build(q, g)
	if err != nil {
		t.Fatal(err)
	}
	for _, pv := range []value.Value{value.Int(1), value.Float(1.0)} {
		ctx := &eval.Ctx{G: g, Named: map[string]value.Value{"p": pv}}
		rows, err := Execute(ctx, pl)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Errorf("param %v = %d rows, want 1", pv, len(rows))
			continue
		}
		if id, _ := rows[0][0].AsNode(); uint32(id) != 1 {
			t.Errorf("param %v = node %d, want 1", pv, id)
		}
	}
	// A non-integral float finds nothing (and must not error).
	ctx := &eval.Ctx{G: g, Named: map[string]value.Value{"p": value.Float(1.5)}}
	rows, err := Execute(ctx, pl)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("param 1.5 = %d rows, want 0", len(rows))
	}
}
