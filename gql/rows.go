// The result surface: Rows, an iterator of named-column Row values. The
// executor materializes the result set and Rows streams it from the buffer
// (same contract as the Rust engine's lazy Rows).
package gql

import (
	"iter"

	"github.com/freeeve/gochickpeas/gql/value"
)

// Row is one result row: named columns mapped to values. The column slice
// is shared across every row of a result.
type Row struct {
	columns []string
	values  []value.Value
}

// Get returns the value of column name; ok is false when absent.
func (r Row) Get(name string) (value.Value, bool) {
	for i, c := range r.columns {
		if c == name {
			return r.values[i], true
		}
	}
	return value.Value{}, false
}

// GetAt returns the value at output position i; ok is false out of range.
func (r Row) GetAt(i int) (value.Value, bool) {
	if i < 0 || i >= len(r.values) {
		return value.Value{}, false
	}
	return r.values[i], true
}

// Values is the row's values in output-column order.
func (r Row) Values() []value.Value { return r.values }

// Columns is the output column names, in order.
func (r Row) Columns() []string { return r.columns }

// Rows iterates a query's result rows.
type Rows struct {
	columns []string
	rows    [][]value.Value
	i       int
}

// newRows wraps the executor's output buffer.
func newRows(columns []string, rows [][]value.Value) *Rows {
	return &Rows{columns: columns, rows: rows}
}

// Columns is the output column names, in order.
func (rs *Rows) Columns() []string { return rs.columns }

// Next pulls the next row; ok is false when the result is exhausted.
func (rs *Rows) Next() (Row, bool) {
	if rs.i >= len(rs.rows) {
		return Row{}, false
	}
	r := Row{columns: rs.columns, values: rs.rows[rs.i]}
	rs.i++
	return r, true
}

// NextBatch pulls up to n rows (fewer if the result is exhausted).
func (rs *Rows) NextBatch(n int) []Row {
	out := make([]Row, 0, n)
	for range n {
		r, ok := rs.Next()
		if !ok {
			break
		}
		out = append(out, r)
	}
	return out
}

// All iterates the remaining rows (range-over-func).
func (rs *Rows) All() iter.Seq[Row] {
	return func(yield func(Row) bool) {
		for {
			r, ok := rs.Next()
			if !ok || !yield(r) {
				return
			}
		}
	}
}
