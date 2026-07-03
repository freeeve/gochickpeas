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
