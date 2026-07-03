// rowhash/v1 -- portable canonical row-multiset hash for cross-engine
// parity gates. Port of rustychickpeas-ldbc's python/cypher/rowhash.py
// (the normative spec; its self-test vectors are reproduced in
// rowhash_test.go). The GQL manifest carries rows_hash(reference_rows);
// the runner recomputes the hash over its own rows after the row's norm
// ops -- equal hash = MATCH.

package ldbc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// msPerDay converts epoch-millis to epoch-days in the msday norm op.
const msPerDay = 86_400_000

// CanonCell encodes one result cell per rowhash/v1: nil, bool, int64,
// float64, string, or []any (recursive). Any other type is a hard error
// -- the query text must project scalars instead.
func CanonCell(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "null", nil
	case bool:
		if x {
			return "1", nil
		}
		return "0", nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return "", fmt.Errorf("rowhash: NaN/Inf cell unsupported (%v)", x)
		}
		s := strconv.FormatFloat(x, 'f', 6, 64)
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
		if s == "" || s == "-0" {
			s = "0"
		}
		return s, nil
	case string:
		return encodeJSONString(x), nil
	case []any:
		parts := make([]string, len(x))
		for i, c := range x {
			enc, err := CanonCell(c)
			if err != nil {
				return "", err
			}
			parts[i] = enc
		}
		return "[" + strings.Join(parts, ",") + "]", nil
	}
	return "", fmt.Errorf("rowhash: unsupported cell type %T (%v); project a scalar", v, v)
}

// encodeJSONString matches python json.dumps(s, ensure_ascii=False):
// minimal escaping with non-ASCII kept literal (UTF-8).
func encodeJSONString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// EncodeRows renders the canonical text form: each row encoded as a JSON
// array string, the encoded rows sorted (byte order), newline-joined.
func EncodeRows(rows [][]any) (string, error) {
	encoded := make([]string, len(rows))
	for i, r := range rows {
		enc, err := CanonCell(any([]any(r)))
		if err != nil {
			return "", fmt.Errorf("row %d: %w", i, err)
		}
		encoded[i] = enc
	}
	sort.Strings(encoded)
	return strings.Join(encoded, "\n"), nil
}

// RowsHash is the first 16 hex chars of SHA-256 over the canonical text
// -- the manifest/parity hash. The empty result set hashes the empty
// string: e3b0c44298fc1c14.
func RowsHash(rows [][]any) (string, error) {
	text, err := EncodeRows(rows)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:16], nil
}

// floorDiv is python's // for int64 (rounds toward negative infinity).
func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// round3 rounds float cells to 3 decimals, recursing into lists; other
// types pass through. Ties resolve by native rounding -- the spec allows
// it (half-way ties are not exercised by the corpus).
func round3(v any) any {
	switch x := v.(type) {
	case float64:
		return math.Round(x*1000) / 1000
	case []any:
		out := make([]any, len(x))
		for i, c := range x {
			out[i] = round3(c)
		}
		return out
	}
	return v
}

// ApplyNorm applies a manifest norm spec to result rows before hashing:
// comma-separated ops, left to right; "-" or empty is the identity.
//   - col<i>:msday -- integer-divide int column i by 86_400_000
//   - round3       -- round every float cell (into lists) to 3 decimals
//   - unwrap1      -- replace each single-list-cell row with that list
func ApplyNorm(rows [][]any, norm string) ([][]any, error) {
	if norm == "" || norm == "-" {
		return rows, nil
	}
	out := rows
	for op := range strings.SplitSeq(norm, ",") {
		op = strings.TrimSpace(op)
		switch {
		case strings.HasPrefix(op, "col") && strings.HasSuffix(op, ":msday"):
			i, err := strconv.Atoi(op[3 : len(op)-6])
			if err != nil {
				return nil, fmt.Errorf("rowhash: bad norm op %q", op)
			}
			next := make([][]any, len(out))
			for r, row := range out {
				nr := make([]any, len(row))
				copy(nr, row)
				if i < len(nr) {
					if ms, ok := nr[i].(int64); ok {
						nr[i] = floorDiv(ms, msPerDay)
					}
				}
				next[r] = nr
			}
			out = next
		case op == "round3":
			next := make([][]any, len(out))
			for r, row := range out {
				nr := make([]any, len(row))
				for c, cell := range row {
					nr[c] = round3(cell)
				}
				next[r] = nr
			}
			out = next
		case op == "unwrap1":
			next := make([][]any, len(out))
			for r, row := range out {
				if len(row) != 1 {
					return nil, fmt.Errorf("rowhash: unwrap1 needs single-list-cell rows, got %d cells", len(row))
				}
				inner, ok := row[0].([]any)
				if !ok {
					return nil, fmt.Errorf("rowhash: unwrap1 needs single-list-cell rows, got %T", row[0])
				}
				next[r] = inner
			}
			out = next
		default:
			return nil, fmt.Errorf("rowhash: unknown norm op %q", op)
		}
	}
	return out, nil
}
