package chickpeas

import (
	"fmt"
	"os"
	"sort"
	"testing"
)

// TestColumnWidthCensus is the env-gated integer-column narrowing census
// (task 213's i64Vec lead): for every I64 column of the graph named by
// CENSUS_RCPG, report the value range, the offset-encoded width class it
// would fit, and the projected saving -- the measured ground for a
// narrow-column representation decision. Skips unless CENSUS_RCPG is set.
//
//	CENSUS_RCPG=path/to/graph.rcpg go test -run TestColumnWidthCensus -v .
func TestColumnWidthCensus(t *testing.T) {
	path := os.Getenv("CENSUS_RCPG")
	if path == "" {
		t.Skip("CENSUS_RCPG not set")
	}
	g, err := ReadRCPGFile(path)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		side, name    string
		n             int
		min, max      int64
		curB, narrowB int64
	}
	var rows []row
	widthOf := func(span uint64) int64 {
		switch {
		case span <= 0xFF:
			return 1
		case span <= 0xFFFF:
			return 2
		case span <= 0xFFFFFFFF:
			return 4
		}
		return 8
	}
	scan := func(side string, m map[PropertyKey]Column) {
		for k, col := range m {
			name, _ := g.atoms.Resolve(k)
			var vals []int64
			var extraB int64
			switch c := col.(type) {
			case denseI64Col:
				vals = c
			case sparseI64Col:
				vals = c.vals
				extraB = int64(len(c.ids)) * 4
			default:
				continue
			}
			if len(vals) == 0 {
				continue
			}
			mn, mx := vals[0], vals[0]
			for _, v := range vals {
				if v < mn {
					mn = v
				}
				if v > mx {
					mx = v
				}
			}
			w := widthOf(uint64(mx - mn))
			rows = append(rows, row{side, name, len(vals), mn, mx,
				int64(len(vals))*8 + extraB, int64(len(vals))*w + extraB})
		}
	}
	scan("node", g.columns)
	scan("rel", g.relColumns)
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].curB-rows[i].narrowB > rows[j].curB-rows[j].narrowB
	})
	var cur, nar int64
	for _, r := range rows {
		cur += r.curB
		nar += r.narrowB
		t.Logf("%-4s %-24s n=%-9d min=%-15d max=%-15d cur=%8.1fMB narrow=%8.1fMB save=%7.1fMB",
			r.side, r.name, r.n, r.min, r.max,
			float64(r.curB)/1e6, float64(r.narrowB)/1e6, float64(r.curB-r.narrowB)/1e6)
	}
	fmt.Printf("CENSUS-TOTAL i64 columns: current=%.1fMB narrowed=%.1fMB save=%.1fMB\n",
		float64(cur)/1e6, float64(nar)/1e6, float64(cur-nar)/1e6)
}
