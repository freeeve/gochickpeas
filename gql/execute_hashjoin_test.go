// Hash-join extraction fixtures: adversarial shapes comparing the rewrite
// against the nested execution row-for-row -- entangled relationship
// uniqueness across the branches, payload multiplicity, the reversed
// probe orientation, cross-branch WHERE movement, excluded var-expands,
// and OPTIONAL nearby. The rewrite thresholds are lowered per forced run
// so fixture-scale cardinality contrast qualifies; the fixtures' fan-outs
// are shaped so the join reorderer produces the nested two-branch plan
// the rewrite targets (a connecting edge cheap enough to chain through
// would be the better plan, and correctly leaves nothing to extract).
package gql

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

// hjRowKeys runs q and returns the sorted row-key multiset.
func hjRowKeys(t *testing.T, g *chickpeas.Snapshot, q string) []string {
	t.Helper()
	rows, err := Run(g, q)
	if err != nil {
		t.Fatalf("query failed: %s\n%v", q, err)
	}
	var out []string
	for r := range rows.All() {
		var sb strings.Builder
		for _, v := range r.Values() {
			sb.WriteString(value.Key(v))
			sb.WriteByte('|')
		}
		out = append(out, sb.String())
	}
	slices.Sort(out)
	return out
}

// hjCompare runs q nested (default thresholds), then re-plans a textual
// variant with the thresholds lowered, requires the rewrite to fire (or
// not, per wantJoin), and requires identical row multisets. Returns the
// multiset for content assertions.
func hjCompare(t *testing.T, g *chickpeas.Snapshot, q string, wantJoin bool) []string {
	t.Helper()
	nested := hjRowKeys(t, g, q)
	mr, ff, ed := plan.HashJoinMinRows, plan.HashJoinFanFactor, plan.HashJoinExtDivisor
	plan.HashJoinMinRows, plan.HashJoinFanFactor, plan.HashJoinExtDivisor = 0, 2, 4
	defer func() {
		plan.HashJoinMinRows, plan.HashJoinFanFactor, plan.HashJoinExtDivisor = mr, ff, ed
	}()
	qf := q + " " // defeat the plan cache: the lowered thresholds must re-plan
	pl, err := Explain(g, qf)
	if err != nil {
		t.Fatalf("explain failed: %s\n%v", qf, err)
	}
	if got := strings.Contains(pl, "HashJoin"); got != wantJoin {
		t.Fatalf("HashJoin in forced plan = %v, want %v:\n%s", got, wantJoin, pl)
	}
	joined := hjRowKeys(t, g, qf)
	if !slices.Equal(nested, joined) {
		t.Fatalf("row multiset divergence for %s\nnested (%d): %v\njoined (%d): %v\nplan:\n%s",
			q, len(nested), nested, len(joined), joined, pl)
	}
	return nested
}

// hashJoinGraph is the two-branch join fixture: an anchor tag, an 11-wide
// A branch (a->f, with a9 and a10 sharing f9 for payload multiplicity
// under one key), an 8-wide B branch (bb->p, with b1 and b7 sharing p1),
// and MEM edges from three forums into p1 (parallel edges on f9 keep the
// connecting hop expensive enough that the reorderer nests the branches
// instead of chaining through it). OPT edges serve the OPTIONAL fixture.
func hashJoinGraph(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(64, 128)
	tag, _ := b.AddNode("Tag")
	_ = b.SetProp(tag, "name", "t")
	mk := func(label string, i int) chickpeas.NodeID {
		n, _ := b.AddNode(label)
		_ = b.SetProp(n, "name", fmt.Sprintf("%s%d", strings.ToLower(label), i))
		_ = b.SetProp(n, "v", int64(i))
		return n
	}
	var fs, ps []chickpeas.NodeID
	for i := range 11 {
		a := mk("A", i)
		b.AddRel(tag, a, "AT")
		fi := min(i, 9) // a9 and a10 share f9
		if fi == len(fs) {
			fs = append(fs, mk("F", fi))
		}
		b.AddRel(a, fs[fi], "AF")
	}
	for j := range 8 {
		bb := mk("BB", j)
		b.AddRel(tag, bb, "BT")
		pj := j
		if j == 7 {
			pj = 1 // b1 and b7 share p1
		}
		if pj == len(ps) {
			ps = append(ps, mk("P", pj))
		}
		b.AddRel(bb, ps[pj], "BC")
	}
	for range 9 {
		b.AddRel(fs[9], ps[1], "MEM") // parallel members: dedup + fan-out
	}
	b.AddRel(fs[4], ps[1], "MEM")
	b.AddRel(fs[0], ps[1], "MEM")
	q0 := mk("Q", 0)
	b.AddRel(ps[1], q0, "OPT")
	return b.Finalize("hashjoin-fixture")
}

const hjBaseQuery = "MATCH (t:Tag {name: 't'}), (t)-[:BT]->(bb:BB)-[:BC]->(p:P), (t)-[:AT]->(a:A)-[:AF]->(f:F), (f)-[:MEM]->(p)"

func TestHashJoinBasicAndMultiplicity(t *testing.T) {
	g := hashJoinGraph(t)
	rows := hjCompare(t, g, hjBaseQuery+" RETURN a.name AS an, bb.name AS bn, p.name AS pn", true)
	// p1 is in f0, f4, f9 (f9 authored by BOTH a9 and a10 -- two payload
	// rows under one key) and reached by two bb's (b1, b7): 4 x 2 rows.
	if len(rows) != 8 {
		t.Fatalf("row count = %d, want 8: %v", len(rows), rows)
	}
}

func TestHashJoinReversedPatternText(t *testing.T) {
	g := hashJoinGraph(t)
	hjCompare(t, g,
		"MATCH (t:Tag {name: 't'}), (t)-[:BT]->(bb:BB)-[:BC]->(p:P), (t)-[:AT]->(a:A)-[:AF]->(f:F), (p)<-[:MEM]-(f) RETURN a.name AS an, bb.name AS bn, p.name AS pn",
		true)
}

func TestHashJoinCrossBranchWhere(t *testing.T) {
	g := hashJoinGraph(t)
	rows := hjCompare(t, g, hjBaseQuery+" WHERE a.v < bb.v RETURN a.name AS an, bb.name AS bn", true)
	// a in {a0,a4,a9,a10} x bb in {b1,b7} with a.v < bb.v.
	if len(rows) != 3 {
		t.Fatalf("row count = %d, want 3: %v", len(rows), rows)
	}
}

func TestHashJoinOptionalDownstream(t *testing.T) {
	g := hashJoinGraph(t)
	hjCompare(t, g, hjBaseQuery+" OPTIONAL MATCH (p)-[:OPT]->(q:Q) RETURN a.name AS an, p.name AS pn, q.name AS qn", true)
}

func TestHashJoinNoConnectingOpRejected(t *testing.T) {
	g := hashJoinGraph(t)
	// No connecting expand between the branches: only a value predicate
	// relates them, so the rewrite must not fire (no zero-key joins).
	hjCompare(t, g,
		"MATCH (t:Tag {name: 't'}), (t)-[:BT]->(bb:BB)-[:BC]->(p:P), (t)-[:AT]->(a:A)-[:AF]->(f:F) WHERE f.v = p.v RETURN a.name AS an, p.name AS pn",
		false)
}

func TestHashJoinDefaultThresholdsUntouched(t *testing.T) {
	g := hashJoinGraph(t)
	pl, err := Explain(g, hjBaseQuery+" RETURN a.name AS an")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(pl, "HashJoin") {
		t.Fatalf("rewrite fired at fixture scale under default thresholds:\n%s", pl)
	}
}

// TestHashJoinUniquenessReplay pins the capture/replay protocol: the two
// branches traverse the SAME relationship type from the same hub, so the
// standalone build cannot see the outer branch's used pair -- the (x == y)
// rows it materializes must be rejected at probe time or the join would
// emit rows the nested order excludes (the self M-loops make them real).
func TestHashJoinUniquenessReplay(t *testing.T) {
	b := chickpeas.NewBuilder(32, 64)
	tag, _ := b.AddNode("Tag")
	_ = b.SetProp(tag, "name", "t")
	var ns []chickpeas.NodeID
	for i := range 6 {
		n, _ := b.AddNode("N")
		_ = b.SetProp(n, "name", fmt.Sprintf("n%d", i))
		b.AddRel(tag, n, "R")
		ns = append(ns, n)
	}
	for range 16 {
		b.AddRel(ns[0], ns[1], "M") // parallel: keeps the M hop expensive to chain through
	}
	b.AddRel(ns[0], ns[0], "M") // self loop: reachable only if uniqueness is dropped
	b.AddRel(ns[2], ns[2], "M")
	b.AddRel(ns[2], ns[0], "M")
	g := b.Finalize("hashjoin-uniq-fixture")

	rows := hjCompare(t, g,
		"MATCH (t:Tag {name: 't'}), (t)-[:R]->(x:N), (t)-[:R]->(y:N), (x)-[:M]->(y) RETURN x.name AS xn, y.name AS yn",
		true)
	if len(rows) != 2 {
		t.Fatalf("rows = %v, want the two x!=y pairs only (self-loop rows must be uniqueness-rejected)", rows)
	}
}

// TestHashJoinExcludedVarExpand pins the dependency-driven exclusion: the
// branch-side unbounded reach runs over a type the outer branch also
// walks (reach-mode Check), so it must run post-probe -- where the outer
// branch's used pairs are live -- not inside the standalone build. The
// undirected reach from y0 can otherwise cross the (x0, y0) edge the
// outer trail already used and reach a root the nested order excludes.
func TestHashJoinExcludedVarExpand(t *testing.T) {
	b := chickpeas.NewBuilder(64, 128)
	tag, _ := b.AddNode("Tag")
	_ = b.SetProp(tag, "name", "t")
	mk := func(label string, i int) chickpeas.NodeID {
		n, _ := b.AddNode(label)
		_ = b.SetProp(n, "name", fmt.Sprintf("%s%d", strings.ToLower(label), i))
		return n
	}
	var xs, ys []chickpeas.NodeID
	for i := range 6 {
		x := mk("X", i)
		b.AddRel(tag, x, "RA")
		xs = append(xs, x)
	}
	for j := range 8 {
		y := mk("Y", j)
		b.AddRel(tag, y, "RB")
		ys = append(ys, y)
	}
	// A-side C edges: x_i -> y_i (the outer trail's used pairs).
	for i, x := range xs {
		b.AddRel(x, ys[i], "C")
	}
	// y0's only route to a Root crosses the (x0, y0) edge the outer
	// branch used; y5 has a direct root as the surviving control row.
	b.AddRel(xs[0], mk("Root", 0), "C")
	b.AddRel(ys[5], mk("Root", 5), "C")
	for range 16 {
		b.AddRel(xs[0], ys[0], "MEM") // parallel: keeps MEM expensive to chain through
	}
	b.AddRel(xs[3], ys[5], "MEM")
	g := b.Finalize("hashjoin-varexpand-fixture")

	hjCompare(t, g,
		"MATCH (t:Tag {name: 't'}), (t)-[:RA]->(x:X)-[:C]->{0,}(cx:Y), (t)-[:RB]->(y:Y), (y)-[:C]-{0,}(cy:Root), (x)-[:MEM]->(y) RETURN x.name AS xn, y.name AS yn, cy.name AS cn",
		true)
}
