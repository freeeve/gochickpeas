// Package semantics holds the passes between parsing and planning:
// desugar (AST normalization), the binder helpers (aggregate detection,
// column naming, reference validation), and auto-parameterization for
// plan-cache sharing. Ports of the Rust crate's desugar.rs / binder.rs /
// autoparam.rs.
package semantics

import "fmt"

// ErrKind says which pipeline sentinel the root package should wrap a
// semantic error with (package gql cannot be imported here -- cycle).
type ErrKind uint8

const (
	// KindBind is a binding error (unknown variable/function) -> ErrBind.
	KindBind ErrKind = iota
	// KindPlan is a planning restriction -> ErrPlan.
	KindPlan
)

// Error is a semantic error with the sentinel kind it maps to.
type Error struct {
	Kind ErrKind
	Msg  string
}

// Error implements the error interface.
func (e *Error) Error() string { return e.Msg }

// bindErrf is a KindBind error.
func bindErrf(format string, args ...any) *Error {
	return &Error{Kind: KindBind, Msg: fmt.Sprintf(format, args...)}
}

// planErrf is a KindPlan error.
func planErrf(format string, args ...any) *Error {
	return &Error{Kind: KindPlan, Msg: fmt.Sprintf(format, args...)}
}
