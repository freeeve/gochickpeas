// Reference-row JSON loading for the native parity manifest: the ldbc
// side's python/refs/*.rust.json files hold each query's result rows as a
// JSON array of arrays. Loading must reproduce python json.load's type
// mapping (int stays integral, everything with a fraction or exponent is
// a float) so RowsHash over the loaded rows equals the manifest refhash
// computed by python's rowhash/v1.

package ldbc

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// refCell maps one decoded JSON value into the rowhash cell domain,
// resolving json.Number to int64 (no '.', 'e', 'E' in the literal) or
// float64, matching python's int/float split.
func refCell(v any) (any, error) {
	switch x := v.(type) {
	case nil, bool, string:
		return x, nil
	case json.Number:
		s := x.String()
		if !strings.ContainsAny(s, ".eE") {
			i, err := x.Int64()
			if err != nil {
				return nil, fmt.Errorf("ref int %q: %w", s, err)
			}
			return i, nil
		}
		f, err := x.Float64()
		if err != nil {
			return nil, fmt.Errorf("ref float %q: %w", s, err)
		}
		return f, nil
	case []any:
		out := make([]any, len(x))
		for i, c := range x {
			cell, err := refCell(c)
			if err != nil {
				return nil, err
			}
			out[i] = cell
		}
		return out, nil
	}
	return nil, fmt.Errorf("ref cell type %T unsupported", v)
}

// LoadRefRows reads a reference JSON file (array of row arrays) into
// rowhash rows.
func LoadRefRows(path string) ([][]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.UseNumber()
	var raw []any
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	rows := make([][]any, len(raw))
	for i, r := range raw {
		cells, ok := r.([]any)
		if !ok {
			return nil, fmt.Errorf("%s: row %d is %T, want array", path, i, r)
		}
		row, err := refCell(cells)
		if err != nil {
			return nil, fmt.Errorf("%s: row %d: %w", path, i, err)
		}
		rows[i] = row.([]any)
	}
	return rows, nil
}

// RefHash is RowsHash over a reference JSON file -- the value the native
// manifest's refhash column carries.
func RefHash(path string) (string, error) {
	rows, err := LoadRefRows(path)
	if err != nil {
		return "", err
	}
	return RowsHash(rows)
}
