// Payload-gated top-k oracle (task 117): under ORDER BY <col> LIMIT k the
// sink must build the projected payload only for rows the bounded heap
// would keep -- the assertion is a BUILD COUNT, not a duration. Ascending
// input under ASC means the first k rows fill the heap and every later
// candidate is refused on one key comparison: exactly k builds.
package exec

import (
	"fmt"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

func topkFixture(t *testing.T, n int) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(n, 1)
	for i := range n {
		nd, err := b.AddNode("N")
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SetProp(nd, "v", int64(i)); err != nil {
			t.Fatal(err)
		}
		if err := b.SetProp(nd, "name", fmt.Sprintf("n%04d", i)); err != nil {
			t.Fatal(err)
		}
		// tie groups of 10 for the boundary-stability case
		if err := b.SetProp(nd, "grp", int64(i/10)); err != nil {
			t.Fatal(err)
		}
	}
	return b.Finalize("topk")
}

// runGateBoth runs q with the payload gate on, then off, returning both
// row lists in engine order (the fixtures' ORDER BY is the contract
// under test). Both legs are vacuity-checked: the gated leg must build
// payloads through the gate, the ungated leg must not touch it -- a
// switch that failed to reach its mechanism would otherwise compare a
// path against itself forever.
func runGateBoth(t *testing.T, g *chickpeas.Snapshot, q string) (gated, ungated []string) {
	t.Helper()
	before := topkPayloadBuilds
	disableTopkGate = false
	gated, _ = runBothOrdered(t, g, q)
	if topkPayloadBuilds == before {
		t.Fatal("gated leg never built through the gate -- the differential would be vacuous")
	}
	before = topkPayloadBuilds
	disableTopkGate = true
	ungated, _ = runBothOrdered(t, g, q)
	disableTopkGate = false
	if topkPayloadBuilds != before {
		t.Fatal("ungated leg reached the gate -- the off switch is not isolating the mechanism")
	}
	return gated, ungated
}

func TestTopKPayloadGate(t *testing.T) {
	g := topkFixture(t, 500)
	before := topkPayloadBuilds
	disableTopkGate = false
	rows, _ := runBothOrdered(t, g,
		"MATCH (m:N) RETURN m.v AS v, m.name AS name ORDER BY v ASC LIMIT 5")
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(rows))
	}
	builds := topkPayloadBuilds - before
	// runBoth executes twice, so each pass builds exactly 5 payloads:
	// ascending input under ASC fills the heap with the first 5 and
	// refuses all 495 others on one key comparison. ~1000 means ungated.
	if builds != 10 {
		t.Fatalf("payload builds = %d across two runs, want 10 (5 per run; ~1000 means ungated)", builds)
	}
	gated, ungated := runGateBoth(t, g,
		"MATCH (m:N) RETURN m.v AS v, m.name AS name ORDER BY v DESC LIMIT 7")
	if fmt.Sprint(gated) != fmt.Sprint(ungated) {
		t.Fatalf("gated path diverged from unguarded:\n%v\nvs\n%v", gated, ungated)
	}
}

// TestTopKGateTieAtBoundary pins stability where a key tie straddles the
// LIMIT boundary: grp 0 has ten rows and LIMIT cuts at six, so which six
// survive is decided by arrival order -- the gate must refuse tied
// late-arrivals exactly as offer-then-pop would.
func TestTopKGateTieAtBoundary(t *testing.T) {
	g := topkFixture(t, 100)
	gated, ungated := runGateBoth(t, g,
		"MATCH (m:N) RETURN m.grp AS grp, m.name AS name ORDER BY grp ASC LIMIT 6")
	if fmt.Sprint(gated) != fmt.Sprint(ungated) {
		t.Fatalf("tie-at-boundary diverged:\n%v\nvs\n%v", gated, ungated)
	}
}

// TestTopKRowEvalKeyGated pins the row-evaluable key extension (task
// 260, the shape the sibling engine's guard wrongly declined): an ORDER
// BY expression that is NOT an output column but references only matched
// row variables (m.v + 1 over RETURN m.name) now gates -- ascending
// input under DESC means every candidate after the first admissions
// competes, so the build count stays O(k), and rows must equal the
// unguarded reference exactly.
func TestTopKRowEvalKeyGated(t *testing.T) {
	g := topkFixture(t, 500)
	before := topkPayloadBuilds
	disableTopkGate = false
	rows, _ := runBothOrdered(t, g,
		"MATCH (m:N) RETURN m.name AS name ORDER BY m.v + 1 ASC LIMIT 4")
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4", len(rows))
	}
	builds := topkPayloadBuilds - before
	if builds != 8 {
		t.Fatalf("payload builds = %d across two runs, want 8 (4 per run; ~1000 means the row-eval key did not gate)", builds)
	}
	gated, ungated := runGateBoth(t, g,
		"MATCH (m:N) RETURN m.name AS name ORDER BY m.v + 1 DESC LIMIT 4")
	if fmt.Sprint(gated) != fmt.Sprint(ungated) {
		t.Fatalf("row-eval-key path diverged:\n%v\nvs\n%v", gated, ungated)
	}
	// The identity-projection family itself: the entity is the output,
	// the key is its property.
	gated, ungated = runGateBoth(t, g,
		"MATCH (m:N) RETURN m ORDER BY m.v DESC LIMIT 5")
	if fmt.Sprint(gated) != fmt.Sprint(ungated) {
		t.Fatalf("identity-projection path diverged:\n%v\nvs\n%v", gated, ungated)
	}
}

// TestTopKComputedAliasKeyUngated pins the remaining unguarded path: a
// key over a COMPUTED output alias (w is not a matched-row variable, and
// w + 1 is not structurally a projected expression) needs the built row,
// so it keeps the build-then-offer flow and still agrees with the
// general sort.
func TestTopKComputedAliasKeyUngated(t *testing.T) {
	g := topkFixture(t, 100)
	// A decline pin, not a neutrality differential: the shape must not
	// gate on EITHER leg (both run build-then-offer, so the comparison
	// below only guards the switch's no-op-ness on unguarded shapes).
	q := "MATCH (m:N) RETURN m.v + 1 AS w ORDER BY w + 1 DESC LIMIT 4"
	before := topkPayloadBuilds
	disableTopkGate = false
	gated, _ := runBothOrdered(t, g, q)
	disableTopkGate = true
	ungated, _ := runBothOrdered(t, g, q)
	disableTopkGate = false
	if topkPayloadBuilds != before {
		t.Fatal("computed-alias key gated -- the decline pin no longer holds")
	}
	if fmt.Sprint(gated) != fmt.Sprint(ungated) {
		t.Fatalf("computed-alias-key path diverged:\n%v\nvs\n%v", gated, ungated)
	}
}

// TestSPScratchEpochSafety pins the two epoch-stamp traps the
// rustychickpeas 097 port hit (their 126 report): (1) lazily-grown dense
// scratch must never stamp new slots with a value a FUTURE search will
// use as its epoch (zero-fill + epochs that never take 0 is the
// invariant); (2) epoch wraparound clears the stamps rather than
// colliding with survivors.
func TestSPScratchEpochSafety(t *testing.T) {
	scr := newSPScratch()
	fs := scr.begin(4)
	scr.gen[2] = fs // a reached node in the small search
	fs2 := scr.begin(8)
	if fs2 == 0 || fs2+1 == 0 {
		t.Fatal("an epoch took 0, the never-stamped sentinel")
	}
	for i := range 8 {
		if scr.gen[i] == fs2 || scr.gen[i] == fs2+1 {
			t.Fatalf("slot %d reads as reached in a fresh search (phantom stamp)", i)
		}
	}
	// Wraparound: force the counter to the clear threshold and verify
	// survivors from the pre-wrap era cannot alias the new epoch.
	scr.gen[1] = scr.cur // stamp under the current era
	scr.cur = ^uint32(0) - 2
	fs3 := scr.begin(8)
	for i := range 8 {
		if scr.gen[i] == fs3 || scr.gen[i] == fs3+1 {
			t.Fatalf("slot %d survived the epoch wrap as reached", i)
		}
	}
}

// runTypedBoth runs q with the typed prefilter on, then off (boxed gated
// flow), returning both row lists in engine order. The off leg is
// vacuity-checked at zero rejects; the on leg's engagement is asserted
// by the callers that expect it (an unclassifiable key set legitimately
// runs boxed on both legs).
func runTypedBoth(t *testing.T, g *chickpeas.Snapshot, q string) (typed, boxed []string) {
	t.Helper()
	disableTypedSink = false
	typed, _ = runBothOrdered(t, g, q)
	before := typedSinkRejects
	disableTypedSink = true
	boxed, _ = runBothOrdered(t, g, q)
	disableTypedSink = false
	if typedSinkRejects != before {
		t.Fatal("disabled leg took typed rejects -- the off switch is not isolating the mechanism")
	}
	return typed, boxed
}

// TestTypedSinkPrefilter pins the typed reject path: i64-column keys on
// a full heap refuse rows without boxed evaluation (the counter), and
// the surviving rows are identical to the boxed gated flow -- single
// key, two-key IC9 shape (DESC then ASC), and DESC alone.
func TestTypedSinkPrefilter(t *testing.T) {
	g := topkFixture(t, 500)
	before := typedSinkRejects
	rows, _ := runBothOrdered(t, g,
		"MATCH (m:N) RETURN m.v AS v, m.name AS name ORDER BY v ASC LIMIT 5")
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(rows))
	}
	if got := typedSinkRejects - before; got < 400 {
		t.Fatalf("typed rejects = %d, want most of the 495 refusals (0 means the prefilter never engaged)", got)
	}
	for _, q := range []string{
		"MATCH (m:N) RETURN m.v AS v, m.name AS name ORDER BY v ASC LIMIT 5",
		"MATCH (m:N) RETURN m.v AS v, m.name AS name ORDER BY v DESC LIMIT 7",
		"MATCH (m:N) RETURN m.grp AS g, m.v AS v ORDER BY g DESC, v ASC LIMIT 9",
		"MATCH (m:N) RETURN m.grp AS g, m.name AS name ORDER BY g ASC LIMIT 6",
	} {
		typed, boxed := runTypedBoth(t, g, q)
		if fmt.Sprint(typed) != fmt.Sprint(boxed) {
			t.Fatalf("typed prefilter diverged on %s:\n%v\nvs\n%v", q, typed, boxed)
		}
	}
}

// TestTypedSinkLargeIntTies pins the float64-mirror constraint: adjacent
// int64 keys above 2^53 collapse to OrderCmp TIES (the boxed Int tier
// compares through float64), so arrival order decides -- a raw i64
// compare would order them and change the survivors. The typed path must
// reproduce the boxed outcome exactly.
func TestTypedSinkLargeIntTies(t *testing.T) {
	b := chickpeas.NewBuilder(64, 1)
	base := int64(1) << 53
	for i := range 40 {
		nd, err := b.AddNode("N")
		if err != nil {
			t.Fatal(err)
		}
		// Pairs (base+2i, base+2i+1) are distinct i64s but equal float64s.
		if err := b.SetProp(nd, "v", base+int64(i)); err != nil {
			t.Fatal(err)
		}
		if err := b.SetProp(nd, "name", fmt.Sprintf("n%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	g := b.Finalize("topk-large")
	for _, q := range []string{
		"MATCH (m:N) RETURN m.v AS v, m.name AS name ORDER BY v ASC LIMIT 7",
		"MATCH (m:N) RETURN m.v AS v, m.name AS name ORDER BY v DESC LIMIT 7",
	} {
		typed, boxed := runTypedBoth(t, g, q)
		if fmt.Sprint(typed) != fmt.Sprint(boxed) {
			t.Fatalf("large-int tie divergence on %s:\n%v\nvs\n%v", q, typed, boxed)
		}
	}
}

// TestTypedSinkNullFallback pins the per-row fallback: nodes lacking the
// key property read as Null (a different OrderCmp tier), which the typed
// reader cannot represent -- those rows must take the boxed path and the
// output must match the boxed flow exactly. When a Null lands IN the
// heap, the root shadow invalidates and the prefilter stands down.
func TestTypedSinkNullFallback(t *testing.T) {
	b := chickpeas.NewBuilder(64, 1)
	for i := range 30 {
		nd, err := b.AddNode("N")
		if err != nil {
			t.Fatal(err)
		}
		if i%3 != 0 { // every third node lacks v entirely
			if err := b.SetProp(nd, "v", int64(100-i)); err != nil {
				t.Fatal(err)
			}
		}
		if err := b.SetProp(nd, "name", fmt.Sprintf("n%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	g := b.Finalize("topk-null")
	for _, q := range []string{
		"MATCH (m:N) RETURN m.v AS v, m.name AS name ORDER BY v ASC LIMIT 4",
		// LIMIT larger than the non-null population: nulls enter the heap.
		"MATCH (m:N) RETURN m.v AS v, m.name AS name ORDER BY v DESC LIMIT 25",
	} {
		typed, boxed := runTypedBoth(t, g, q)
		if fmt.Sprint(typed) != fmt.Sprint(boxed) {
			t.Fatalf("null-fallback divergence on %s:\n%v\nvs\n%v", q, typed, boxed)
		}
	}
}
