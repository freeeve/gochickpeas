// The GQL parity manifest (rustychickpeas-ldbc's viz/data/gql_variants.tsv,
// their task 258): one row per query cell, carrying the graph to run
// against, the reference-row hash that gates emission, and the norm ops to
// apply before hashing.

package ldbc

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// ManifestRow is one gql_variants.tsv row.
type ManifestRow struct {
	Family  string
	Query   string
	Variant string
	Graph   string
	RefHash string
	Norm    string
	GQL     string
}

// Blocked reports whether the ldbc side marked this row not-runnable (a
// "blocked:" prefix on the query text); the runner must skip it loudly
// and emit nothing.
func (r ManifestRow) Blocked() bool {
	return strings.HasPrefix(r.GQL, "blocked:")
}

// LoadManifest reads a gql_variants.tsv: tab-separated, 7 columns,
// #-prefixed and blank lines skipped.
func LoadManifest(path string) ([]ManifestRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rows []ManifestRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if strings.TrimSpace(text) == "" || strings.HasPrefix(text, "#") {
			continue
		}
		cols := strings.Split(text, "\t")
		if len(cols) != 7 {
			return nil, fmt.Errorf("%s:%d: %d columns, want 7", path, line, len(cols))
		}
		rows = append(rows, ManifestRow{
			Family: cols[0], Query: cols[1], Variant: cols[2], Graph: cols[3],
			RefHash: cols[4], Norm: cols[5], GQL: cols[6],
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}
