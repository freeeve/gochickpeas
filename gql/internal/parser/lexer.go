// The GQL lexer: whitespace-separated tokens, case-insensitive keywords
// (classified by the parser, not here), quoted strings with NO escape
// processing (a quote cannot appear inside a string -- same rule as the
// Rust engine's grammar), digits-dot-digits floats, and $name parameters.
package parser

// reserved is the reserved-word table: these never lex as identifiers used
// for variables, labels, keys, or aliases. Extends the Rust engine's list
// with the GQL statement keywords (filter/let/for/next/offset/shortest)
// and the GQL write/catalog keywords, which are reserved so a write
// statement fails with a clear read-only error instead of a generic one.
var reserved = map[string]bool{
	// Query structure (shared with the Rust engine's grammar).
	"distinct": true, "optional": true, "return": true, "exists": true,
	"order": true, "limit": true, "match": true, "where": true,
	"false": true, "yield": true, "with": true, "when": true,
	"then": true, "else": true, "case": true, "call": true,
	"skip": true, "null": true, "true": true, "desc": true,
	"asc": true, "end": true, "and": true, "not": true,
	"as": true, "by": true, "or": true, "is": true, "in": true,
	"union": true, "reduce": true, "unwind": true,
	// GQL statement keywords.
	"filter": true, "let": true, "for": true, "next": true,
	"offset": true, "shortest": true,
	// GQL write/catalog/session keywords -- reserved for clean rejection.
	"insert": true, "set": true, "delete": true, "remove": true,
	"create": true, "drop": true, "merge": true, "detach": true,
	"session": true, "commit": true, "rollback": true,
}

// writeKeywords are the reserved words that mark a write/catalog/session
// statement; seeing one leads with the read-only explanation.
var writeKeywords = map[string]bool{
	"insert": true, "set": true, "delete": true, "remove": true,
	"create": true, "drop": true, "merge": true, "detach": true,
	"session": true, "commit": true, "rollback": true,
}

// lex tokenizes the whole input. The only error is an unterminated string.
func lex(src string) ([]Token, *Error) {
	toks := make([]Token, 0, len(src)/4+4)
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case isDigit(c):
			start := i
			for i < len(src) && isDigit(src[i]) {
				i++
			}
			// A float needs digits on both sides of a single dot; `1..3`
			// keeps the int and leaves the range dots.
			if i+1 < len(src) && src[i] == '.' && isDigit(src[i+1]) {
				i++
				for i < len(src) && isDigit(src[i]) {
					i++
				}
				toks = append(toks, Token{TokFloat, src[start:i], start})
			} else {
				toks = append(toks, Token{TokInt, src[start:i], start})
			}
		case isIdentStart(c):
			start := i
			for i < len(src) && isIdentChar(src[i]) {
				i++
			}
			toks = append(toks, Token{TokIdent, src[start:i], start})
		case c == '\'' || c == '"':
			quote := c
			start := i
			i++
			for i < len(src) && src[i] != quote {
				i++
			}
			if i >= len(src) {
				return nil, errf(start, "unterminated string")
			}
			toks = append(toks, Token{TokStr, src[start+1 : i], start})
			i++
		case c == '$':
			start := i
			i++
			for i < len(src) && isIdentChar(src[i]) {
				i++
			}
			if i == start+1 {
				return nil, errf(start, "empty parameter name after '$'")
			}
			toks = append(toks, Token{TokParam, src[start+1 : i], start})
		default:
			kind, width, ok := punct(src, i)
			if !ok {
				return nil, errf(i, "unexpected character %q", string(c))
			}
			toks = append(toks, Token{kind, src[i : i+width], i})
			i += width
		}
	}
	toks = append(toks, Token{TokEOF, "", len(src)})
	return toks, nil
}

// punct matches punctuation/operators, longest first (<=, >=, <>, ..).
func punct(src string, i int) (TokKind, int, bool) {
	if i+1 < len(src) {
		switch src[i : i+2] {
		case "<=":
			return TokLte, 2, true
		case ">=":
			return TokGte, 2, true
		case "<>":
			return TokNeq, 2, true
		case "..":
			return TokDotDot, 2, true
		}
	}
	switch src[i] {
	case '(':
		return TokLParen, 1, true
	case ')':
		return TokRParen, 1, true
	case '[':
		return TokLBracket, 1, true
	case ']':
		return TokRBracket, 1, true
	case '{':
		return TokLBrace, 1, true
	case '}':
		return TokRBrace, 1, true
	case ',':
		return TokComma, 1, true
	case '.':
		return TokDot, 1, true
	case ':':
		return TokColon, 1, true
	case '|':
		return TokPipe, 1, true
	case '&':
		return TokAmp, 1, true
	case '!':
		return TokBang, 1, true
	case '=':
		return TokEq, 1, true
	case '<':
		return TokLt, 1, true
	case '>':
		return TokGt, 1, true
	case '+':
		return TokPlus, 1, true
	case '-':
		return TokMinus, 1, true
	case '*':
		return TokStar, 1, true
	case '/':
		return TokSlash, 1, true
	}
	return TokEOF, 0, false
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentChar(c byte) bool { return isIdentStart(c) || isDigit(c) }
