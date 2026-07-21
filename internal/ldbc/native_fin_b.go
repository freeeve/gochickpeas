// Native FinBench kernels CR7-CR12 -- ports of rustychickpeas-ldbc
// src/finbench/{cr/reads_7_12.rs, mod.rs} (in/out ratio, loan fund
// trace, laundering ratios, investor similarity, guarantee exposure,
// company transfer stats).

package ldbc

import (
	"fmt"
	"slices"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/flatset"
	"github.com/freeeve/gochickpeas/gql/value"
)

func init() {
	registerNativeV("FinBench", "CR7", finCR7)
	registerNativeV("FinBench", "CR8", finCR8)
	registerNativeV("FinBench", "CR9", finCR9)
	registerNativeV("FinBench", "CR10", finCR10)
	registerNativeV("FinBench", "CR11", finCR11)
	registerNativeV("FinBench", "CR12", finCR12)
}

// tsAmtRel is a (ts, amount, endpoint) transfer candidate.
type tsAmtRel struct {
	ts  int64
	amt float64
	nbr chickpeas.NodeID
}

// truncByTS keeps the top-limit rels by timestamp (desc when !asc).
func truncByTS(rels []tsAmtRel, limit int, asc bool) []tsAmtRel {
	if len(rels) <= limit {
		return rels
	}
	if asc {
		sortByLess(rels, func(a, b tsAmtRel) bool { return a.ts < b.ts })
	} else {
		sortByLess(rels, func(a, b tsAmtRel) bool { return a.ts > b.ts })
	}
	return rels[:limit]
}

// finCR7 -- transfer in/out ratio around the seed account;
// [[numSrc, numDst, inOutRatio]] (ratio rounded 3dp, -1 when no
// outgoing amount).
func finCR7(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	account, err := finNode(g, "Account", finSeedAccount)
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		const threshold = 0.0
		gather := func(dir chickpeas.Direction) (int64, float64) {
			var rels []tsAmtRel
			for r := range g.Rels(account, dir, "transfer") {
				ts := cols.relTS(r.Pos)
				amt := cols.relAmt(r.Pos)
				if ts >= finWS && ts <= finWE && amt > threshold {
					rels = append(rels, tsAmtRel{ts, amt, r.Neighbor})
				}
			}
			rels = truncByTS(rels, finTruncLimit, false)
			distinct := map[chickpeas.NodeID]bool{}
			var sum float64
			for _, r := range rels {
				distinct[r.nbr] = true
				sum += r.amt
			}
			return int64(len(distinct)), sum
		}
		numSrc, inAmt := gather(chickpeas.Incoming)
		numDst, outAmt := gather(chickpeas.Outgoing)
		ratio := -1.0
		if outAmt > 0 {
			ratio = round3f(inAmt / outAmt)
		}
		return [][]value.Value{{value.Int(numSrc), value.Int(numDst), value.Float(ratio)}}, nil
	}, nil
}

// finCR8 -- transfer trace after loan applied: BFS from the loan's
// deposited accounts over transfer/withdraw (<=3 from the loan), each
// hop's amount gated by the node's upstream inflow; [dstId, ratio,
// minDistance] (ratio = inflow / loanAmount, rounded 3dp).
func finCR8(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	loan, err := finNode(g, "Loan", finSeedLoan)
	if err != nil {
		return nil, err
	}
	laCol, ok := g.ColIndexed("loanAmount")
	if !ok {
		return nil, fmt.Errorf("node column loanAmount missing")
	}
	la := laCol.F64()
	type depRel struct {
		acct chickpeas.NodeID
		amt  float64
	}
	type qe struct {
		node   chickpeas.NodeID
		dist   uint32
		inflow float64
	}
	type amtRel struct {
		amt float64
		nbr chickpeas.NodeID
	}
	// Per-run scratch hoisted into the prepare scope: the flat results
	// table (packed-key probe index over parallel inflow/dist slabs), the
	// per-deposit visited set and BFS queue, and the rel gather. Warm runs
	// reset each into its high-water backing instead of re-growing maps
	// and queue arrays from empty.
	var (
		deposits []depRel
		queue    []qe
		rels     []amtRel
		visited  flatset.U32Set
		resIdx   flatset.U64Map
		resNode  []chickpeas.NodeID
		resIn    []float64
		resDist  []uint32
	)
	return func() ([][]value.Value, error) {
		const threshold = 0.0
		loanAmount, ok := la.Get(loan)
		if !ok {
			loanAmount = 1.0
		}
		deposits = deposits[:0]
		for r := range g.Rels(loan, chickpeas.Outgoing, "deposit") {
			ts := cols.relTS(r.Pos)
			if ts >= finWS && ts <= finWE {
				deposits = append(deposits, depRel{r.Neighbor, cols.relAmt(r.Pos)})
			}
		}
		resIdx.Reset()
		resNode, resIn, resDist = resNode[:0], resIn[:0], resDist[:0]
		for _, dep := range deposits {
			visited.Reset()
			visited.Add(uint32(dep.acct))
			queue = append(queue[:0], qe{dep.acct, 1, dep.amt})
			for head := 0; head < len(queue); head++ {
				cur := queue[head]
				i := resIdx.GetOrCreate(uint64(cur.node), func() int {
					resNode = append(resNode, cur.node)
					resIn = append(resIn, 0)
					resDist = append(resDist, ^uint32(0))
					return len(resNode) - 1
				})
				resIn[i] += cur.inflow
				if cur.dist < resDist[i] {
					resDist[i] = cur.dist
				}
				if cur.dist >= 3 {
					continue
				}
				var upstream float64
				for r := range g.Rels(cur.node, chickpeas.Incoming, "transfer") {
					ts := cols.relTS(r.Pos)
					if ts >= finWS && ts <= finWE {
						upstream += cols.relAmt(r.Pos)
					}
				}
				rels = rels[:0]
				for _, relType := range [...]string{"transfer", "withdraw"} {
					for r := range g.Rels(cur.node, chickpeas.Outgoing, relType) {
						ts := cols.relTS(r.Pos)
						if ts >= finWS && ts <= finWE {
							rels = append(rels, amtRel{cols.relAmt(r.Pos), r.Neighbor})
						}
					}
				}
				// The Rust reference compares its truncation order
				// case-SENSITIVELY ("DESC"), so the harness's "desc"
				// falls through to the ascending sort -- the lowest
				// amount claims each node. The refs pin that behavior.
				sortByLess(rels, func(a, b amtRel) bool { return a.amt < b.amt })
				if len(rels) > finTruncLimit {
					rels = rels[:finTruncLimit]
				}
				for _, r := range rels {
					if r.amt > threshold*upstream && visited.Add(uint32(r.nbr)) {
						queue = append(queue, qe{r.nbr, cur.dist + 1, r.amt})
					}
				}
			}
		}
		// Typed sort, then one flat cell block for the boxed rows.
		type cand struct {
			id    int64
			ratio float64
			dist  int64
		}
		cands := make([]cand, 0, len(resNode))
		for i, did := range resNode {
			cands = append(cands, cand{cols.oid(did), round3f(resIn[i] / loanAmount), int64(resDist[i])})
		}
		sortByLess(cands, func(a, b cand) bool {
			return cmpChain(
				cmpI64Desc(a.dist, b.dist),
				cmpF64Desc(a.ratio, b.ratio),
				cmpI64Asc(a.id, b.id),
			)
		})
		cells := make([]value.Value, len(cands)*3)
		rows := make([][]value.Value, len(cands))
		for i, c := range cands {
			cells[i*3] = value.Int(c.id)
			cells[i*3+1] = value.Float(c.ratio)
			cells[i*3+2] = value.Int(c.dist)
			rows[i] = cells[i*3 : i*3+3 : i*3+3]
		}
		return rows, nil
	}, nil
}

// finCR9 -- money laundering ratios around the seed account;
// [[ratioRepay, ratioDeposit, ratioTransfer]] (each rounded 3dp, -1 on
// an empty denominator).
func finCR9(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	account, err := finNode(g, "Account", finSeedAccount)
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		const threshold = 0.0
		gather := func(dir chickpeas.Direction, relType string, amtFloor bool) []tsAmtRel {
			var rels []tsAmtRel
			for r := range g.Rels(account, dir, relType) {
				ts := cols.relTS(r.Pos)
				amt := cols.relAmt(r.Pos)
				if ts < finWS || ts > finWE {
					continue
				}
				if amtFloor && amt < threshold {
					continue
				}
				rels = append(rels, tsAmtRel{ts, amt, r.Neighbor})
			}
			return truncByTS(rels, finTruncLimit, false)
		}
		sum := func(rels []tsAmtRel) float64 {
			var s float64
			for _, r := range rels {
				s += r.amt
			}
			return s
		}
		rel1 := sum(gather(chickpeas.Outgoing, "repay", false))
		rel2 := sum(gather(chickpeas.Incoming, "deposit", false))
		rel3 := sum(gather(chickpeas.Outgoing, "transfer", true))
		rel4 := sum(gather(chickpeas.Incoming, "transfer", true))
		ratio := func(num, den float64) float64 {
			if den == 0 {
				return -1.0
			}
			return round3f(num / den)
		}
		return [][]value.Value{{value.Float(ratio(rel1, rel2)), value.Float(ratio(rel1, rel4)), value.Float(ratio(rel3, rel4))}}, nil
	}, nil
}

// finCR10 -- investor similarity: co-investors per company the seed
// invests in (window is full, so all invest rels qualify); [otherId,
// sharedCompanyCount].
func finCR10(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	person, err := finNode(g, "Person", finSeedInvestor)
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		companies := map[chickpeas.NodeID]bool{}
		for r := range g.Rels(person, chickpeas.Outgoing, "invest") {
			ts := cols.relTS(r.Pos)
			if ts >= finWS && ts <= finWE {
				companies[r.Neighbor] = true
			}
		}
		shared := map[chickpeas.NodeID]int64{}
		for c := range companies {
			for r := range g.Rels(c, chickpeas.Incoming, "invest") {
				if r.Neighbor != person {
					shared[r.Neighbor]++
				}
			}
		}
		rows := make([][]value.Value, 0, len(shared))
		for o, c := range shared {
			rows = append(rows, []value.Value{value.Int(cols.oid(o)), value.Int(c)})
		}
		return sortTruncate(rows, 0, func(a, b []value.Value) bool {
			a1, _ := a[1].AsInt()
			b1, _ := b[1].AsInt()
			a0, _ := a[0].AsInt()
			b0, _ := b[0].AsInt()
			return cmpChain(
				cmpI64Desc(a1, b1),
				cmpI64Asc(a0, b0),
			)
		}), nil
	}, nil
}

// finCR11 -- guarantee exposure: walk the guarantee chain from the
// seed person, summing every reached person's applied loan amounts;
// [[total]].
func finCR11(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	person, err := finNode(g, "Person", finSeedPerson)
	if err != nil {
		return nil, err
	}
	laCol, ok := g.RelColIndexed("loanAmount")
	if !ok {
		return nil, fmt.Errorf("rel column loanAmount missing")
	}
	la := laCol.F64()
	return func() ([][]value.Value, error) {
		visited := map[chickpeas.NodeID]bool{person: true}
		queue := []chickpeas.NodeID{person}
		var total float64
		for len(queue) > 0 {
			p := queue[0]
			queue = queue[1:]
			for r := range g.Rels(p, chickpeas.Outgoing, "apply") {
				if v, ok := la.Get(r.Pos); ok {
					total += v
				}
			}
			for r := range g.Rels(p, chickpeas.Outgoing, "guarantee") {
				if !visited[r.Neighbor] {
					visited[r.Neighbor] = true
					queue = append(queue, r.Neighbor)
				}
			}
		}
		return [][]value.Value{{value.Float(total)}}, nil
	}, nil
}

// finCR12 -- transfer-to-company statistics: the owner's accounts'
// in-window transfers into company-owned accounts, summed per target;
// [compAccountId, summedAmount].
func finCR12(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	person, err := finNode(g, "Person", finSeedOwner)
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		companies, ok := g.NodesWithLabel("Company")
		if !ok {
			return [][]value.Value{}, nil
		}
		var personAccounts []chickpeas.NodeID
		for r := range g.Rels(person, chickpeas.Outgoing, "own") {
			personAccounts = append(personAccounts, r.Neighbor)
		}
		if len(personAccounts) > finTruncLimit {
			slices.Sort(personAccounts)
			personAccounts = personAccounts[:finTruncLimit]
		}
		amounts := map[chickpeas.NodeID]float64{}
		var transfers []tsAmtRel
		for _, acct := range personAccounts {
			transfers = transfers[:0]
			for r := range g.Rels(acct, chickpeas.Outgoing, "transfer") {
				ts := cols.relTS(r.Pos)
				if ts >= finWS && ts <= finWE {
					transfers = append(transfers, tsAmtRel{ts, cols.relAmt(r.Pos), r.Neighbor})
				}
			}
			if len(transfers) > finTruncLimit {
				sortByLess(transfers, func(a, b tsAmtRel) bool { return a.amt > b.amt })
				transfers = transfers[:finTruncLimit]
			}
			for _, t := range transfers {
				companyOwned := false
				for own := range g.Rels(t.nbr, chickpeas.Incoming, "own") {
					if companies.Contains(own.Neighbor) {
						companyOwned = true
						break
					}
				}
				if companyOwned {
					amounts[t.nbr] += t.amt
				}
			}
		}
		rows := make([][]value.Value, 0, len(amounts))
		for a, s := range amounts {
			rows = append(rows, []value.Value{value.Int(cols.oid(a)), value.Float(s)})
		}
		return sortTruncate(rows, 0, func(a, b []value.Value) bool {
			a1, _ := a[1].AsFloat()
			b1, _ := b[1].AsFloat()
			a0, _ := a[0].AsInt()
			b0, _ := b[0].AsInt()
			return cmpChain(
				cmpF64Desc(a1, b1),
				cmpI64Asc(a0, b0),
			)
		}), nil
	}, nil
}
