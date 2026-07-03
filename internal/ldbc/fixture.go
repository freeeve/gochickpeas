// Package ldbc loads the Rust-exported LDBC SF1 expected-results fixture
// (rustychickpeas-ldbc task 256) and runs the Go kernels in the exact
// shapes it encodes, so the cross-check tests and the bench emitter share
// one implementation (gochickpeas task 012).
package ldbc

import (
	"encoding/json"
	"fmt"
	"os"
)

// Pair is one ["name", count] fixture cell -- label cardinalities and
// rel-type counts are dumped as sorted pairs, not JSON objects.
type Pair struct {
	Name  string
	Count uint64
}

// UnmarshalJSON decodes the two-element array form.
func (p *Pair) UnmarshalJSON(raw []byte) error {
	var cell []json.RawMessage
	if err := json.Unmarshal(raw, &cell); err != nil {
		return err
	}
	if len(cell) != 2 {
		return fmt.Errorf("pair has %d cells, want 2", len(cell))
	}
	if err := json.Unmarshal(cell[0], &p.Name); err != nil {
		return err
	}
	return json.Unmarshal(cell[1], &p.Count)
}

// Structural is the fixture's structural section: whole-graph facts the
// Rust engine reports for the same rcpg file.
type Structural struct {
	NodeCount          *uint32 `json:"node_count"`
	RelCount           *uint64 `json:"relationship_count"`
	CSRIDSpace         *uint32 `json:"csr_id_space"`
	LabelCardinalities []Pair  `json:"label_cardinalities"`
	RelCountByType     []Pair  `json:"relationship_count_by_type"`
}

// WSPCase is one weighted-shortest-path probe; CostBits is the f64 cost
// dumped as its to_bits() u64 pattern, nil when the pair is unreachable.
type WSPCase struct {
	Src      uint32  `json:"src"`
	Dst      uint32  `json:"dst"`
	CostBits *uint64 `json:"cost_bits"`
}

// Expected mirrors sf1_expected.json. Absent sections stay nil and are
// skipped by consumers -- the fixture grows over time, and the harness
// works from day one.
type Expected struct {
	Meta struct {
		Schema     string            `json:"schema"`
		CoreCommit string            `json:"core_commit"`
		Keys       map[string]string `json:"keys"`
	} `json:"meta"`
	Structural           *Structural `json:"structural"`
	NeighborGroups       [][]int64   `json:"neighbor_groups"`
	FoldViaTop100        [][]int64   `json:"fold_via_top100"`
	CommonNeighborCounts [][]int64   `json:"common_neighbor_counts"`
	Aggregate            *struct {
		ByBirthMonth   [][]int64 `json:"by_birth_month"`
		ByCreationYear [][]int64 `json:"by_creation_year"`
	} `json:"aggregate"`
	WeightedShortestPath []WSPCase `json:"weighted_shortest_path"`
}

// Load reads and decodes the fixture at path.
func Load(path string) (*Expected, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var exp Expected
	if err := json.Unmarshal(raw, &exp); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", path, err)
	}
	return &exp, nil
}
