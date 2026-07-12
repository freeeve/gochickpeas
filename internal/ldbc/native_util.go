// Shared helpers for the native per-query kernels: date arithmetic,
// typed column access, and the top-k selection idioms the Rust kernels
// (rustychickpeas-ldbc src/{bi,interactive,finbench}) repeat. Property
// names follow the CANONICAL .rcpg exports (Person/Forum/City ids are
// the label-scoped `id` column; dates are epoch-ms `creationDate` plus
// the precomputed `day`/`ms`/`year` columns).

package ldbc

import (
	"fmt"
	"math"
	"slices"

	chickpeas "github.com/freeeve/gochickpeas"
)

// inf is the sentinel weight for "no usable edge" in the derived-weight
// shortest-path kernels (Q15/Q19/Q20, IC14).
var inf = math.Inf(1)

// finite mirrors the Rust kernels' is_finite filters on shortest-path
// costs.
func finite(d float64) bool { return !math.IsInf(d, 0) && !math.IsNaN(d) }

// dayFromCivil is days since 1970-01-01 for a proleptic-Gregorian date
// (Howard Hinnant's algorithm) -- the same helper the Rust kernels use,
// so date-window params are plain integer comparisons on the `day`
// column.
func dayFromCivil(y, m, d int64) int64 {
	if m <= 2 {
		y--
	}
	var era int64
	if y >= 0 {
		era = y / 400
	} else {
		era = (y - 399) / 400
	}
	yoe := y - era*400
	var moff int64
	if m > 2 {
		moff = m - 3
	} else {
		moff = m + 9
	}
	doy := (153*moff+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

// nodeI64Col resolves a node i64 column, erroring when the snapshot
// lacks it (a schema mismatch the caller should surface, not mask).
func nodeI64Col(g *chickpeas.Snapshot, key string) (chickpeas.I64Col, error) {
	c, ok := g.ColIndexed(key)
	if !ok {
		return chickpeas.I64Col{}, fmt.Errorf("node column %s missing", key)
	}
	return c.I64(), nil
}

// i64At reads an i64 column at n, defaulting absent cells to 0 (the
// Rust kernels' i64_or_zero).
func i64At(c chickpeas.I64Col, n chickpeas.NodeID) int64 {
	v, _ := c.Get(n)
	return v
}

// strAt reads a node's string property, defaulting to "".
func strAt(g *chickpeas.Snapshot, n chickpeas.NodeID, key string) string {
	s, _ := g.Prop(n, key).Str()
	return s
}

// nodeByName finds the unique node of a label with the given name.
func nodeByName(g *chickpeas.Snapshot, label, name string) (chickpeas.NodeID, bool) {
	return g.NodeWithLabelProperty(label, "name", name)
}

// nodeByID finds the unique node of a label with the given LDBC id.
func nodeByID(g *chickpeas.Snapshot, label string, id int64) (chickpeas.NodeID, bool) {
	return g.NodeWithLabelProperty(label, "id", id)
}

// firstNeighbor is Snapshot.FirstNeighbor with the ok dropped to a
// sentinel-free two-value form used pervasively below.
func creatorOf(g *chickpeas.Snapshot, m chickpeas.NodeID) (chickpeas.NodeID, bool) {
	return g.FirstNeighbor(m, chickpeas.Outgoing, "HAS_CREATOR")
}

// personsOfCountry folds every node located in a city of the country
// (unfiltered by label, matching the Rust kernels: organisations never
// contribute messages or knows edges downstream).
func personsOfCountry(g *chickpeas.Snapshot, country chickpeas.NodeID) map[chickpeas.NodeID]bool {
	out := map[chickpeas.NodeID]bool{}
	for city := range g.Neighbors(country, chickpeas.Incoming, "IS_PART_OF") {
		for p := range g.Neighbors(city, chickpeas.Incoming, "IS_LOCATED_IN") {
			out[p] = true
		}
	}
	return out
}

// sortTruncate sorts rows by less and keeps the top limit. The generic
// sort avoids sort.Slice's reflection-based swapper (a typedmemmove per
// element move), which dominated hot kernels; less runs at most twice
// per comparison, still far cheaper than the reflected swaps.
func sortTruncate(rows [][]any, limit int, less func(a, b []any) bool) [][]any {
	slices.SortFunc(rows, func(a, b []any) int {
		if less(a, b) {
			return -1
		}
		if less(b, a) {
			return 1
		}
		return 0
	})
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

// cmpChain resolves a sequence of orderings: each step returns <0/0/>0;
// the first non-zero decides.
func cmpChain(cmps ...int) bool {
	for _, c := range cmps {
		if c != 0 {
			return c < 0
		}
	}
	return false
}

// cmpI64Desc / cmpI64Asc / cmpF64Asc / cmpF64Desc / cmpStrAsc are the
// comparator steps for cmpChain.
func cmpI64Desc(a, b int64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return 0
}

func cmpI64Asc(a, b int64) int { return -cmpI64Desc(a, b) }

func cmpF64Desc(a, b float64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return 0
}

func cmpF64Asc(a, b float64) int { return -cmpF64Desc(a, b) }

func cmpStrAsc(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// sortByLess sorts through the generic sort with a less predicate,
// avoiding sort.Slice's reflection-based swapper (a typedmemmove per
// element move) -- the shared conversion target for the kernels' sorts.
func sortByLess[T any](xs []T, less func(a, b T) bool) {
	slices.SortFunc(xs, func(a, b T) int {
		if less(a, b) {
			return -1
		}
		if less(b, a) {
			return 1
		}
		return 0
	})
}
