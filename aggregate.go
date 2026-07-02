// Fluent parallel grouped reductions over i64 node columns: scan the nodes
// of one or more labels, filter, group by columns / bins / temporal
// components / label membership, and reduce each group to a count and an
// optional sum -- the fast path for analytic rollups. The scan runs in
// parallel via nodeset.ParFold.

package chickpeas

import (
	"errors"
	"fmt"
)

// ErrSchema reports an aggregation over an unknown column/label, a
// mistyped column, or an unsupported option combination.
var ErrSchema = errors.New("schema error")

// AggOp is a comparison operator for Aggregation filters.
type AggOp uint8

const (
	OpLt AggOp = iota
	OpLe
	OpGt
	OpGe
	OpEq
	OpNe
)

// ParseAggOp parses a comparison symbol (<, <=, >, >=, ==, !=).
func ParseAggOp(s string) (AggOp, error) {
	switch s {
	case "<":
		return OpLt, nil
	case "<=":
		return OpLe, nil
	case ">":
		return OpGt, nil
	case ">=":
		return OpGe, nil
	case "==":
		return OpEq, nil
	case "!=":
		return OpNe, nil
	}
	return 0, fmt.Errorf("%w: unknown comparison op %q (use <, <=, >, >=, ==, !=)", ErrSchema, s)
}

// Test applies the operator.
func (op AggOp) Test(a, b int64) bool {
	switch op {
	case OpLt:
		return a < b
	case OpLe:
		return a <= b
	case OpGt:
		return a > b
	case OpGe:
		return a >= b
	case OpEq:
		return a == b
	}
	return a != b
}

// TemporalUnit is a calendar/clock component of an epoch-millis (UTC) i64,
// for a temporal group dimension -- mirroring openCypher's .year/.month/...
type TemporalUnit uint8

const (
	UnitYear TemporalUnit = iota
	UnitMonth
	UnitDay
	UnitHour
	UnitMinute
	UnitSecond
)

func (u TemporalUnit) suffix() string {
	switch u {
	case UnitYear:
		return "year"
	case UnitMonth:
		return "month"
	case UnitDay:
		return "day"
	case UnitHour:
		return "hour"
	case UnitMinute:
		return "minute"
	}
	return "second"
}

// of extracts the component from an epoch-millis (UTC) value.
func (u TemporalUnit) of(millis int64) int64 {
	const msPerDay = 86_400_000
	msOfDay := ((millis % msPerDay) + msPerDay) % msPerDay
	switch u {
	case UnitHour:
		return msOfDay / 3_600_000
	case UnitMinute:
		return (msOfDay / 60_000) % 60
	case UnitSecond:
		return (msOfDay / 1000) % 60
	}
	days := millis / msPerDay
	if millis%msPerDay < 0 {
		days--
	}
	y, mo, d := civilFromDays(days)
	switch u {
	case UnitYear:
		return y
	case UnitMonth:
		return int64(mo)
	}
	return int64(d)
}

// civilFromDays is civil (year, month, day) from days since 1970-01-01
// (Howard Hinnant's algorithm), matching the Rust aggregate and the query
// engine's temporal path.
func civilFromDays(z int64) (int64, uint32, uint32) {
	z += 719_468
	era := z
	if z < 0 {
		era = z - 146_096
	}
	era /= 146_097
	doe := z - era*146_097
	yoe := (doe - doe/1460 + doe/36524 - doe/146_096) / 365
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d := uint32(doy - (153*mp+2)/5 + 1)
	m := uint32(mp + 3)
	if mp >= 10 {
		m = uint32(mp - 9)
	}
	if m <= 2 {
		y++
	}
	return y, m, d
}

type groupDimKind uint8

const (
	dimCol groupDimKind = iota
	dimBin
	dimComponent
	dimLabel
)

type groupDim struct {
	kind   groupDimKind
	column string
	bounds []int64
	unit   TemporalUnit
}

type projectedFilter struct {
	projection []NodeID
	column     string
	allowed    map[Value]struct{}
}

// Aggregation is a fluent parallel grouped reduction; build with
// Snapshot.Aggregate, chain the steps, then Run.
type Aggregation struct {
	g              *Snapshot
	labels         []string
	filters        []aggFilter
	having         []aggFilter
	byLabel        bool
	group          []groupDim
	sumCol         string
	hasSum         bool
	through        string
	throughDir     Direction
	hasThrough     bool
	hops           []Step
	neighborFilter map[NodeID]struct{}
	projFilters    []projectedFilter
	requirePresent []string
}

type aggFilter struct {
	column string
	op     AggOp
	value  int64
}

// AggRow is one output group: the key values in field order (the source
// label as its index when grouping by label), the row count, and the
// summed value (0 without a Sum column).
type AggRow struct {
	Key   []int64
	Count uint64
	Sum   int64
}

// AggResult is the outcome of Run.
type AggResult struct {
	// Total counts rows passing the population (Filter) predicates.
	Total uint64
	// Rows holds one AggRow per group, unordered.
	Rows []AggRow
	// Fields names each key position: "label", a column name, or
	// "{col}_bin" / "{col}_{unit}" / "neighbor" / "endpoint".
	Fields []string
}

// Aggregate starts a parallel grouped reduction over the given labels.
func (g *Snapshot) Aggregate(labels ...string) *Aggregation {
	return &Aggregation{g: g, labels: labels}
}

// Filter adds the population predicate `column op value`; rows passing all
// filters are counted in Total.
func (a *Aggregation) Filter(column string, op AggOp, value int64) *Aggregation {
	a.filters = append(a.filters, aggFilter{column: column, op: op, value: value})
	return a
}

// FilterVia keeps a source node only when column of its projected node
// (projection[node], e.g. a RootsVia array) is in allowed -- a membership
// test over any value type, applied with the scalar filters.
func (a *Aggregation) FilterVia(projection []NodeID, column string, allowed ...Value) *Aggregation {
	set := make(map[Value]struct{}, len(allowed))
	for _, v := range allowed {
		set[v] = struct{}{}
	}
	a.projFilters = append(a.projFilters, projectedFilter{projection: projection, column: column, allowed: set})
	return a
}

// Having adds a predicate applied to grouped rows only (after the
// population filters).
func (a *Aggregation) Having(column string, op AggOp, value int64) *Aggregation {
	a.having = append(a.having, aggFilter{column: column, op: op, value: value})
	return a
}

// ByLabel groups by the source node label (key = its index; field "label").
func (a *Aggregation) ByLabel() *Aggregation {
	a.byLabel = true
	return a
}

// By groups by an i64 column's value (field = the column name).
func (a *Aggregation) By(column string) *Aggregation {
	a.group = append(a.group, groupDim{kind: dimCol, column: column})
	return a
}

// Bin groups by a column bucketed at ascending bounds (bucket = count of
// bounds <= value; field = "{column}_bin").
func (a *Aggregation) Bin(column string, bounds ...int64) *Aggregation {
	a.group = append(a.group, groupDim{kind: dimBin, column: column, bounds: bounds})
	return a
}

// TemporalComponent groups by a temporal component of an epoch-millis
// column (field = "{column}_{unit}").
func (a *Aggregation) TemporalComponent(column string, unit TemporalUnit) *Aggregation {
	a.group = append(a.group, groupDim{kind: dimComponent, column: column, unit: unit})
	return a
}

// ByLabelMembership groups by membership of one label (key 0/1; field =
// the label name) -- the n:Label predicate as a dimension, distinct from
// ByLabel.
func (a *Aggregation) ByLabelMembership(label string) *Aggregation {
	a.group = append(a.group, groupDim{kind: dimLabel, column: label})
	return a
}

// RequirePresent keeps only source nodes carrying a value for column (the
// IS NOT NULL predicate), regardless of dtype.
func (a *Aggregation) RequirePresent(column string) *Aggregation {
	a.requirePresent = append(a.requirePresent, column)
	return a
}

// Sum also sums this i64 column per group.
func (a *Aggregation) Sum(column string) *Aggregation {
	a.sumCol, a.hasSum = column, true
	return a
}

// Through counts rels of relType/dir out of each filtered source instead
// of counting nodes, grouping additionally by the neighbor (field
// "neighbor"); Total still counts source nodes.
func (a *Aggregation) Through(relType string, dir Direction) *Aggregation {
	a.through, a.throughDir, a.hasThrough = relType, dir, true
	return a
}

// OnlyNeighbors restricts Through counting to these neighbors.
func (a *Aggregation) OnlyNeighbors(ids ...NodeID) *Aggregation {
	a.neighborFilter = make(map[NodeID]struct{}, len(ids))
	for _, id := range ids {
		a.neighborFilter[id] = struct{}{}
	}
	return a
}

// Hop appends a hop to a chained n-hop WALK count: Run then counts the
// walks of the whole hop sequence from each filtered source, one group per
// reachable final endpoint (field "endpoint"). A rel may be reused across
// hops, so this equals a relationship-unique path count only for shapes
// where no rel can repeat -- the caller restricts to such shapes. Mutually
// exclusive with Through.
func (a *Aggregation) Hop(relType string, dir Direction) *Aggregation {
	a.hops = append(a.hops, Step{Dir: dir, RelType: relType})
	return a
}
