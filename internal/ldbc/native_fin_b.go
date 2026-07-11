// Native FinBench kernels CR7-CR12 -- ports of rustychickpeas-ldbc
// src/finbench/{cr/reads_7_12.rs, mod.rs} (in/out ratio, loan fund
// trace, laundering ratios, investor similarity, guarantee exposure,
// company transfer stats).

package ldbc

import (
	"fmt"
	"slices"
	"sort"

	chickpeas "github.com/freeeve/gochickpeas"
)

func init() {
	registerNative("FinBench", "CR7", finCR7)
	registerNative("FinBench", "CR8", finCR8)
	registerNative("FinBench", "CR9", finCR9)
	registerNative("FinBench", "CR10", finCR10)
	registerNative("FinBench", "CR11", finCR11)
	registerNative("FinBench", "CR12", finCR12)
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
		sort.Slice(rels, func(i, j int) bool { return rels[i].ts < rels[j].ts })
	} else {
		sort.Slice(rels, func(i, j int) bool { return rels[i].ts > rels[j].ts })
	}
	return rels[:limit]
}

// finCR7 -- transfer in/out ratio around the seed account;
// [[numSrc, numDst, inOutRatio]] (ratio rounded 3dp, -1 when no
// outgoing amount).
func finCR7(g *chickpeas.Snapshot) (func() ([][]any, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	account, err := finNode(g, "Account", finSeedAccount)
	if err != nil {
		return nil, err
	}
	return func() ([][]any, error) {
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
		return [][]any{{numSrc, numDst, ratio}}, nil
	}, nil
}

// finCR8 -- transfer trace after loan applied: BFS from the loan's
// deposited accounts over transfer/withdraw (<=3 from the loan), each
// hop's amount gated by the node's upstream inflow; [dstId, ratio,
// minDistance] (ratio = inflow / loanAmount, rounded 3dp).
func finCR8(g *chickpeas.Snapshot) (func() ([][]any, error), error) {
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
	return func() ([][]any, error) {
		const threshold = 0.0
		loanAmount, ok := la.Get(loan)
		if !ok {
			loanAmount = 1.0
		}
		type depRel struct {
			acct chickpeas.NodeID
			amt  float64
		}
		var deposits []depRel
		for r := range g.Rels(loan, chickpeas.Outgoing, "deposit") {
			ts := cols.relTS(r.Pos)
			if ts >= finWS && ts <= finWE {
				deposits = append(deposits, depRel{r.Neighbor, cols.relAmt(r.Pos)})
			}
		}
		type acc struct {
			inflow float64
			dist   uint32
		}
		results := map[chickpeas.NodeID]*acc{}
		type amtRel struct {
			amt float64
			nbr chickpeas.NodeID
		}
		var rels []amtRel
		for _, dep := range deposits {
			visited := map[chickpeas.NodeID]bool{dep.acct: true}
			type qe struct {
				node   chickpeas.NodeID
				dist   uint32
				inflow float64
			}
			queue := []qe{{dep.acct, 1, dep.amt}}
			for len(queue) > 0 {
				cur := queue[0]
				queue = queue[1:]
				e := results[cur.node]
				if e == nil {
					results[cur.node] = &acc{cur.inflow, cur.dist}
				} else {
					e.inflow += cur.inflow
					if cur.dist < e.dist {
						e.dist = cur.dist
					}
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
				for _, relType := range []string{"transfer", "withdraw"} {
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
				sort.Slice(rels, func(i, j int) bool { return rels[i].amt < rels[j].amt })
				if len(rels) > finTruncLimit {
					rels = rels[:finTruncLimit]
				}
				for _, r := range rels {
					if r.amt > threshold*upstream && !visited[r.nbr] {
						visited[r.nbr] = true
						queue = append(queue, qe{r.nbr, cur.dist + 1, r.amt})
					}
				}
			}
		}
		rows := make([][]any, 0, len(results))
		for did, a := range results {
			rows = append(rows, []any{cols.oid(did), round3f(a.inflow / loanAmount), int64(a.dist)})
		}
		return sortTruncate(rows, 0, func(a, b []any) bool {
			return cmpChain(
				cmpI64Desc(a[2].(int64), b[2].(int64)),
				cmpF64Desc(a[1].(float64), b[1].(float64)),
				cmpI64Asc(a[0].(int64), b[0].(int64)),
			)
		}), nil
	}, nil
}

// finCR9 -- money laundering ratios around the seed account;
// [[ratioRepay, ratioDeposit, ratioTransfer]] (each rounded 3dp, -1 on
// an empty denominator).
func finCR9(g *chickpeas.Snapshot) (func() ([][]any, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	account, err := finNode(g, "Account", finSeedAccount)
	if err != nil {
		return nil, err
	}
	return func() ([][]any, error) {
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
		return [][]any{{ratio(rel1, rel2), ratio(rel1, rel4), ratio(rel3, rel4)}}, nil
	}, nil
}

// finCR10 -- investor similarity: co-investors per company the seed
// invests in (window is full, so all invest rels qualify); [otherId,
// sharedCompanyCount].
func finCR10(g *chickpeas.Snapshot) (func() ([][]any, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	person, err := finNode(g, "Person", finSeedInvestor)
	if err != nil {
		return nil, err
	}
	return func() ([][]any, error) {
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
		rows := make([][]any, 0, len(shared))
		for o, c := range shared {
			rows = append(rows, []any{cols.oid(o), c})
		}
		return sortTruncate(rows, 0, func(a, b []any) bool {
			return cmpChain(
				cmpI64Desc(a[1].(int64), b[1].(int64)),
				cmpI64Asc(a[0].(int64), b[0].(int64)),
			)
		}), nil
	}, nil
}

// finCR11 -- guarantee exposure: walk the guarantee chain from the
// seed person, summing every reached person's applied loan amounts;
// [[total]].
func finCR11(g *chickpeas.Snapshot) (func() ([][]any, error), error) {
	person, err := finNode(g, "Person", finSeedPerson)
	if err != nil {
		return nil, err
	}
	laCol, ok := g.RelColIndexed("loanAmount")
	if !ok {
		return nil, fmt.Errorf("rel column loanAmount missing")
	}
	la := laCol.F64()
	return func() ([][]any, error) {
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
		return [][]any{{total}}, nil
	}, nil
}

// finCR12 -- transfer-to-company statistics: the owner's accounts'
// in-window transfers into company-owned accounts, summed per target;
// [compAccountId, summedAmount].
func finCR12(g *chickpeas.Snapshot) (func() ([][]any, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	person, err := finNode(g, "Person", finSeedOwner)
	if err != nil {
		return nil, err
	}
	return func() ([][]any, error) {
		companies, ok := g.NodesWithLabel("Company")
		if !ok {
			return [][]any{}, nil
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
				sort.Slice(transfers, func(i, j int) bool { return transfers[i].amt > transfers[j].amt })
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
		rows := make([][]any, 0, len(amounts))
		for a, s := range amounts {
			rows = append(rows, []any{cols.oid(a), s})
		}
		return sortTruncate(rows, 0, func(a, b []any) bool {
			return cmpChain(
				cmpF64Desc(a[1].(float64), b[1].(float64)),
				cmpI64Asc(a[0].(int64), b[0].(int64)),
			)
		}), nil
	}, nil
}
