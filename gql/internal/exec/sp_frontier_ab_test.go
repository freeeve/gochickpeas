// Structural A/B of the bidirectional shortest-path frontier-selection
// heuristic: degree-sum (expand the frontier with the smaller pending-EDGE
// count) versus node-count (expand the frontier with fewer nodes). The
// rustychickpeas twin (our tasks 178/179) found degree-sum is skew-gated --
// a win only when a frontier is dominated by extreme-degree hubs, and net
// overhead on moderate-skew graphs (LDBC Person-KNOWS) because there the two
// strategies pick the SAME side, so the per-node Degree() sum is pure cost.
//
// This measures that claim on OUR shortestPath deterministically, with no
// reliance on this box's noisy wall-clock: per regime it counts EDGES TOUCHED
// (the real traversal cost = sum of expanded nodes' degrees) under each
// strategy, plus the extra Degree() calls degree-sum pays. Degree-sum is a net
// win iff edges_saved(node - degree) exceeds those extra calls, since one
// Degree() call and touching one edge are both ~one CSR access.
package exec

import (
	"math/rand"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// spCounters accumulates the cost metrics of one instrumented search.
type spCounters struct {
	edgesTouched int // sum of expanded nodes' out-degrees -- the walk cost
	degreeCalls  int // extra Degree() calls degree-sum pays (0 for node-count)
}

// shortestPathAB mirrors shortestPath's bidirectional walk but selects the
// side to expand by degree-sum (useDegree) or node-count, and tallies the cost
// metrics instead of building a path. Result reachability/length matches
// shortestPath so the strategies stay comparable.
func shortestPathAB(ctx *eval.Ctx, a, b graph.NodeID, sp *plan.SpStage, rm *graph.RelMatcher, scr *spScratch, useDegree bool, c *spCounters) (int, bool) {
	if a == b {
		return 0, true
	}
	fs := scr.begin(int(ctx.G.IDSpace()))
	bs := fs + 1
	scr.gen[a], scr.dist[a], scr.parent[a] = fs, 0, a
	scr.gen[b], scr.dist[b], scr.parent[b] = bs, 0, b
	fFront := append(scr.frontier[:0], a)
	fNext := scr.next[:0]
	bFront := append(scr.bFrontier[:0], b)
	bNext := scr.bNext[:0]
	defer func() {
		scr.frontier, scr.next = fFront, fNext
		scr.bFrontier, scr.bNext = bFront, bNext
	}()
	dirB := flipDir(sp.Dir)
	found := false
	df, db := uint64(0), uint64(0)
	fDeg := uint64(ctx.G.Degree(a, sp.Dir))
	bDeg := uint64(ctx.G.Degree(b, dirB))
	if useDegree {
		c.degreeCalls += 2
	}
	meetDist := 0
	for len(fFront) > 0 && len(bFront) > 0 && df+db < spCap(sp) && !found {
		expandForward := fDeg <= bDeg
		if !useDegree {
			expandForward = len(fFront) <= len(bFront)
		}
		if expandForward {
			fNext = fNext[:0]
			fDeg = 0
			for _, u := range fFront {
				nbrs := appendHopNeighbors(ctx, scr, u, sp.Dir, rm, nil)
				c.edgesTouched += len(nbrs)
				for _, v := range nbrs {
					switch scr.gen[v] {
					case fs:
					case bs:
						found = true
						meetDist = int(df) + 1 + int(scr.dist[v])
					default:
						scr.gen[v], scr.parent[v], scr.dist[v] = fs, u, uint32(df+1)
						fNext = append(fNext, v)
						if useDegree {
							fDeg += uint64(ctx.G.Degree(v, sp.Dir))
							c.degreeCalls++
						}
					}
					if found {
						break
					}
				}
				if found {
					break
				}
			}
			fFront, fNext = fNext, fFront
			df++
		} else {
			bNext = bNext[:0]
			bDeg = 0
			for _, u := range bFront {
				nbrs := appendHopNeighbors(ctx, scr, u, dirB, rm, nil)
				c.edgesTouched += len(nbrs)
				for _, v := range nbrs {
					switch scr.gen[v] {
					case bs:
					case fs:
						found = true
						meetDist = int(db) + 1 + int(scr.dist[v])
					default:
						scr.gen[v], scr.parent[v], scr.dist[v] = bs, u, uint32(db+1)
						bNext = append(bNext, v)
						if useDegree {
							bDeg += uint64(ctx.G.Degree(v, dirB))
							c.degreeCalls++
						}
					}
					if found {
						break
					}
				}
				if found {
					break
				}
			}
			bFront, bNext = bNext, bFront
			db++
		}
	}
	return meetDist, found
}

// randomGraph builds n nodes over one rel type "R"; edgesOf(i) returns node i's
// out-degree, targets chosen uniformly. A fixed rand source keeps runs
// reproducible.
func buildRegime(t *testing.B, n int, edgesOf func(i int, rng *rand.Rand) int, seed int64) *eval.Ctx {
	rng := rand.New(rand.NewSource(seed))
	var degs []int
	total := 0
	for i := 0; i < n; i++ {
		d := edgesOf(i, rng)
		degs = append(degs, d)
		total += d
	}
	bl := chickpeas.NewBuilder(n, total)
	for i := 0; i < n; i++ {
		if _, err := bl.AddNode("N"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		for j := 0; j < degs[i]; j++ {
			tgt := chickpeas.NodeID(rng.Intn(n))
			if tgt == chickpeas.NodeID(i) {
				continue
			}
			if _, err := bl.AddRel(chickpeas.NodeID(i), tgt, "R"); err != nil {
				t.Fatal(err)
			}
		}
	}
	return &eval.Ctx{G: graph.New(bl.Finalize())}
}

// BenchmarkSPFrontierAB reports the structural A/B as benchmark log output
// (run with -run x -bench SPFrontierAB to see the per-regime table). It is a
// measurement harness, not a timing benchmark -- the reported edges/calls are
// deterministic and box-independent.
func BenchmarkSPFrontierAB(b *testing.B) {
	const n = 100_000
	regimes := []struct {
		name    string
		edgesOf func(i int, rng *rand.Rand) int
	}{
		{"uniform-deg8", func(i int, rng *rand.Rand) int { return 8 }},
		{"skewed-heavytail", func(i int, rng *rand.Rand) int {
			if rng.Float64() < 0.02 {
				return 150 + rng.Intn(100) // ~2% heavy nodes ~200-degree
			}
			return 3
		}},
		{"hub-extreme", func(i int, rng *rand.Rand) int {
			if i < 32 {
				return 40_000 // 32 giant hubs
			}
			return 3
		}},
	}
	for _, rg := range regimes {
		ctx := buildRegime(b, n, rg.edgesOf, 42)
		rm := ctx.G.CompileRelMatcher([]string{"R"})
		sp := &plan.SpStage{Dir: graph.Outgoing, Types: []string{"R"}}
		scr := newSPScratch()
		pairRng := rand.New(rand.NewSource(7))
		var degE, degCalls, nodeE, connected, mismatch int
		const pairs = 2000
		for p := 0; p < pairs; p++ {
			a := graph.NodeID(pairRng.Intn(n))
			bb := graph.NodeID(pairRng.Intn(n))
			var cd, cn spCounters
			ld, fd := shortestPathAB(ctx, a, bb, sp, rm, scr, true, &cd)
			ln, fn := shortestPathAB(ctx, a, bb, sp, rm, scr, false, &cn)
			if fd != fn || (fd && ld != ln) {
				mismatch++
			}
			if fd {
				connected++
			}
			degE += cd.edgesTouched
			degCalls += cd.degreeCalls
			nodeE += cn.edgesTouched
		}
		edgesSaved := nodeE - degE
		b.Logf("%-18s connected=%d/%d mismatch=%d | edgesTouched degree=%d node=%d saved=%d | degreeCalls=%d | verdict=%s",
			rg.name, connected, pairs, mismatch, degE, nodeE, edgesSaved, degCalls, verdict(edgesSaved, degCalls))
	}
}

// verdict classifies degree-sum's edge savings against its extra Degree() calls.
// One edge-touch (a gen-stamp check + slice append + parent/dist writes) costs
// somewhat more than one Degree() call (a CSR offset subtraction), so the
// break-even is not exactly 1:1 -- hence a middle "cost-ratio-dependent" band
// rather than a false-precision boundary.
func verdict(edgesSaved, degreeCalls int) string {
	switch {
	case edgesSaved < degreeCalls/10:
		return "NODE-COUNT WINS (degree-sum is pure overhead: <10% of its cost saved)"
	case edgesSaved > degreeCalls*2:
		return "DEGREE-SUM WINS (edge savings dwarf the Degree() overhead)"
	default:
		return "COST-RATIO-DEPENDENT (savings ~ overhead; degree-sum likely wins as edge-touch > Degree call)"
	}
}
