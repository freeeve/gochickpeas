// Token kinds and the parse error type for the hand-written GQL lexer.
package parser

import "fmt"

// TokKind discriminates a token.
type TokKind uint8

const (
	// TokEOF terminates the stream.
	TokEOF TokKind = iota
	// TokIdent is an identifier or (case-insensitive) keyword; Text keeps
	// the original spelling.
	TokIdent
	// TokInt is an integer literal.
	TokInt
	// TokFloat is a float literal (digits '.' digits).
	TokFloat
	// TokStr is a quoted string; Text is the content, no escape processing.
	TokStr
	// TokParam is $name; Text is the name (not reserved-word checked).
	TokParam
	// Punctuation and operators.
	TokLParen   // (
	TokRParen   // )
	TokLBracket // [
	TokRBracket // ]
	TokLBrace   // {
	TokRBrace   // }
	TokComma    // ,
	TokDot      // .
	TokDotDot   // ..
	TokQuestion
	TokPercent
	TokColon // :
	TokPipe  // |
	TokAmp   // &
	TokBang  // !
	TokEq    // =
	TokNeq   // <>
	TokLt    // <
	TokLte   // <=
	TokGt    // >
	TokGte   // >=
	TokPlus  // +
	TokMinus // -
	TokStar  // *
	TokSlash // /
)

// Token is one lexed token; Pos is the byte offset in the input.
type Token struct {
	Kind TokKind
	Text string
	Pos  int
}

// Error is a parse error with the byte offset it was detected at. The root
// gql package wraps it with the ErrParse sentinel (this package cannot
// import gql -- that would be an import cycle).
type Error struct {
	Pos int
	Msg string
}

// Error formats the message with its position.
func (e *Error) Error() string { return fmt.Sprintf("at offset %d: %s", e.Pos, e.Msg) }

// errf builds a positioned parse error.
func errf(pos int, format string, args ...any) *Error {
	return &Error{Pos: pos, Msg: fmt.Sprintf(format, args...)}
}
