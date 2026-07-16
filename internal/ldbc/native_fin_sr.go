// Native FinBench Simple Reads SR1-SR6 -- ports of rustychickpeas-ldbc
// src/finbench/sr.rs (account lookup, transfer aggregates, blocked
// ratios, threshold groupings, shared-source detection). All anchor on
// the recorded seed account with the full window and threshold 1000.

package ldbc

import (
	"fmt"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql/value"
)

const finSRThreshold = 1000.0

func init() {
	registerNativeV("FinBench", "SR1", finSR1)
	registerNativeV("FinBench", "SR2", finSR2)
	registerNativeV("FinBench", "SR3", finSR3)
	registerNativeV("FinBench", "SR4", finSR4)
	registerNativeV("FinBench", "SR5", finSR5)
	registerNativeV("FinBench", "SR6", finSR6)
}

// finSeedAccountNode resolves the shared SR seed account.
func finSeedAccountNode(g *chickpeas.Snapshot) (chickpeas.NodeID, error) {
	return finNode(g, "Account", finSeedAccount)
}

// finSR1 -- exact account query; [[createTime, isBlocked, type]].
func finSR1(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	account, err := finSeedAccountNode(g)
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		createTime := g.Prop(account, "createTime").I64Or(0)
		blocked := g.Prop(account, "isBlocked").BoolOr(false)
		typ := g.Prop(account, "type").StrOr("")
		return [][]value.Value{{value.Int(createTime), value.Bool(blocked), value.Str(typ)}}, nil
	}, nil
}

// finSR2 -- transfer-ins and outs; [[sumOut, maxOut, numOut, sumIn,
// maxIn, numIn]] (max -1.0 for an empty side).
func finSR2(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	account, err := finSeedAccountNode(g)
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		agg := func(dir chickpeas.Direction) (float64, float64, int64) {
			var sum float64
			maxAmt := -1.0
			var n int64
			for r := range g.Rels(account, dir, "transfer") {
				ts := cols.relTS(r.Pos)
				if ts >= finWS && ts <= finWE {
					amt := cols.relAmt(r.Pos)
					sum += amt
					if n == 0 || amt > maxAmt {
						maxAmt = amt
					}
					n++
				}
			}
			if n == 0 {
				maxAmt = -1.0
			}
			return sum, maxAmt, n
		}
		so, mo, no := agg(chickpeas.Outgoing)
		si, mi, ni := agg(chickpeas.Incoming)
		return [][]value.Value{{
			value.Float(so), value.Float(mo), value.Int(no),
			value.Float(si), value.Float(mi), value.Int(ni),
		}}, nil
	}, nil
}

// finSR3 -- blocked-source ratio of in-window transfer-ins over the
// threshold; [[ratio]] (-1.0 when none qualify).
func finSR3(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	account, err := finSeedAccountNode(g)
	if err != nil {
		return nil, err
	}
	blockedCol, ok := g.ColIndexed("isBlocked")
	if !ok {
		return nil, fmt.Errorf("node column isBlocked missing")
	}
	blocked := blockedCol.Bool()
	return func() ([][]value.Value, error) {
		const threshold = 0.0
		var total, blk int64
		for r := range g.Rels(account, chickpeas.Incoming, "transfer") {
			ts := cols.relTS(r.Pos)
			if ts >= finWS && ts <= finWE && cols.relAmt(r.Pos) > threshold {
				total++
				if b, ok := blocked.Get(r.Neighbor); ok && b {
					blk++
				}
			}
		}
		if total == 0 {
			return [][]value.Value{{value.Float(-1.0)}}, nil
		}
		return [][]value.Value{{value.Float(round3f(float64(blk) / float64(total)))}}, nil
	}, nil
}

// srThresholdGroup groups in-window transfers over the threshold by
// far endpoint; [farId, numEdges, sumAmount], sum desc / id asc.
func srThresholdGroup(g *chickpeas.Snapshot, cols finCols, account chickpeas.NodeID, dir chickpeas.Direction) [][]value.Value {
	type agg struct {
		n   int64
		sum float64
	}
	byFar := map[chickpeas.NodeID]*agg{}
	for r := range g.Rels(account, dir, "transfer") {
		ts := cols.relTS(r.Pos)
		amt := cols.relAmt(r.Pos)
		if ts >= finWS && ts <= finWE && amt > finSRThreshold {
			e := byFar[r.Neighbor]
			if e == nil {
				e = &agg{}
				byFar[r.Neighbor] = e
			}
			e.n++
			e.sum += amt
		}
	}
	rows := make([][]value.Value, 0, len(byFar))
	for far, a := range byFar {
		rows = append(rows, []value.Value{value.Int(cols.oid(far)), value.Int(a.n), value.Float(a.sum)})
	}
	return sortTruncate(rows, 0, func(a, b []value.Value) bool {
		a2, _ := a[2].AsFloat()
		b2, _ := b[2].AsFloat()
		a0, _ := a[0].AsInt()
		b0, _ := b[0].AsInt()
		return cmpChain(
			cmpF64Desc(a2, b2),
			cmpI64Asc(a0, b0),
		)
	})
}

// finSR4 -- transfer-outs over the threshold grouped by destination.
func finSR4(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	account, err := finSeedAccountNode(g)
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		return srThresholdGroup(g, cols, account, chickpeas.Outgoing), nil
	}, nil
}

// finSR5 -- transfer-ins over the threshold grouped by source.
func finSR5(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	account, err := finSeedAccountNode(g)
	if err != nil {
		return nil, err
	}
	return func() ([][]value.Value, error) {
		return srThresholdGroup(g, cols, account, chickpeas.Incoming), nil
	}, nil
}

// finSR6 -- blocked accounts sharing an in-window transfer source with
// the seed; [dstId] ascending.
func finSR6(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
	cols, err := finColsOf(g)
	if err != nil {
		return nil, err
	}
	account, err := finSeedAccountNode(g)
	if err != nil {
		return nil, err
	}
	blockedCol, ok := g.ColIndexed("isBlocked")
	if !ok {
		return nil, fmt.Errorf("node column isBlocked missing")
	}
	blocked := blockedCol.Bool()
	return func() ([][]value.Value, error) {
		inWindow := func(pos uint32) bool {
			ts := cols.relTS(pos)
			return ts >= finWS && ts <= finWE
		}
		result := map[chickpeas.NodeID]bool{}
		for r1 := range g.Rels(account, chickpeas.Incoming, "transfer") {
			if !inWindow(r1.Pos) {
				continue
			}
			for r2 := range g.Rels(r1.Neighbor, chickpeas.Outgoing, "transfer") {
				if !inWindow(r2.Pos) {
					continue
				}
				dst := r2.Neighbor
				if dst == account {
					continue
				}
				if b, ok := blocked.Get(dst); ok && b {
					result[dst] = true
				}
			}
		}
		rows := make([][]value.Value, 0, len(result))
		for dst := range result {
			rows = append(rows, []value.Value{value.Int(cols.oid(dst))})
		}
		return sortTruncate(rows, 0, func(a, b []value.Value) bool {
			a0, _ := a[0].AsInt()
			b0, _ := b[0].AsInt()
			return a0 < b0
		}), nil
	}, nil
}
