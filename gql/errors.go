// Package gql is a read-only GQL query engine over an immutable
// chickpeas.Snapshot -- the Go port of the rustychickpeas-cypher engine
// with an ISO GQL (read subset) surface. Pipeline: parse -> desugar ->
// bind -> plan -> execute, returning Rows.
package gql

import "errors"

// Sentinel error kinds, wrapped by the pipeline stages so callers can
// errors.Is-match the failing stage (port of the Rust CypherError enum).
var (
	// ErrParse marks a syntax error from the parser.
	ErrParse = errors.New("gql parse error")
	// ErrBind marks a semantic error (unknown variable, bad reference).
	ErrBind = errors.New("gql bind error")
	// ErrPlan marks a planning error (unsupported construct, bad plan input).
	ErrPlan = errors.New("gql plan error")
	// ErrEval marks a runtime evaluation error.
	ErrEval = errors.New("gql evaluation error")
)
