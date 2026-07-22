package gql

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// Hash-join extraction edge cases: post-extraction re-mark / pair-reuse
// under order inversion, keyless cartesian build-once, multi-pivot
// extraction of two independent branches, and the pivot gate's
// exact-degree pricing. Split from execute_hashjoin_test.go (which keeps
// the shared fixtures/helpers and the core join tests).

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
