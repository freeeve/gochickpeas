// Package plan compiles a desugared query into the plan IR the executor
// consumes: a sequence of Segments split at each projection boundary, each
// a scan/expand chain plus a projection. This file holds the operator IR
// (port of the Rust plan/ir.rs, minus the recognizer-only kernel specs);
// planning logic lives in plan.go/build.go and the lowering helpers.
package plan

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
)

// ScanKind discriminates a ScanSource.
type ScanKind uint8

const (
	// ScanProperty is an indexed anchor: nodes of Label whose Key == Value.
	ScanProperty ScanKind = iota
	// ScanLabel is all nodes of Label.
	ScanLabel
	// ScanNodeID is a direct seek to the node whose id equals the Value
	// literal (an int or lifted param), recognized from WHERE id(n) = N.
	// The WHERE conjunct is kept and re-checked, so the result is identical
	// to the scan-then-filter.
	ScanNodeID
	// ScanNodeIDVar is a per-row seek to the node whose id is the integer
	// in row slot Slot, recognized from WHERE id(n) = boundVar.
	ScanNodeIDVar
	// ScanAll is every node.
	ScanAll
	// ScanArg is the node already bound in row slot Slot (carried in or
	// reused across MATCH clauses).
	ScanArg
	// ScanTextMatch is a substring-index candidate scan: nodes of Label
	// whose Field satisfies Mode (STARTS WITH/ENDS WITH/CONTAINS) against
	// the Value needle. Candidates are a superset; the kept WHERE conjunct
	// finalizes, so this is purely a faster source.
	ScanTextMatch
)

// ScanSource is where a Scan op's candidate nodes come from.
type ScanSource struct {
	Kind  ScanKind
	Label string
	Key   string      // ScanProperty
	Field string      // ScanTextMatch
	Mode  ast.BinOp   // ScanTextMatch
	Value ast.Literal // ScanProperty value / ScanNodeID id / ScanTextMatch needle
	Slot  int         // ScanArg / ScanNodeIDVar
}

// OpKind discriminates a BindOp.
type OpKind uint8

const (
	// OpScan binds a fresh node variable from a ScanSource.
	OpScan OpKind = iota
	// OpExpand traverses one relationship hop.
	OpExpand
	// OpVarExpand traverses a quantified (variable-length) hop. Bounded
	// (min>=1, max set) enumerates trails, one row per path; zero-length or
	// unbounded resolves the distinct reachable set via dedup'd BFS.
	OpVarExpand
)

// NoSlot marks an absent optional slot (rel variable, group index).
const NoSlot = -1

// BindOp is one operator of a MATCH stage's bind chain.
type BindOp struct {
	Kind   OpKind
	Slot   int        // OpScan: the bound node's slot
	Source ScanSource // OpScan

	From, To int  // expand endpoints (slots)
	Rebind   bool // To is already bound (join/cycle/carried-in)
	Dir      graph.Direction
	Types    []string
	RelSlot  int // slot for a named rel variable; NoSlot when unbound

	// Target-node constraints (all ops; scan uses Slot's own).
	Labels []string
	Props  []ast.PropEntry

	// OpVarExpand only.
	Min            uint64
	Max            *uint64 // nil = unbounded
	RelVar         string  // the pattern's rel variable name ("" anonymous)
	RelPred        *RelHopPred
	MonoHop        *MonoHopSpec
	DedupEndpoints bool
}

// RelHopPred is a per-hop relationship predicate lifted from
// all(r IN rels(e) WHERE pred) onto a var-expand: each traversed rel must
// satisfy Pred (which references only Var) or the hop is pruned.
type RelHopPred struct {
	Var  string
	Pred ast.Expr
}

// MonoHopSpec is a path-ordered monotonic constraint on a bounded
// var-expand: each hop's RelKey (an i64 property) must strictly continue
// the order vs the previous hop, pruned during the walk.
type MonoHopSpec struct {
	RelKey    string
	Ascending bool
}

// AggKind is an aggregate function kind.
type AggKind uint8

const (
	// AggCount is count(x) / count(*).
	AggCount AggKind = iota
	// AggSum is sum(x).
	AggSum
	// AggAvg is avg(x).
	AggAvg
	// AggMin is min(x).
	AggMin
	// AggMax is max(x).
	AggMax
	// AggCollect is collect(x).
	AggCollect
)

// AggCol is one aggregate output column.
type AggCol struct {
	Kind     AggKind
	Arg      ast.Expr // nil for count(*)
	Distinct bool
	OutIdx   int
}

// MatchStage is one MATCH clause's bind chain. Optional rows that fail to
// match survive with the stage's new variables left null (left join).
type MatchStage struct {
	Ops      []BindOp
	Where    ast.Expr // nil when absent
	Optional bool
	// PathBind is set for MATCH p = (a)-[...]->(b): assemble the named
	// path after the pattern binds.
	PathBind *PathBindSpec
}

// PathBindSpec says how to assemble a named path: the start node lives in
// FromSlot, the hop's rel(s) at RelsSlot (a rel for a fixed hop, a list
// for a quantified hop); the node sequence is reconstructed by walking
// Dir/Types from the start, and the path value binds to PathSlot.
type PathBindSpec struct {
	PathSlot int
	FromSlot int
	RelsSlot int
	Dir      graph.Direction
	Types    []string
}

// SpStage binds PathSlot to the minimum-hop path between the bound
// endpoint slots From and To. When All, the stage is row-expanding: one
// row per minimum-hop path.
type SpStage struct {
	PathSlot int
	From, To int
	Dir      graph.Direction
	Types    []string
	Max      *uint64 // nil = unbounded hop count
	Optional bool
	All      bool
	Weight   *ast.CostSpec // nil = hop-minimal
	// WeightVar is the path's rel variable when Weight is a per-edge
	// expression (bound per candidate hop).
	WeightVar string
	RelPred   *RelHopPred
}

// ProcKind discriminates a CallProc.
type ProcKind uint8

const (
	// ProcWcc is wcc(relType[, direction]).
	ProcWcc ProcKind = iota
	// ProcFtsSearch is fts.search(label, field, searchTerm).
	ProcFtsSearch
	// ProcGeoWithinRadius is geo.withinRadius(label, latField, longField, lat, lon, km).
	ProcGeoWithinRadius
	// ProcGeoWithinBBox is geo.withinBBox(label, latField, longField, minLat, minLon, maxLat, maxLon).
	ProcGeoWithinBBox
	// ProcBfs is algo.bfs(source[, directed]).
	ProcBfs
	// ProcPageRank is algo.pagerank([directed][, damping][, iterations]).
	ProcPageRank
	// ProcWccAll is algo.wcc().
	ProcWccAll
	// ProcCdlp is algo.cdlp([directed][, iterations][, seedProp]).
	ProcCdlp
	// ProcLcc is algo.lcc([directed]).
	ProcLcc
	// ProcSssp is algo.sssp(source[, directed][, weighted]).
	ProcSssp
)

// CallProc is a CALL procedure with validated, concrete arguments.
type CallProc struct {
	Kind      ProcKind
	RelType   string
	Direction graph.Direction
	Label     string
	Field     string
	Query     string
	LatField  string
	LonField  string
	Lat, Lon  float64
	Km        float64
	MinLat    float64
	MinLon    float64
	MaxLat    float64
	MaxLon    float64
	Source    graph.NodeID
	Directed  bool
	Weighted  bool
	Damping   float64
	Iters     uint32
	SeedProp  string // "" absent
}

// CallStage runs a procedure, binding the yielded columns.
type CallStage struct {
	Proc CallProc
	// NodeSlot is the yielded node column's slot (NoSlot if not yielded).
	NodeSlot int
	// ValueSlot is the yielded per-node scalar's slot (NoSlot if not
	// yielded; the search procedures have no scalar column).
	ValueSlot int
}

// UnwindStage is FOR x IN list: each input row evaluates List; a list
// emits one row per element bound to OutSlot, null emits none, any other
// scalar emits a single row bound to it.
type UnwindStage struct {
	List    ast.Expr
	OutSlot int
}

// CallSubqueryStage is CALL { subquery }: a correlated lateral join. Sub
// is planned with inCols = the import list, so its first segment carries
// the imported columns in slots 0..len(ImportSlots).
type CallSubqueryStage struct {
	Sub         *Plan
	ImportSlots []int
	OutSlots    []int
}

// Stage is one pipeline stage of a segment.
type Stage interface{ isStage() }

func (*MatchStage) isStage()        {}
func (*SpStage) isStage()           {}
func (*CallStage) isStage()         {}
func (*UnwindStage) isStage()       {}
func (*CallSubqueryStage) isStage() {}
