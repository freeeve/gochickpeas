// value.Value mirror of the rowhash/v1 flow (rowhash.go). The native result
// migration moves kernels from boxed [][]any to zero-box
// [][]value.Value; these encode/norm/verify functions produce byte-identical
// canonical text to their [][]any twins, so the pinned parity hashes hold
// unchanged. Kept beside the boxed flow, not merged into it, so the two paths
// coexist during the family-by-family port and the [][]any twin can be deleted
// wholesale once every kernel is migrated.

package ldbc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/freeeve/gochickpeas/gql/value"
)

// CanonCellV encodes one value.Value cell per rowhash/v1, identical in output
// to CanonCell over the equivalent boxed value. Only the rowhash scalar kinds
// (null, bool, int, float, str) and lists are representable; any other kind is
// a hard error, matching CanonCell's "project a scalar" contract.
func CanonCellV(v value.Value) (string, error) {
	switch v.Kind() {
	case value.KindNull:
		return "null", nil
	case value.KindBool:
		if b, _ := v.AsBool(); b {
			return "1", nil
		}
		return "0", nil
	case value.KindInt:
		i, _ := v.AsInt()
		return strconv.FormatInt(i, 10), nil
	case value.KindFloat:
		f, _ := v.AsFloat()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return "", fmt.Errorf("rowhash: NaN/Inf cell unsupported (%v)", f)
		}
		s := strconv.FormatFloat(f, 'f', 6, 64)
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
		if s == "" || s == "-0" {
			s = "0"
		}
		return s, nil
	case value.KindStr:
		s, _ := v.AsStr()
		return encodeJSONString(s), nil
	case value.KindList:
		vs, _ := v.AsList()
		parts := make([]string, len(vs))
		for i, c := range vs {
			enc, err := CanonCellV(c)
			if err != nil {
				return "", err
			}
			parts[i] = enc
		}
		return "[" + strings.Join(parts, ",") + "]", nil
	}
	return "", fmt.Errorf("rowhash: unsupported value kind %d; project a scalar", v.Kind())
}

// EncodeRowsV is EncodeRows over value.Value rows: each row encoded as a JSON
// array string, the encoded rows sorted (byte order), newline-joined.
func EncodeRowsV(rows [][]value.Value) (string, error) {
	encoded := make([]string, len(rows))
	for i, r := range rows {
		parts := make([]string, len(r))
		for j, c := range r {
			enc, err := CanonCellV(c)
			if err != nil {
				return "", fmt.Errorf("row %d col %d: %w", i, j, err)
			}
			parts[j] = enc
		}
		encoded[i] = "[" + strings.Join(parts, ",") + "]"
	}
	sort.Strings(encoded)
	return strings.Join(encoded, "\n"), nil
}

// RowsHashV is RowsHash over value.Value rows -- the first 16 hex chars of
// SHA-256 over the canonical text.
func RowsHashV(rows [][]value.Value) (string, error) {
	text, err := EncodeRowsV(rows)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:16], nil
}

// round3V rounds float cells to 3 decimals, recursing into lists; other kinds
// pass through. Mirror of round3.
func round3V(v value.Value) value.Value {
	switch v.Kind() {
	case value.KindFloat:
		f, _ := v.AsFloat()
		return value.Float(math.Round(f*1000) / 1000)
	case value.KindList:
		vs, _ := v.AsList()
		out := make([]value.Value, len(vs))
		for i, c := range vs {
			out[i] = round3V(c)
		}
		return value.List(out)
	}
	return v
}

// ApplyNormV applies a manifest norm spec to value.Value rows before hashing,
// mirroring ApplyNorm: comma-separated ops, left to right; "-" or empty is the
// identity.
func ApplyNormV(rows [][]value.Value, norm string) ([][]value.Value, error) {
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
			next := make([][]value.Value, len(out))
			for r, row := range out {
				nr := make([]value.Value, len(row))
				copy(nr, row)
				if i < len(nr) {
					if ms, ok := nr[i].AsInt(); ok {
						nr[i] = value.Int(floorDiv(ms, msPerDay))
					}
				}
				next[r] = nr
			}
			out = next
		case op == "round3":
			next := make([][]value.Value, len(out))
			for r, row := range out {
				nr := make([]value.Value, len(row))
				for c, cell := range row {
					nr[c] = round3V(cell)
				}
				next[r] = nr
			}
			out = next
		case op == "unwrap1":
			next := make([][]value.Value, len(out))
			for r, row := range out {
				if len(row) != 1 {
					return nil, fmt.Errorf("rowhash: unwrap1 needs single-list-cell rows, got %d cells", len(row))
				}
				inner, ok := row[0].AsList()
				if !ok {
					return nil, fmt.Errorf("rowhash: unwrap1 needs single-list-cell rows, got kind %d", row[0].Kind())
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

// VerifyCellV is VerifyCell over value.Value rows: apply the norm, hash, and
// compare against the row's refhash. The value.Value native/gql result path
// verifies through this without ever boxing a cell into any.
func VerifyCellV(row ManifestRow, cells [][]value.Value) (match bool, detail string, err error) {
	normed, err := ApplyNormV(cells, row.Norm)
	if err != nil {
		return false, "", err
	}
	hash, err := RowsHashV(normed)
	if err != nil {
		return false, "", err
	}
	if hash != row.RefHash {
		return false, fmt.Sprintf("hash %s != ref %s (%d rows)", hash, row.RefHash, len(cells)), nil
	}
	return true, "", nil
}
