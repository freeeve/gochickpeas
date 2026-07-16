// Package eval is the interpreted expression evaluator: the portable path
// that evaluates bound AST expressions per row against the graph seam.
// Port of the Rust engine's eval.rs; the columnar compiled path (M16)
// mirrors it and both must produce identical results.
package eval

import (
	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/value"
)

// Ctx carries the evaluation environment: the graph and the query's
// parameter values. It replaces the Rust engine's thread-local
// ParamScope/NamedParamScope -- explicit and threaded through every
// evaluation, so nested subquery evaluation and reuse across goroutines
// need no scope choreography.
type Ctx struct {
	G graph.Graph
	// Params are the auto-lifted parameter values, indexed by slot.
	Params []value.Value
	// Named are the explicit $name parameter values.
	Named map[string]value.Value
	// ForceInterp pins every expression to the interpreted path even on a
	// native-capable graph -- the differential test hook behind the
	// dual-path harness.
	ForceInterp bool
	// MatchEpoch is bumped once per match-call by the executor, so a
	// loop-invariant carried IN list's membership cache rebuilds per call
	// and is reused across that call's candidates (replaces the Rust
	// thread-local epoch).
	MatchEpoch uint64
	// subqShapes caches each subquery pattern's DFS shape and scratch for
	// this execution (params are fixed per Ctx, so the memoized level-0
	// scan stays valid; a fresh Ctx per run keeps prepared-plan reuse
	// safe).
	subqShapes map[*ast.Pattern]*subqueryShape
	// scopes caches each list-scope node's reusable inner scope (the slot
	// map and idx are lexically invariant across the node's per-row
	// evaluations; the row buffer is refilled each call). A tree AST never
	// evaluates one node re-entrantly, so a single buffer per node is safe.
	scopes map[ast.Expr]*scopeScratch
	// DecorTables shares decorrelated COUNT{} side tables across sibling
	// compiled subqueries within one execution, keyed by the subquery's
	// canonical identity (endpoint variable names substituted, so the same
	// subquery written against two outer variables collides) and the
	// resolved anchor node. Params are fixed per Ctx, so identities that
	// embed parameter slots stay valid for the Ctx's lifetime. DecorBuilds
	// counts table builds across the execution -- tests assert a shared
	// identity builds once, not once per sibling.
	DecorTables map[DecorTableKey]map[graph.NodeID]int
	DecorBuilds int
	// constCalls memoizes all-literal scalar calls for this execution: a
	// call whose every argument is a literal (params included -- fixed per
	// Ctx) evaluates once instead of per row, so a temporal constructor in
	// a subquery WHERE parses its ISO string once per execution rather
	// than once per visited row. A nil entry marks a known non-constant
	// call so the argument scan runs once per node. Sound on the same
	// invariant as the compile-layer fold: every scalar function is
	// deterministic (locked by the zero-arg fold tests); a future volatile
	// function must be excluded here as well as at foldFunc/constExpr.
	constCalls map[*ast.Func]*value.Value
	// argvStack backs the interpreter's function-call argument rows as
	// stack frames (see evalScalarFuncUncached): one growable slice per
	// execution instead of one slice per call.
	argvStack []value.Value
}

// DecorTableKey identifies one shared decorrelated side table.
type DecorTableKey struct {
	Canon  string
	Anchor uint32
}

// scopeScratch is one list-scope node's reused inner environment: the slot
// map and iteration-variable indices built once, and a row buffer refilled
// from the outer row on each evaluation.
type scopeScratch struct {
	slots   map[string]int
	idx     []int
	row     []value.Value
	baseLen int
}

// ParamValue resolves an auto-lifted slot; out of range is Null.
func (c *Ctx) ParamValue(slot uint32) value.Value {
	if int(slot) < len(c.Params) {
		return c.Params[slot]
	}
	return value.Null()
}

// NamedValue resolves an explicit $name parameter; an unsupplied name is
// Null (a missing parameter reads as null rather than an error).
func (c *Ctx) NamedValue(name string) value.Value {
	if v, ok := c.Named[name]; ok {
		return v
	}
	return value.Null()
}

// LitValue converts a parsed literal into a runtime value, resolving
// parameters from the context.
func LitValue(ctx *Ctx, l ast.Literal) value.Value {
	switch l.Kind {
	case ast.LitInt:
		return value.Int(l.I)
	case ast.LitFloat:
		return value.Float(l.F)
	case ast.LitStr:
		return value.Str(l.S)
	case ast.LitBool:
		return value.Bool(l.B)
	case ast.LitParam:
		return ctx.ParamValue(l.P)
	case ast.LitNamedParam:
		return ctx.NamedValue(l.S)
	default:
		return value.Null()
	}
}
