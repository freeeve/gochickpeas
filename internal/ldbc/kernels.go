package ldbc

import (
	"fmt"
	"math"
	"slices"

	chickpeas "github.com/freeeve/gochickpeas"
)

// Kernel is one fixture-checked kernel: Rows runs the Go kernel on the
// loaded SF1 snapshot and renders the result in the fixture's row shape;
// Want pulls the matching fixture section (ok=false when absent). Rows is
// deterministic and side-effect free, so the bench emitter can time
// repeated calls of the same work the cross-check verified.
type Kernel struct {
	Name string
	Rows func(g *chickpeas.Snapshot) ([][]int64, error)
	Want func(exp *Expected) ([][]int64, bool)
}

// Kernels lists the six kernels in fixture order. Row encodings follow
// the fixture's meta.notes; weighted_shortest_path rows are normalized to
// [src, dst, reached, cost_bits] on both sides (see wspRow).
func Kernels() []Kernel {
	return []Kernel{
		{
			Name: "neighbor_groups",
			Rows: neighborGroupsRows,
			Want: func(exp *Expected) ([][]int64, bool) { return exp.NeighborGroups, exp.NeighborGroups != nil },
		},
		{
			Name: "fold_via",
			Rows: foldViaRows,
			Want: func(exp *Expected) ([][]int64, bool) { return exp.FoldViaTop100, exp.FoldViaTop100 != nil },
		},
		{
			Name: "common_neighbor_counts",
			Rows: commonNeighborRows,
			Want: func(exp *Expected) ([][]int64, bool) {
				return exp.CommonNeighborCounts, exp.CommonNeighborCounts != nil
			},
		},
		{
			Name: "aggregate_by_birth_month",
			Rows: byBirthMonthRows,
			Want: func(exp *Expected) ([][]int64, bool) {
				if exp.Aggregate == nil {
					return nil, false
				}
				return exp.Aggregate.ByBirthMonth, exp.Aggregate.ByBirthMonth != nil
			},
		},
		{
			Name: "aggregate_by_creation_year",
			Rows: byCreationYearRows,
			Want: func(exp *Expected) ([][]int64, bool) {
				if exp.Aggregate == nil {
					return nil, false
				}
				return exp.Aggregate.ByCreationYear, exp.Aggregate.ByCreationYear != nil
			},
		},
		{
			Name: "weighted_shortest_path",
			Rows: weightedShortestPathRows,
			Want: func(exp *Expected) ([][]int64, bool) {
				if exp.WeightedShortestPath == nil {
					return nil, false
				}
				rows := make([][]int64, len(exp.WeightedShortestPath))
				for i, c := range exp.WeightedShortestPath {
					bits, reached := uint64(0), int64(0)
					if c.CostBits != nil {
						bits, reached = *c.CostBits, 1
					}
					rows[i] = wspRow(c.Src, c.Dst, reached, bits)
				}
				return rows, true
			},
		},
	}
}

// DiffRows compares kernel output to the fixture section, reporting the
// row count mismatch or the first differing row.
func DiffRows(got, want [][]int64) error {
	if len(got) != len(want) {
		return fmt.Errorf("row count: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			return fmt.Errorf("row %d: got %v, want %v", i, got[i], want[i])
		}
	}
	return nil
}

// labelIDs is a label's node ids in ascending internal-id order.
func labelIDs(g *chickpeas.Snapshot, label string) ([]chickpeas.NodeID, error) {
	set, ok := g.NodesWithLabel(label)
	if !ok {
		return nil, fmt.Errorf("label %s missing", label)
	}
	return set.ToSlice(), nil
}

// neighborGroupsRows: BI Q4 shape -- Forum -HAS_MEMBER-> Person projected
// through (Outgoing IS_LOCATED_IN, Outgoing IS_PART_OF) to Country,
// top_by_size(100, tie=flid); [forum_id, largest_cohort_size] ranked.
func neighborGroupsRows(g *chickpeas.Snapshot) ([][]int64, error) {
	forums, err := labelIDs(g, "Forum")
	if err != nil {
		return nil, err
	}
	top := g.NeighborGroups(forums, g.Match("HAS_MEMBER"), chickpeas.Outgoing).
		Project(
			chickpeas.Step{Dir: chickpeas.Outgoing, RelType: "IS_LOCATED_IN"},
			chickpeas.Step{Dir: chickpeas.Outgoing, RelType: "IS_PART_OF"},
		).
		TopBySize(100, "flid")
	rows := make([][]int64, len(top))
	for i, s := range top {
		rows[i] = []int64{int64(s.Source), int64(s.Size)}
	}
	return rows, nil
}

// foldViaRows: REPLY_OF folded through neighbor_via(HAS_CREATOR,
// Outgoing); top 100 [a, b, count] by count desc then (a, b) asc.
func foldViaRows(g *chickpeas.Snapshot) ([][]int64, error) {
	hasCreator, ok := g.RelType("HAS_CREATOR")
	if !ok {
		return nil, fmt.Errorf("rel type HAS_CREATOR missing")
	}
	projection := g.NeighborVia(hasCreator, chickpeas.Outgoing)
	counts := g.FoldVia(g.Match("REPLY_OF"), chickpeas.Outgoing, projection)
	rows := make([][]int64, 0, len(counts))
	for pair, count := range counts {
		rows = append(rows, []int64{int64(pair.Lo), int64(pair.Hi), int64(count)})
	}
	sortByLess(rows, func(a, b []int64) bool {
		if a[2] != b[2] {
			return a[2] > b[2]
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		return a[1] < b[1]
	})
	if len(rows) > 100 {
		rows = rows[:100]
	}
	return rows, nil
}

// commonNeighborRows: sources = 50 smallest Person ids, Direction Both
// over KNOWS, targets = all Persons, self-pairs dropped; [s, t, count]
// sorted by (s, t).
func commonNeighborRows(g *chickpeas.Snapshot) ([][]int64, error) {
	personSet, ok := g.NodesWithLabel("Person")
	if !ok {
		return nil, fmt.Errorf("label Person missing")
	}
	persons := personSet.ToSlice()
	sources := persons[:min(50, len(persons))]
	counts := g.CommonNeighborCounts(sources, chickpeas.Both, g.Match("KNOWS"), personSet)
	rows := make([][]int64, 0, len(counts))
	for _, c := range counts {
		if c.Source == c.Target {
			continue
		}
		rows = append(rows, []int64{int64(c.Source), int64(c.Target), int64(c.Count)})
	}
	sortByLess(rows, func(a, b []int64) bool {
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		return a[1] < b[1]
	})
	return rows, nil
}

// aggRows renders an AggResult's single-key groups as [key, count, sum]
// sorted by key ascending. LDBC-scale totals cannot leave int64 range; a
// nil Sum here is an engine defect, and the panic is the loudest signal.
func aggRows(res *chickpeas.AggResult) [][]int64 {
	rows := make([][]int64, len(res.Rows))
	for i, r := range res.Rows {
		rows[i] = []int64{r.Key[0], int64(r.Count), *r.Sum}
	}
	sortByLess(rows, func(a, b []int64) bool { return a[0] < b[0] })
	return rows
}

// byBirthMonthRows: aggregate(Person).by(bmon).sum(pday);
// [bmon, count, sum_pday] sorted by bmon.
func byBirthMonthRows(g *chickpeas.Snapshot) ([][]int64, error) {
	res, err := g.Aggregate("Person").By("bmon").Sum("pday").Run()
	if err != nil {
		return nil, err
	}
	return aggRows(res), nil
}

// byCreationYearRows: aggregate(Post, Comment).temporal_component(ms,
// Year); [year, count, sum] sorted by year (sum is 0 -- no Sum column).
func byCreationYearRows(g *chickpeas.Snapshot) ([][]int64, error) {
	res, err := g.Aggregate("Post", "Comment").TemporalComponent("ms", chickpeas.UnitYear).Run()
	if err != nil {
		return nil, err
	}
	return aggRows(res), nil
}

// wspRow normalizes one weighted-shortest-path probe for DiffRows: the
// fixture's nullable cost_bits becomes an explicit reached flag plus the
// f64 bit pattern reinterpreted as int64 (0 when unreachable).
func wspRow(src, dst uint32, reached int64, costBits uint64) []int64 {
	return []int64{int64(src), int64(dst), reached, int64(costBits)}
}

// weightedShortestPathRows: 10 pairs (persons[i], persons[i+25]) for i in
// 0..10, Direction Both over KNOWS, uniform weight 1.0.
func weightedShortestPathRows(g *chickpeas.Snapshot) ([][]int64, error) {
	persons, err := labelIDs(g, "Person")
	if err != nil {
		return nil, err
	}
	if len(persons) < 35 {
		return nil, fmt.Errorf("only %d Person nodes, need 35", len(persons))
	}
	m := g.Match("KNOWS")
	unit := func(chickpeas.NodeID, chickpeas.RelRef) float64 { return 1.0 }
	rows := make([][]int64, 10)
	for i := range 10 {
		src, dst := persons[i], persons[i+25]
		cost, ok := g.WeightedShortestPath(src, dst, chickpeas.Both, m, unit)
		bits, reached := uint64(0), int64(0)
		if ok {
			bits, reached = math.Float64bits(cost), 1
		}
		rows[i] = wspRow(src, dst, reached, bits)
	}
	return rows, nil
}
