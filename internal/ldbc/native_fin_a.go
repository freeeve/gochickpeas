// Native FinBench kernels CR1-CR6 -- ports of rustychickpeas-ldbc
// src/finbench/{cr/reads_1_6.rs, mod.rs} onto the canonical SF10
// .rcpg: lowercase rel types, rel cols createTime (ms) / amount, node
// cols isBlocked / loanAmount / balance. Seeds and the full-window
// params are the recorded values the refs were emitted with
// (python/refs/finbench/seeds-sf10.json).

package ldbc

import (
	"fmt"
	"math"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

// Recorded FinBench SF10 seeds (original FinBench ids).
const (
	finSeedAccount  = 234469755111622579
	finSeedCR1      = 4877400870343548146
	finSeedPerson   = 26388279096622
	finSeedOwner    = 24189255841071
	finSeedCard     = 240943129820355424
	finSeedLoan     = 279505201629616671
	finSeedInvestor = 28587302363231
	finSeedCycle    = 4877400870343548146
	finSeedDst      = 304838499289284059
	finWindowMS     = 7776000000 // 90 days
	finTruncLimit   = 10000
	finWS           = math.MinInt64
	finWE           = math.MaxInt64
)

func init() {
	registerNativeV("FinBench", "CR1", finCR1)
	registerNativeV("FinBench", "CR2", finCR2)
	registerNativeV("FinBench", "CR3", finCR3)
	registerNativeV("FinBench", "CR4", finCR4)
	registerNativeV("FinBench", "CR5", finCR5)
	registerNativeV("FinBench", "CR6", finCR6)
}

// finCols bundles the hot FinBench columns every kernel hoists once:
// rel createTime/amount, and the node id map back to original ids.
type finCols struct {
	ts  chickpeas.I64Col
	amt chickpeas.F64Col
	id  chickpeas.I64Col
}

func finColsOf(g *chickpeas.Snapshot) (finCols, error) {
	tsCol, ok := g.RelColIndexed("createTime")
	if !ok {
		return finCols{}, fmt.Errorf("rel column createTime missing")
	}
	amtCol, ok := g.RelColIndexed("amount")
	if !ok {
		return finCols{}, fmt.Errorf("rel column amount missing")
	}
	idCol, err := nodeI64Col(g, "id")
	if err != nil {
		return finCols{}, err
	}
	return finCols{tsCol.I64(), amtCol.F64(), idCol}, nil
}

// relTS reads a rel timestamp, defaulting absent to MinInt64 (in-window
// under the full-window seeds, like the Rust rel_ts).
func (c finCols) relTS(pos uint32) int64 {
	if v, ok := c.ts.Get(pos); ok {
		return v
	}
	return math.MinInt64
}

// relAmt reads a rel amount, defaulting absent to 0.
func (c finCols) relAmt(pos uint32) float64 {
	v, _ := c.amt.Get(pos)
	return v
}

// oid maps an internal node to its original FinBench id.
func (c finCols) oid(n chickpeas.NodeID) int64 { return i64At(c.id, n) }

// finNode resolves a seed by label + original id.
func finNode(g *chickpeas.Snapshot, label string, id int64) (chickpeas.NodeID, error) {
	n, ok := nodeByID(g, label, id)
	if !ok {
		return 0, fmt.Errorf("%s %d missing", label, id)
	}
	return n, nil
}

// tsRel is a (ts, neighbor) candidate of the truncation-ordered BFS
// frontiers.
type tsRel struct {
	ts  int64
	nbr chickpeas.NodeID
}

// sortTSRels orders a frontier by timestamp in truncation order.
func sortTSRels(rels []tsRel, asc bool) {
	if asc {
		sortByLess(rels, func(a, b tsRel) bool { return a.ts < b.ts })
	} else {
		sortByLess(rels, func(a, b tsRel) bool { return a.ts > b.ts })
	}
}

// round3f mirrors the Rust kernels' in-query 3-decimal rounding.
func round3f(x float64) float64 { return math.Round(x*1000) / 1000 }

// finCR1 -- blocked-medium related accounts: <=3-hop reverse transfer
// trace with strictly-decreasing (backward) timestamps; each reached
// account's blocked signIn media emit [otherId, distance, mediumId,
// "Medium"].
func finCR1(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	account, err := finNode(g, "Account", finSeedCR1)
	if err != nil {
		return nil, err
	}
	blockedCol, ok := g.ColIndexed("isBlocked")
	if !ok {
		return nil, fmt.Errorf("node column isBlocked missing")
	}
	blocked := blockedCol.Bool()
	return func() ([][]value.Value, error) {
		var rows [][]value.Value
		visited := map[chickpeas.NodeID]bool{account: true}
		type qe struct {
			node   chickpeas.NodeID
			depth  uint32
			lastTS int64
		}
		queue := []qe{{account, 0, math.MaxInt64}}
		var rels []tsRel
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if cur.depth >= 3 {
				continue
			}
			rels = rels[:0]
			for r := range g.Rels(cur.node, chickpeas.Incoming, "transfer") {
				ts := cols.relTS(r.Pos)
				if ts >= finWS && ts <= finWE && ts < cur.lastTS {
					rels = append(rels, tsRel{ts, r.Neighbor})
				}
			}
			sortTSRels(rels, false)
			if len(rels) > finTruncLimit {
				rels = rels[:finTruncLimit]
			}
			for _, e := range rels {
				if visited[e.nbr] {
					continue
				}
				visited[e.nbr] = true
				dist := cur.depth + 1
				for sig := range g.Rels(e.nbr, chickpeas.Incoming, "signIn") {
					if b, ok := blocked.Get(sig.Neighbor); ok && b {
						rows = append(rows, []value.Value{
							value.Int(cols.oid(e.nbr)), value.Int(int64(dist)),
							value.Int(cols.oid(sig.Neighbor)), value.Str("Medium"),
						})
					}
				}
				queue = append(queue, qe{e.nbr, dist, e.ts})
			}
		}
		return sortTruncate(rows, 0, func(a, b []value.Value) bool {
			a1, _ := a[1].AsInt()
			b1, _ := b[1].AsInt()
			a0, _ := a[0].AsInt()
			b0, _ := b[0].AsInt()
			a2, _ := a[2].AsInt()
			b2, _ := b[2].AsInt()
			return cmpChain(
				cmpI64Asc(a1, b1),
				cmpI64Asc(a0, b0),
				cmpI64Asc(a2, b2),
			)
		}), nil
	}, nil
}

// finCR2 -- fund gathered from loan-applying accounts: reverse-trace
// each owned account (<=3 hops, monotonic backward time), then per
// upstream account sum the loan amounts/balances deposited into it;
// [otherId, sumLoanAmount, sumLoanBalance].
func finCR2(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	person, err := finNode(g, "Person", finSeedOwner)
	if err != nil {
		return nil, err
	}
	loanAmt, ok := g.ColIndexed("loanAmount")
	if !ok {
		return nil, fmt.Errorf("node column loanAmount missing")
	}
	loanBal, ok := g.ColIndexed("balance")
	if !ok {
		return nil, fmt.Errorf("node column balance missing")
	}
	la, lb := loanAmt.F64(), loanBal.F64()
	return func() ([][]value.Value, error) {
		type sums struct{ amt, bal float64 }
		byAcct := map[chickpeas.NodeID]*sums{}
		var rels []tsRel
		loans := map[chickpeas.NodeID]bool{}
		for own := range g.Rels(person, chickpeas.Outgoing, "own") {
			owned := own.Neighbor
			visited := map[chickpeas.NodeID]bool{owned: true}
			type qe struct {
				node   chickpeas.NodeID
				depth  uint32
				lastTS int64
			}
			queue := []qe{{owned, 0, math.MaxInt64}}
			for len(queue) > 0 {
				cur := queue[0]
				queue = queue[1:]
				if cur.depth >= 3 {
					continue
				}
				rels = rels[:0]
				for r := range g.Rels(cur.node, chickpeas.Incoming, "transfer") {
					rels = append(rels, tsRel{cols.relTS(r.Pos), r.Neighbor})
				}
				sortTSRels(rels, false)
				if len(rels) > finTruncLimit {
					rels = rels[:finTruncLimit]
				}
				for _, e := range rels {
					if e.ts < finWS || e.ts > finWE || e.ts >= cur.lastTS {
						continue
					}
					if !visited[e.nbr] {
						visited[e.nbr] = true
						queue = append(queue, qe{e.nbr, cur.depth + 1, e.ts})
					}
				}
			}
			for acct := range visited {
				if acct == owned {
					continue
				}
				clear(loans)
				var amt, bal float64
				for dep := range g.Rels(acct, chickpeas.Incoming, "deposit") {
					ts := cols.relTS(dep.Pos)
					if ts < finWS || ts > finWE {
						continue
					}
					if !loans[dep.Neighbor] {
						loans[dep.Neighbor] = true
						if v, ok := la.Get(dep.Neighbor); ok {
							amt += v
						}
						if v, ok := lb.Get(dep.Neighbor); ok {
							bal += v
						}
					}
				}
				if len(loans) > 0 {
					e := byAcct[acct]
					if e == nil {
						e = &sums{}
						byAcct[acct] = e
					}
					e.amt += amt
					e.bal += bal
				}
			}
		}
		rows := make([][]value.Value, 0, len(byAcct))
		for acct, s := range byAcct {
			rows = append(rows, []value.Value{value.Int(cols.oid(acct)), value.Float(s.amt), value.Float(s.bal)})
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

// finCR3 -- shortest in-window transfer path length between the seed
// account and the recorded destination; [[hops]] (-1 unreachable).
func finCR3(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	src, err := finNode(g, "Account", finSeedAccount)
	if err != nil {
		return nil, err
	}
	dst, err := finNode(g, "Account", finSeedDst)
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		weight := func(_ chickpeas.NodeID, r chickpeas.RelRef) float64 {
			ts := cols.relTS(r.Pos)
			if ts >= finWS && ts <= finWE {
				return 1.0
			}
			return inf
		}
		if cost, ok := g.WeightedShortestPath(src, dst, chickpeas.Outgoing, g.Match("transfer"), weight); ok && finite(cost) {
			return [][]value.Value{{value.Int(int64(cost))}}, nil
		}
		return [][]value.Value{{value.Int(int64(-1))}}, nil
	}, nil
}

// finCR4 -- time-ordered transfer cycles back to the seed (amounts >=
// 1000, cycle within the 90-day window, <=6 nodes, capped at 1000
// cycles); one row per cycle's id sequence.
func finCR4(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	account, err := finNode(g, "Account", finSeedCycle)
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		const maxCycleLen, maxCycles = 6, 1000
		const minAmount = 1000.0
		var cycles [][]chickpeas.NodeID
		path := []chickpeas.NodeID{account}
		onPath := map[chickpeas.NodeID]bool{account: true}
		var dfs func(node chickpeas.NodeID, lastTS int64, firstTS int64, haveFirst bool)
		dfs = func(node chickpeas.NodeID, lastTS int64, firstTS int64, haveFirst bool) {
			if len(path) > maxCycleLen || len(cycles) >= maxCycles {
				return
			}
			for r := range g.Rels(node, chickpeas.Outgoing, "transfer") {
				ts := cols.relTS(r.Pos)
				if ts <= lastTS || cols.relAmt(r.Pos) < minAmount {
					continue
				}
				f0 := ts
				if haveFirst {
					f0 = firstTS
				}
				if ts-f0 > finWindowMS {
					continue
				}
				if r.Neighbor == account {
					if len(path) >= 2 {
						cycles = append(cycles, append([]chickpeas.NodeID(nil), path...))
					}
					continue
				}
				if onPath[r.Neighbor] {
					continue
				}
				path = append(path, r.Neighbor)
				onPath[r.Neighbor] = true
				dfs(r.Neighbor, ts, f0, true)
				path = path[:len(path)-1]
				delete(onPath, r.Neighbor)
			}
		}
		dfs(account, math.MinInt64, 0, false)
		rows := make([][]value.Value, len(cycles))
		for i, cy := range cycles {
			row := make([]value.Value, len(cy))
			for j, n := range cy {
				row[j] = value.Int(cols.oid(n))
			}
			rows[i] = row
		}
		return rows, nil
	}, nil
}

// finCR5 -- exact downstream transfer traces from the owner's accounts
// (<=3 hops, strictly increasing timestamps, acyclic); each trace is
// one single-list-cell row (the manifest's unwrap1 norm flattens it,
// matching the GQL twin's list projection).
func finCR5(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	person, err := finNode(g, "Person", finSeedOwner)
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		var all [][]chickpeas.NodeID
		var dfs func(node chickpeas.NodeID, lastTS int64, path *[]chickpeas.NodeID, visited map[chickpeas.NodeID]bool, depth int)
		dfs = func(node chickpeas.NodeID, lastTS int64, path *[]chickpeas.NodeID, visited map[chickpeas.NodeID]bool, depth int) {
			if depth >= 3 {
				return
			}
			byNeighbor := map[chickpeas.NodeID]int64{}
			for r := range g.Rels(node, chickpeas.Outgoing, "transfer") {
				ts := cols.relTS(r.Pos)
				if ts >= finWS && ts <= finWE && ts > lastTS && !visited[r.Neighbor] {
					if cur, ok := byNeighbor[r.Neighbor]; !ok || ts < cur {
						byNeighbor[r.Neighbor] = ts
					}
				}
			}
			cands := make([]tsRel, 0, len(byNeighbor))
			for n, ts := range byNeighbor {
				cands = append(cands, tsRel{ts, n})
			}
			if len(cands) > finTruncLimit {
				sortTSRels(cands, false)
				cands = cands[:finTruncLimit]
			}
			for _, c := range cands {
				*path = append(*path, c.nbr)
				visited[c.nbr] = true
				all = append(all, append([]chickpeas.NodeID(nil), *path...))
				dfs(c.nbr, c.ts, path, visited, depth+1)
				*path = (*path)[:len(*path)-1]
				delete(visited, c.nbr)
			}
		}
		for r := range g.Rels(person, chickpeas.Outgoing, "own") {
			start := r.Neighbor
			path := []chickpeas.NodeID{start}
			visited := map[chickpeas.NodeID]bool{start: true}
			dfs(start, math.MinInt64, &path, visited, 0)
		}
		// Dedup node sequences (parallel rels collapse to one path).
		sortByLess(all, func(a, b []chickpeas.NodeID) bool {
			for k := 0; k < len(a) && k < len(b); k++ {
				if a[k] != b[k] {
					return a[k] < b[k]
				}
			}
			return len(a) < len(b)
		})
		var rows [][]value.Value
		for i, p := range all {
			if i > 0 && slicesEqualNodes(all[i-1], p) {
				continue
			}
			ids := make([]value.Value, len(p))
			for j, n := range p {
				ids[j] = value.Int(cols.oid(n))
			}
			rows = append(rows, []value.Value{value.List(ids)})
		}
		return rows, nil
	}, nil
}

// slicesEqualNodes reports whether two node paths are identical.
func slicesEqualNodes(a, b []chickpeas.NodeID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// finCR6 -- withdrawal after many-to-one transfer (single-hop
// cross-engine reduction): the card's in-window withdrawals bound a
// fan-in of incoming transfers, summed per source; [srcId,
// sumRel1Amount, sumRel2Amount].
func finCR6(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	card, err := finNode(g, "Account", finSeedCard)
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		const threshold1, threshold2 = 0.0, 0.0
		var totalWithdraw float64
		lastWithdraw := int64(math.MinInt64)
		haveWithdraw := false
		for r := range g.Rels(card, chickpeas.Outgoing, "withdraw") {
			ts := cols.relTS(r.Pos)
			amt := cols.relAmt(r.Pos)
			if ts >= finWS && ts <= finWE && amt > threshold2 {
				totalWithdraw += amt
				if ts > lastWithdraw {
					lastWithdraw = ts
				}
				haveWithdraw = true
			}
		}
		if !haveWithdraw {
			return [][]value.Value{}, nil
		}
		type inRel struct {
			ts  int64
			amt float64
			src chickpeas.NodeID
		}
		var inRels []inRel
		for r := range g.Rels(card, chickpeas.Incoming, "transfer") {
			ts := cols.relTS(r.Pos)
			amt := cols.relAmt(r.Pos)
			if ts >= finWS && ts <= finWE && amt > threshold1 && ts <= lastWithdraw {
				inRels = append(inRels, inRel{ts, amt, r.Neighbor})
			}
		}
		if len(inRels) > finTruncLimit {
			sortByLess(inRels, func(a, b inRel) bool { return a.ts > b.ts })
			inRels = inRels[:finTruncLimit]
		}
		bySrc := map[chickpeas.NodeID]float64{}
		for _, r := range inRels {
			bySrc[r.src] += r.amt
		}
		rows := make([][]value.Value, 0, len(bySrc))
		for s, a := range bySrc {
			rows = append(rows, []value.Value{value.Int(cols.oid(s)), value.Float(a), value.Float(totalWithdraw)})
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
