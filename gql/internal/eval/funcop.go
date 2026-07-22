// The scalar-function registry: the FuncOp enumeration and the
// name-resolution helpers that map a (case-insensitive) function name to a
// resolved op. The per-row evaluation kernels live in funcs.go; carrying a
// resolved FuncOp lets the compiled path skip name dispatch.
package eval

import "strings"

// FuncOp is a resolved scalar function; the compiled path carries this
// instead of the name string so per-row evaluation skips name dispatch.
type FuncOp uint8

// Resolved scalar functions.
const (
	FuncDate FuncOp = iota
	FuncDateTime
	FuncLocalDateTime
	FuncDuration
	FuncLength
	FuncNodes
	FuncRels
	FuncSize
	FuncRange
	FuncLeft
	FuncRight
	FuncSubstring
	FuncID
	FuncAbs
	FuncCeil
	FuncFloor
	FuncRound
	FuncSign
	FuncSqrt
	FuncToFloat
	FuncToInteger
	FuncToString
	FuncToBoolean
	FuncCoalesce
	FuncLower
	FuncUpper
	FuncCharLength
	FuncCardinality
	FuncTrim
	FuncLTrim
	FuncRTrim
	FuncMod
	FuncPower
	FuncExp
	FuncLn
	FuncLog10
	FuncSin
	FuncCos
	FuncTan
	FuncAsin
	FuncAcos
	FuncAtan
	FuncDegrees
	FuncRadians
	FuncNullIf
	FuncHead
	FuncLast
	FuncTail
	FuncElementID
	FuncNormalize
	FuncIsNormalized
)

// ResolveFuncOp resolves a scalar-function name (case-insensitive); ok is
// false for an unknown function (which yields null at evaluation).
func ResolveFuncOp(name string) (FuncOp, bool) {
	switch strings.ToLower(name) {
	case "date":
		return FuncDate, true
	case "datetime", "zoned_datetime":
		return FuncDateTime, true
	case "localdatetime":
		return FuncLocalDateTime, true
	case "duration":
		return FuncDuration, true
	case "length":
		return FuncLength, true
	case "nodes":
		return FuncNodes, true
	case "rels", "relationships":
		return FuncRels, true
	case "size":
		return FuncSize, true
	case "range":
		return FuncRange, true
	case "left":
		return FuncLeft, true
	case "right":
		return FuncRight, true
	case "substring":
		return FuncSubstring, true
	case "id":
		return FuncID, true
	case "abs":
		return FuncAbs, true
	case "ceil":
		return FuncCeil, true
	case "floor":
		return FuncFloor, true
	case "round":
		return FuncRound, true
	case "sign":
		return FuncSign, true
	case "sqrt":
		return FuncSqrt, true
	case "tofloat":
		return FuncToFloat, true
	case "tointeger":
		return FuncToInteger, true
	case "tostring":
		return FuncToString, true
	case "toboolean":
		return FuncToBoolean, true
	case "coalesce":
		return FuncCoalesce, true
	case "lower", "tolower":
		return FuncLower, true
	case "upper", "toupper":
		return FuncUpper, true
	case "char_length", "character_length":
		return FuncCharLength, true
	case "cardinality":
		return FuncCardinality, true
	case "trim":
		return FuncTrim, true
	case "normalize":
		return FuncNormalize, true
	case "is_normalized":
		return FuncIsNormalized, true
	case "ltrim":
		return FuncLTrim, true
	case "rtrim":
		return FuncRTrim, true
	case "mod":
		return FuncMod, true
	case "power", "pow":
		return FuncPower, true
	case "exp":
		return FuncExp, true
	case "ln":
		return FuncLn, true
	case "log10":
		return FuncLog10, true
	case "sin":
		return FuncSin, true
	case "cos":
		return FuncCos, true
	case "tan":
		return FuncTan, true
	case "asin":
		return FuncAsin, true
	case "acos":
		return FuncAcos, true
	case "atan":
		return FuncAtan, true
	case "degrees":
		return FuncDegrees, true
	case "radians":
		return FuncRadians, true
	case "nullif":
		return FuncNullIf, true
	case "head":
		return FuncHead, true
	case "last":
		return FuncLast, true
	case "tail":
		return FuncTail, true
	case "element_id", "elementid":
		return FuncElementID, true
	}
	return 0, false
}

// IsKnownScalarFunc reports whether name is a scalar function the engine
// evaluates (case-insensitive): a ResolveFuncOp op or the graph-resolved
// startNode/endNode. The binder consults this to reject unknown function
// names at plan time rather than silently evaluating them to null.
func IsKnownScalarFunc(name string) bool {
	if _, ok := ResolveFuncOp(name); ok {
		return true
	}
	l := strings.ToLower(name)
	return l == "startnode" || l == "endnode" || l == "type" || l == "labels"
}
