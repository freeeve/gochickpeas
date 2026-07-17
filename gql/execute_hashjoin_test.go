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
	// p1 is in f0 (one MEM), f4 (one MEM), and f9 (NINE parallel MEMs,
	// authored by BOTH a9 and a10 -- two payload rows under one key), and
	// reached by two bb's (b1, b7): (1 + 1 + 9*2) * 2 = 40 rows. Parallel
	// relationships carry per-rel multiplicity through probe and nested
	// paths alike (task 144's multigraph consistency fix).
	if len(rows) != 40 {
		t.Fatalf("row count = %d, want 40: %v", len(rows), rows)
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

func TestHashJoinValueKeyWithinScope(t *testing.T) {
	g := hashJoinGraph(t)
	// No connecting expand between the branches -- but the value predicate
	// relating them IS the key now: the equality's branch side keys the
	// build, its outer side probes. (This pinned the opposite before the
	// value-keyed join existed: the shape used to nested-loop.)
	hjCompare(t, g,
		"MATCH (t:Tag {name: 't'}), (t)-[:BT]->(bb:BB)-[:BC]->(p:P), (t)-[:AT]->(a:A)-[:AF]->(f:F) WHERE f.v = p.v RETURN a.name AS an, p.name AS pn",
		true)
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
	// (n0,n1) once per parallel M (16) plus (n2,n0) once = 17; the
	// self-loop pairs (x == y reuses the R pair) stay rejected. Task 144:
	// parallel rels carry per-rel multiplicity through the probe.
	if len(rows) != 17 {
		t.Fatalf("rows = %v, want 17 (16 parallel (n0,n1) + (n2,n0); self-loop rows uniqueness-rejected)", rows)
	}
	for _, r := range rows {
		if strings.Contains(r, "n0|   n0") || strings.Contains(r, "n2|   n2") {
			t.Fatalf("self-loop row leaked: %v", r)
		}
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

// valueJoinGraph: two DISCONNECTED components -- Persons and Accounts --
// joined only by an email equality. Duplicate emails on both sides pin
// multiplicity (2 persons x 2 accounts on dup@x = 4 rows), nulls on both
// sides pin never-match, and an int/float pair on the `num` property pins
// the key encoding's numeric canonicalization (int prop 1 must join
// float prop 1.0 across two typed columns, exactly as `=` coerces).
func valueJoinGraph(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(64, 8)
	person := func(name, email string, num value.Value) {
		t.Helper()
		n, err := b.AddNode("Person")
		if err != nil {
			t.Fatal(err)
		}
		_ = b.SetProp(n, "name", name)
		if email != "" {
			_ = b.SetProp(n, "email", email)
		}
		if i, ok := num.AsInt(); ok {
			_ = b.SetProp(n, "ni", i)
		}
	}
	account := func(name, email string, num value.Value) {
		t.Helper()
		n, err := b.AddNode("Account")
		if err != nil {
			t.Fatal(err)
		}
		_ = b.SetProp(n, "name", name)
		if email != "" {
			_ = b.SetProp(n, "email", email)
		}
		if f, ok := num.AsFloat(); ok && num.Kind() == value.KindFloat {
			_ = b.SetProp(n, "nf", f)
		}
	}
	person("p1", "a@x", value.Null())
	person("p2", "dup@x", value.Null())
	person("p3", "dup@x", value.Null())
	person("p4", "", value.Null()) // null email: never joins
	person("p5", "unmatched@x", value.Null())
	person("pInt", "", value.Int(1))
	account("a1", "a@x", value.Null())
	account("a2", "dup@x", value.Null())
	account("a3", "dup@x", value.Null())
	account("a4", "", value.Null()) // null email: never joins
	account("aFloat", "", value.Float(1.0))
	// Filler accounts so the account scan is the fanning branch.
	for i := range 30 {
		account(fmt.Sprintf("filler%d", i), fmt.Sprintf("f%d@x", i), value.Null())
	}
	return b.Finalize("valuejoin-fixture")
}

// TestValueHashJoinDisconnectedComponents pins task 108's value-keyed
// join: a WHERE equality between two disconnected components keys the
// hash table (no connecting expand exists), and the joined multiset is
// exactly the nested loop's -- duplicates multiplied, nulls dropped.
func TestValueHashJoinDisconnectedComponents(t *testing.T) {
	g := valueJoinGraph(t)
	rows := hjCompare(t, g,
		"MATCH (p:Person), (a:Account) WHERE p.email = a.email RETURN p.name AS pn, a.name AS an",
		true)
	// 1 (a@x) + 4 (dup@x cross product) = 5 rows; nulls and unmatched drop.
	if len(rows) != 5 {
		t.Fatalf("joined rows = %d, want 5: %v", len(rows), rows)
	}
}

// TestValueHashJoinNumericCoercion pins the key encoding against the
// equality's numeric coercion: int 1 joins float 1.0.
func TestValueHashJoinNumericCoercion(t *testing.T) {
	g := valueJoinGraph(t)
	rows := hjCompare(t, g,
		"MATCH (p:Person), (a:Account) WHERE p.ni = a.nf RETURN p.name AS pn, a.name AS an",
		true)
	if len(rows) != 1 || !strings.Contains(rows[0], "pInt") || !strings.Contains(rows[0], "aFloat") {
		t.Fatalf("coercion join = %v, want the single pInt/aFloat row", rows)
	}
}

// TestValueHashJoinReversedSides pins the discovery when the equality is
// written outer-side first, and with an expression key on one side.
func TestValueHashJoinReversedSides(t *testing.T) {
	g := valueJoinGraph(t)
	hjCompare(t, g,
		"MATCH (p:Person), (a:Account) WHERE a.email = p.email RETURN p.name AS pn, a.name AS an",
		true)
	hjCompare(t, g,
		"MATCH (p:Person), (a:Account) WHERE p.email + 'z' = a.email + 'z' RETURN p.name AS pn, a.name AS an",
		true)
}

// TestHashJoinUniquenessReplayDeepHop is task 114's "case B": the type the
// clause repeats sits on the build chain's SECOND hop, so the DEEP
// captured pair is what rejects the conflicting row -- a clause only
// tracks a type that actually repeats in it, and a test whose repetition
// touches only hop 1 leaves hop 2's capture handling deletable (the trap
// rustychickpeas shipped and caught late). Expected row hand-derived: the
// self-MEM route reuses M(h1,h2) in both branches and must be rejected;
// only (o=h1, w=h2, y=h3) survives.
func TestHashJoinUniquenessReplayDeepHop(t *testing.T) {
	b := chickpeas.NewBuilder(32, 64)
	tag, _ := b.AddNode("Tag")
	_ = b.SetProp(tag, "name", "t")
	mk := func(name string) chickpeas.NodeID {
		n, _ := b.AddNode("N")
		_ = b.SetProp(n, "name", name)
		return n
	}
	h1, h2, h3 := mk("h1"), mk("h2"), mk("h3")
	// u0:U repeats type A against the build's FIRST hop without ever
	// colliding on endpoints (U is disjoint from the x side), so the build
	// captures TWO pairs -- A(t,x) at index 0, M(x,y) at index 1 -- and the
	// M conflict is rejected by the DEEP entry alone.
	u0, _ := b.AddNode("U")
	_ = b.SetProp(u0, "name", "u0")
	b.AddRel(tag, u0, "A")
	b.AddRel(tag, h1, "A1") // outer branch arm: t -> h1
	b.AddRel(tag, h1, "A")  // build branch arm: t -> h1
	b.AddRel(h1, h2, "M")   // shared second hop: used by outer (o->w) AND build (x->y)
	b.AddRel(h1, h3, "M")
	b.AddRel(h2, h2, "MEM") // closes onto the y whose build row reused M(h1,h2)
	for range 16 {
		b.AddRel(h2, h3, "MEM") // parallel: keeps the connecting hop expensive to chain
	}
	// Width fillers: both branches fan (the pivot gate needs a multiply),
	// but no filler w has a MEM edge, so the expected output stays the one
	// hand-derived row.
	for i := range 3 {
		o, _ := b.AddNode("N")
		_ = b.SetProp(o, "name", fmt.Sprintf("o%d", i))
		b.AddRel(tag, o, "A1")
		x, _ := b.AddNode("N")
		_ = b.SetProp(x, "name", fmt.Sprintf("x%d", i))
		b.AddRel(tag, x, "A")
		for j := range 2 {
			w, _ := b.AddNode("N")
			_ = b.SetProp(w, "name", fmt.Sprintf("w%d_%d", i, j))
			b.AddRel(o, w, "M")
			y, _ := b.AddNode("N")
			_ = b.SetProp(y, "name", fmt.Sprintf("y%d_%d", i, j))
			b.AddRel(x, y, "M")
		}
	}
	g := b.Finalize("hashjoin-deep-uniq-fixture")

	rows := hjCompare(t, g,
		"MATCH (t:Tag {name: 't'}), (t)-[:A]->(u:U), (t)-[:A1]->(o:N)-[:M]->(w:N), (t)-[:A]->(x:N)-[:M]->(y:N), (w)-[:MEM]->(y) RETURN u.name AS un, o.name AS on, w.name AS wn, y.name AS yn",
		true)
	// The one logical row appears once per parallel (h2,h3) MEM (16); the
	// self-MEM route (reusing the deep M(h1,h2) pair) stays rejected --
	// every row must be the h3 row (task 144 per-rel multiplicity).
	if len(rows) != 16 {
		t.Fatalf("rows = %v, want 16 h3 rows (the self-MEM route reuses the deep M(h1,h2) pair and must be rejected)", rows)
	}
	for _, r := range rows {
		if !strings.Contains(r, "h3") {
			t.Fatalf("non-h3 row leaked (deep-pair reuse): %v", r)
		}
	}
}

// TestHashJoinUniquenessOrderInversion pins the post-extraction re-mark
// plus the probe-vs-build-row pair check (task 194): the connecting
// expand the rewrite consumes as its probe can MOVE ahead of a same-type
// hop left behind the join, inverting the Check/Contribute flags the
// nested order justified -- the probe (stale Check-only) pushed nothing
// while the hop now running after it (stale Contribute-only) was checked
// by nobody, so a row binding one physical relationship for BOTH hops
// leaked. Found on BI Q17 when an anchor-choice change produced the
// inverted stitching: one forum's membership edge served both person
// hops. The fixture mirrors that shape: the q-chain reads the outer f
// and the build-payload bb, so it must run post-join, and q = p1 reuses
// the probe's (f, p1) MEM pair.
func TestHashJoinUniquenessOrderInversion(t *testing.T) {
	b := chickpeas.NewBuilder(64, 128)
	tag, _ := b.AddNode("Tag")
	_ = b.SetProp(tag, "name", "t")
	mk := func(label string, i int) chickpeas.NodeID {
		n, _ := b.AddNode(label)
		_ = b.SetProp(n, "name", fmt.Sprintf("%s%d", strings.ToLower(label), i))
		return n
	}
	var fs, ps []chickpeas.NodeID
	for i := range 11 {
		a := mk("A", i)
		b.AddRel(tag, a, "AT")
		fi := min(i, 9)
		if fi == len(fs) {
			fs = append(fs, mk("F", fi))
		}
		b.AddRel(a, fs[fi], "AF")
	}
	var bbs []chickpeas.NodeID
	for j := range 8 {
		bb := mk("BB", j)
		b.AddRel(tag, bb, "BT")
		bbs = append(bbs, bb)
		pj := j
		if j == 7 {
			pj = 1
		}
		if pj == len(ps) {
			ps = append(ps, mk("P", pj))
		}
		b.AddRel(bb, ps[pj], "BC")
	}
	for range 9 {
		b.AddRel(fs[9], ps[1], "MEM")
	}
	b.AddRel(fs[4], ps[1], "MEM")
	b.AddRel(fs[0], ps[1], "MEM")
	// The q hop's alternative target: each MEM forum reaches p8 (nine
	// parallel rels apiece, keeping the global MEM fan-out high enough
	// that the reorderer still nests the branches instead of chaining
	// through the reversed MEM hop), and both member bb's BC2 both p1
	// (the reuse bait) and p8 (the survivor).
	p8 := mk("P", 8)
	for _, fi := range []int{0, 4, 9} {
		for range 9 {
			b.AddRel(fs[fi], p8, "MEM")
		}
	}
	for _, j := range []int{1, 7} {
		b.AddRel(bbs[j], ps[1], "BC2")
		b.AddRel(bbs[j], p8, "BC2")
	}
	g := b.Finalize("hashjoin-uniq-order-fixture")

	rows := hjCompare(t, g,
		"MATCH (t:Tag {name: 't'}), (t)-[:BT]->(bb:BB)-[:BC]->(p:P), (t)-[:AT]->(a:A)-[:AF]->(f:F), (f)-[:MEM]->(p), (f)-[:MEM]->(q:P)<-[:BC2]-(bb) RETURN a.name AS an, bb.name AS bn, p.name AS pn, q.name AS qn",
		true)
	// The probe rows are the base fixture's 40 (f0 + f4 + 9-parallel f9,
	// each x {b1, b7}), each times nine parallel MEM rels to p8; q must
	// be p8 on every one -- q = p1 would reuse the probe's (f, p1) MEM
	// pair (parallel rels collapse to one key, the documented multigraph
	// deviation) and pairwise distinctness is order-independent, so both
	// executions must reject it.
	if len(rows) != 360 {
		t.Fatalf("rows = %d, want 360 (40 probe rows x 9 parallel MEMs to p8; q=p1 reuses the probe's MEM pair)", len(rows))
	}
	for _, r := range rows {
		if !strings.Contains(r, "p8") {
			t.Fatalf("pair-reusing row leaked (q != p8): %v", r)
		}
	}
}

// TestCartesianBuildOnce pins 130 piece 1: a genuinely disconnected
// component with NO relating predicate joins keylessly -- the build
// materializes once and every build row emits per outer row. The output
// multiset is the same product the nested loop produced (pinned by the
// differential); the win is the branch's scan not re-running per row.
func TestCartesianBuildOnce(t *testing.T) {
	g := valueJoinGraph(t)
	rows := hjCompare(t, g,
		"MATCH (p:Person), (a:Account) RETURN p.name AS pn, a.name AS an",
		true)
	// 6 persons x 35 accounts, every pair.
	if len(rows) != 6*35 {
		t.Fatalf("cartesian rows = %d, want %d", len(rows), 6*35)
	}
}

// TestMultiPivotExtraction pins 130 piece 2: a segment holding TWO
// independent multiplying branches extracts BOTH (the old pass stopped
// at the first success despite its own comment). Three genuinely
// disconnected components force it -- the reorderer cannot chain them,
// so the first pass extracts one cartesian and the second pass the
// other; the output is the full 2x3x4 product either way.
func TestMultiPivotExtraction(t *testing.T) {
	b := chickpeas.NewBuilder(16, 4)
	mk := func(label, name string) {
		n, _ := b.AddNode(label)
		_ = b.SetProp(n, "name", name)
	}
	for i := range 2 {
		mk("A", fmt.Sprintf("a%d", i))
	}
	for i := range 3 {
		mk("B", fmt.Sprintf("b%d", i))
	}
	for i := range 4 {
		mk("C", fmt.Sprintf("c%d", i))
	}
	g := b.Finalize("multipivot")

	q := "MATCH (a:A), (bb:B), (c:C) RETURN a.name AS an, bb.name AS bn, c.name AS cn"
	nested := hjRowKeys(t, g, q)
	if len(nested) != 24 {
		t.Fatalf("nested product = %d, want 24", len(nested))
	}
	mr, ff, ed := plan.HashJoinMinRows, plan.HashJoinFanFactor, plan.HashJoinExtDivisor
	plan.HashJoinMinRows, plan.HashJoinFanFactor, plan.HashJoinExtDivisor = 0, 2, 4
	defer func() {
		plan.HashJoinMinRows, plan.HashJoinFanFactor, plan.HashJoinExtDivisor = mr, ff, ed
	}()
	pl, err := Explain(g, q+" ")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(pl, "HashJoin"); n != 2 {
		t.Fatalf("HashJoin count = %d, want 2 (both cartesians extract):\n%s", n, pl)
	}
	joined := hjRowKeys(t, g, q+" ")
	if !slices.Equal(nested, joined) {
		t.Fatalf("multiset divergence:\nnested: %v\njoined: %v", nested, joined)
	}
}

// TestExactDegreeGate pins the pivot gate's exact-degree pricing (task
// 134): a hop from a slot proven to hold one concrete node prices at that
// node's REAL degree, so the same query fires the extraction anchored on
// a hub and declines it anchored on a sparse node -- with the type average
// (~2 here) lying below the gate for both. The components join by VALUE
// so the reorderer has no edge to chain through and the gate decision is
// isolated. Suppressing the resolution (mutation) must turn the hub case
// red while the sparse control stays green.
func TestExactDegreeGate(t *testing.T) {
	b := chickpeas.NewBuilder(512, 512)
	must := func(id chickpeas.NodeID, err error) chickpeas.NodeID {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	// Anchors: one hub (64 R-edges), one sparse (1), 63 fillers (1 each)
	// -- avg R degree over anchors ~= 2, far under the hub's real 64.
	addW := func() chickpeas.NodeID {
		w := must(b.AddNode("W"))
		_ = b.SetProp(w, "wk", int64(7))
		return w
	}
	hub := must(b.AddNode("HAnchor"))
	_ = b.SetProp(hub, "hk", int64(1))
	for range 64 {
		b.AddRel(hub, addW(), "R")
	}
	sparse := must(b.AddNode("HAnchor"))
	_ = b.SetProp(sparse, "hk", int64(2))
	b.AddRel(sparse, addW(), "R")
	for i := range 63 {
		f := must(b.AddNode("HAnchor"))
		_ = b.SetProp(f, "hk", int64(10+i))
		b.AddRel(f, addW(), "R")
	}
	// Outer chain: 16 rows, value-joined to the arm (every wk matches
	// every mk, so counts are exact products).
	for range 16 {
		o := must(b.AddNode("O"))
		m := must(b.AddNode("M"))
		_ = b.SetProp(m, "mk", int64(7))
		b.AddRel(o, m, "C")
	}
	g := b.Finalize("exactdegree")

	q := func(hk int) string {
		return fmt.Sprintf("MATCH (a:HAnchor {hk: %d}) MATCH (o:O)-[:C]->(m:M) MATCH (a)-[:R]->(w:W) WHERE w.wk = m.mk RETURN count(*) AS n", hk)
	}
	mr, ff, ed := plan.HashJoinMinRows, plan.HashJoinFanFactor, plan.HashJoinExtDivisor
	plan.HashJoinMinRows, plan.HashJoinFanFactor, plan.HashJoinExtDivisor = 0, 8, 4
	defer func() {
		plan.HashJoinMinRows, plan.HashJoinFanFactor, plan.HashJoinExtDivisor = mr, ff, ed
	}()
	for _, tc := range []struct {
		hk       int
		wantJoin bool
		wantN    int64
	}{
		{1, true, 16 * 64}, // hub: real degree 64 clears the x8 gate
		{2, false, 16 * 1}, // sparse: real degree 1 declines it
	} {
		pl, err := Explain(g, q(tc.hk))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(pl, "HashJoin"); got != tc.wantJoin {
			t.Fatalf("hk=%d HashJoin = %v, want %v:\n%s", tc.hk, got, tc.wantJoin, pl)
		}
		rows, err := Run(g, q(tc.hk))
		if err != nil {
			t.Fatal(err)
		}
		r, ok := rows.Next()
		if !ok {
			t.Fatal("no row")
		}
		if n, _ := func() (int64, bool) { v, _ := r.GetAt(0); return v.AsInt() }(); n != tc.wantN {
			t.Fatalf("hk=%d count = %d, want %d", tc.hk, n, tc.wantN)
		}
	}
}
