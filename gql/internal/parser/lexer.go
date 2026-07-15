// The GQL lexer: whitespace-separated tokens, case-insensitive keywords
// (classified by the parser, not here), quoted strings with quote
// doubling and backslash escapes, scientific-notation floats, // and
// /* */ comments, unicode identifiers, backtick-delimited identifiers,
// and $name parameters.
package parser

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

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
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			// Line comment: to end of line.
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			// Block comment (non-nesting), must terminate.
			start := i
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			if i+1 >= len(src) {
				return nil, errf(start, "unterminated block comment")
			}
			i += 2
		case isDigit(c):
			start := i
			isFloat := false
			for i < len(src) && isDigit(src[i]) {
				i++
			}
			// A float needs digits on both sides of a single dot; `1..3`
			// keeps the int and leaves the range dots.
			if i+1 < len(src) && src[i] == '.' && isDigit(src[i+1]) {
				isFloat = true
				i++
				for i < len(src) && isDigit(src[i]) {
					i++
				}
			}
			// Scientific notation: an exponent consumes only when digits
			// follow (with an optional sign), so `1e` stays int + ident.
			if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
				j := i + 1
				if j < len(src) && (src[j] == '+' || src[j] == '-') {
					j++
				}
				if j < len(src) && isDigit(src[j]) {
					isFloat = true
					i = j
					for i < len(src) && isDigit(src[i]) {
						i++
					}
				}
			}
			if isFloat {
				toks = append(toks, Token{TokFloat, src[start:i], start})
			} else {
				toks = append(toks, Token{TokInt, src[start:i], start})
			}
		case isIdentStart(c) || c >= utf8.RuneSelf && identStartRune(src, i):
			start := i
			for i < len(src) {
				if isIdentChar(src[i]) {
					i++
					continue
				}
				if src[i] >= utf8.RuneSelf {
					r, w := utf8.DecodeRuneInString(src[i:])
					if unicode.IsLetter(r) || unicode.IsDigit(r) {
						i += w
						continue
					}
				}
				break
			}
			toks = append(toks, Token{TokIdent, src[start:i], start})
		case c == '`':
			// Backtick-delimited identifier: any characters except a
			// backtick (labels/keys with spaces or reserved spellings --
			// the parser's reserved checks still apply by text).
			start := i
			i++
			for i < len(src) && src[i] != '`' {
				i++
			}
			if i >= len(src) {
				return nil, errf(start, "unterminated delimited identifier")
			}
			if i == start+1 {
				return nil, errf(start, "empty delimited identifier")
			}
			toks = append(toks, Token{TokIdent, src[start+1 : i], start})
			i++
		case c == '\'' || c == '"':
			text, next, err := lexString(src, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, Token{TokStr, text, i})
			i = next
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

// lexString scans a quoted string starting at src[i], handling quote
// doubling ('it”s') and backslash escapes (\\ \' \" \n \t \r, and
// \uXXXX). The fast path (no escapes) returns a source slice; escapes
// build once. An unknown escape is an error, not a silent literal.
func lexString(src string, i int) (string, int, *Error) {
	quote := src[i]
	start := i
	i++
	var b *strings.Builder
	seg := i
	flush := func(end int) {
		if b == nil {
			b = &strings.Builder{}
		}
		b.WriteString(src[seg:end])
	}
	for i < len(src) {
		c := src[i]
		switch {
		case c == quote:
			// Doubled quote = literal quote; otherwise the terminator.
			if i+1 < len(src) && src[i+1] == quote {
				flush(i)
				b.WriteByte(quote)
				i += 2
				seg = i
				continue
			}
			if b == nil {
				return src[start+1 : i], i + 1, nil
			}
			b.WriteString(src[seg:i])
			return b.String(), i + 1, nil
		case c == '\\':
			if i+1 >= len(src) {
				return "", 0, errf(i, "unterminated escape")
			}
			flush(i)
			esc := src[i+1]
			switch esc {
			case '\\', '\'', '"', '`':
				b.WriteByte(esc)
				i += 2
			case 'n':
				b.WriteByte('\n')
				i += 2
			case 't':
				b.WriteByte('\t')
				i += 2
			case 'r':
				b.WriteByte('\r')
				i += 2
			case 'u':
				if i+6 > len(src) {
					return "", 0, errf(i, "\\u needs four hex digits")
				}
				r := rune(0)
				for _, h := range src[i+2 : i+6] {
					d := hexVal(byte(h))
					if d < 0 {
						return "", 0, errf(i, "\\u needs four hex digits")
					}
					r = r<<4 | rune(d)
				}
				b.WriteRune(r)
				i += 6
			default:
				return "", 0, errf(i, "unknown string escape \\%c", esc)
			}
			seg = i
		default:
			i++
		}
	}
	return "", 0, errf(start, "unterminated string")
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// identStartRune reports whether the rune at src[i] can start an
// identifier (unicode letter).
func identStartRune(src string, i int) bool {
	r, _ := utf8.DecodeRuneInString(src[i:])
	return unicode.IsLetter(r)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentChar(c byte) bool { return isIdentStart(c) || isDigit(c) }
