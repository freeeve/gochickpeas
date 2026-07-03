// The native parity manifest and per-query kernel registry (task 025):
// each LDBC benchmark query implemented as a hand-written Go kernel,
// gated on the same rowhash/v1 reference hashes the GQL manifest uses.
// Native kernels have no query text, so a manifest row is just
// family/query/variant/graph/refhash/norm; the runner looks the kernel
// up by (family, query) and skips loudly when it is not implemented yet.

package ldbc

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	chickpeas "github.com/freeeve/gochickpeas"
)

// NativeRow is one native_variants.tsv row (6 tab-separated columns).
type NativeRow struct {
	Family  string
	Query   string
	Variant string
	Graph   string
	RefHash string
	Norm    string
}

// LoadNativeManifest reads a native_variants.tsv: tab-separated, 6
// columns, #-prefixed and blank lines skipped.
func LoadNativeManifest(path string) ([]NativeRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rows []NativeRow
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
		if len(cols) != 6 {
			return nil, fmt.Errorf("%s:%d: %d columns, want 6", path, line, len(cols))
		}
		rows = append(rows, NativeRow{
			Family: cols[0], Query: cols[1], Variant: cols[2], Graph: cols[3],
			RefHash: cols[4], Norm: cols[5],
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}

// NativeKernel prepares one LDBC benchmark query on a loaded snapshot
// and returns its runnable. The prepare step is UNTIMED -- it mirrors
// exactly what the rcp-native harness builds outside its timer (seed
// resolution, and for Q19/Q20/IC14 the derived weight maps; the IS
// short reads' message anchors) so the emitted timings measure the
// same work on both engines. The runnable computes the result rows in
// the reference's column order, with cells in the rowhash domain (nil,
// bool, int64, float64, string, []any); it must be deterministic and
// side-effect free so the emitter can time repeated calls of the same
// work the parity check verified.
type NativeKernel func(g *chickpeas.Snapshot) (func() ([][]any, error), error)

// simpleKernel adapts a query with no untimed prepare phase: every run
// does the full work, matching the rcp-native timings whose closures
// recompute everything per iteration.
func simpleKernel(rows func(g *chickpeas.Snapshot) ([][]any, error)) NativeKernel {
	return func(g *chickpeas.Snapshot) (func() ([][]any, error), error) {
		return func() ([][]any, error) { return rows(g) }, nil
	}
}

// nativeRegistry maps "Family/Query" to its kernel. Families register
// from their own files' init functions.
var nativeRegistry = map[string]NativeKernel{}

// registerNative adds one kernel at init time; a duplicate id is a
// programming error.
func registerNative(family, query string, k NativeKernel) {
	id := family + "/" + query
	if _, dup := nativeRegistry[id]; dup {
		panic("duplicate native kernel " + id)
	}
	nativeRegistry[id] = k
}

// NativeKernelFor looks up the kernel for a manifest row.
func NativeKernelFor(family, query string) (NativeKernel, bool) {
	k, ok := nativeRegistry[family+"/"+query]
	return k, ok
}

// NativeKernelCount reports how many per-query kernels are registered.
func NativeKernelCount() int { return len(nativeRegistry) }
