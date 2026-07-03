// Reference-row loading for the SPB family: unlike BI/IC/FinBench,
// whose refs are one JSON file per query, all 30 SPB oracle result sets
// live in one combined file (rustychickpeas-ldbc
// python/refs/spb/spb.parity.rust.json) as {"params":..., "queries":
// {q: {"kind":..., "rows":[...]}}}. Rows are FULL result sets with no
// significant order (their parity runner disables LIMITs), which
// rowhash/v1's sorted multiset encoding absorbs with no norm.

package ldbc

import (
	"encoding/json"
	"fmt"
	"os"
)

// spbRefFile mirrors the parity JSON's structure; params stay raw for
// callers that only need a value or two.
type spbRefFile struct {
	Params  map[string]json.RawMessage `json:"params"`
	Queries map[string]struct {
		Kind string `json:"kind"`
		Rows []any  `json:"rows"`
	} `json:"queries"`
}

// loadSPBRef decodes the combined parity JSON with json.Number cells.
func loadSPBRef(path string) (*spbRefFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.UseNumber()
	var ref spbRefFile
	if err := dec.Decode(&ref); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &ref, nil
}

// LoadSPBRefRows returns one SPB query's reference rows in the rowhash
// domain. The uris/uri_opt kinds store bare strings -- each becomes a
// one-cell row; array elements (kv/kvx/pairs/day_count/who_days) map
// through refCell unchanged.
func LoadSPBRefRows(path, query string) ([][]any, error) {
	ref, err := loadSPBRef(path)
	if err != nil {
		return nil, err
	}
	q, ok := ref.Queries[query]
	if !ok {
		return nil, fmt.Errorf("%s: no query %q", path, query)
	}
	rows := make([][]any, len(q.Rows))
	for i, r := range q.Rows {
		cell, err := refCell(r)
		if err != nil {
			return nil, fmt.Errorf("%s %s row %d: %w", path, query, i, err)
		}
		if row, ok := cell.([]any); ok {
			rows[i] = row
		} else {
			rows[i] = []any{cell}
		}
	}
	return rows, nil
}

// SPBRefHash is RowsHash over one SPB query's reference rows -- the
// value the native manifest's refhash column carries for Family=SPB.
func SPBRefHash(path, query string) (string, error) {
	rows, err := LoadSPBRefRows(path, query)
	if err != nil {
		return "", err
	}
	return RowsHash(rows)
}
