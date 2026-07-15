// Data-driven output-boxing representation experiment (task 155/171). Builds
// the same (uri, count) result three ways -- boxed [][]any, flat-backed
// [][]any, and flat-backed [][]value.Value -- and reports allocs/op + ns/op so
// the [][]any -> typed-result decision rests on measurement, not intuition.
// TestReprHashParity proves the value.Value path (production RowsHashV) hashes
// byte-identically to the boxed path (RowsHash), so a migration keeps the pins.
package ldbc

import (
	"fmt"
	"testing"

	"github.com/freeeve/gochickpeas/gql/value"
)

// synthReprRows builds n synthetic (uri, count) pairs shaped like SPB a25's
// output: a string cell (always boxes as any) and an int64 above the small-int
// static cache (so any(count) also boxes). Setup runs outside the timed loop.
func synthReprRows(n int) ([]string, []int64) {
	uris := make([]string, n)
	counts := make([]int64, n)
	for i := 0; i < n; i++ {
		uris[i] = fmt.Sprintf("http://www.bbc.co.uk/things/%d#id", i)
		counts[i] = int64(1000 + i) // > 255: any(count) heap-boxes
	}
	return uris, counts
}

// buildBoxed is the current native-kernel idiom: one []any per row, both cells
// boxed.
func buildBoxed(uris []string, counts []int64) [][]any {
	out := make([][]any, len(uris))
	for i := range uris {
		out[i] = []any{uris[i], counts[i]}
	}
	return out
}

// buildFlatBoxed is option B: one flat []any backing sub-sliced per row -- the
// per-row slice alloc is gone, but the cells still box.
func buildFlatBoxed(uris []string, counts []int64) [][]any {
	n := len(uris)
	backing := make([]any, n*2)
	out := make([][]any, n)
	for i := range uris {
		row := backing[i*2 : i*2+2 : i*2+2]
		row[0] = uris[i]
		row[1] = counts[i]
		out[i] = row
	}
	return out
}

// buildValueRows is option V: flat [][]value.Value backing; value.Str/value.Int
// store the payload inline (string header / uint64), so no cell boxes.
func buildValueRows(uris []string, counts []int64) [][]value.Value {
	n := len(uris)
	backing := make([]value.Value, n*2)
	out := make([][]value.Value, n)
	for i := range uris {
		row := backing[i*2 : i*2+2 : i*2+2]
		row[0] = value.Str(uris[i])
		row[1] = value.Int(counts[i])
		out[i] = row
	}
	return out
}

const reprN = 50000

var (
	reprSinkAny [][]any
	reprSinkVal [][]value.Value
)

func BenchmarkReprBoxed(b *testing.B) {
	uris, counts := synthReprRows(reprN)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reprSinkAny = buildBoxed(uris, counts)
	}
}

func BenchmarkReprFlatBoxed(b *testing.B) {
	uris, counts := synthReprRows(reprN)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reprSinkAny = buildFlatBoxed(uris, counts)
	}
}

func BenchmarkReprValueRows(b *testing.B) {
	uris, counts := synthReprRows(reprN)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reprSinkVal = buildValueRows(uris, counts)
	}
}

// TestReprHashParity locks in that the value.Value result path produces the
// identical parity hash to the boxed [][]any path via the production encoders
// -- the precondition for migrating NativeKernel to [][]value.Value without
// re-pinning the 89/89 gate.
func TestReprHashParity(t *testing.T) {
	uris, counts := synthReprRows(1000)
	hAny, err := RowsHash(buildBoxed(uris, counts))
	if err != nil {
		t.Fatalf("boxed hash: %v", err)
	}
	hVal, err := RowsHashV(buildValueRows(uris, counts))
	if err != nil {
		t.Fatalf("value hash: %v", err)
	}
	if hAny != hVal {
		t.Fatalf("hash mismatch: boxed=%s value=%s -- value.Value path diverges from [][]any", hAny, hVal)
	}
}
