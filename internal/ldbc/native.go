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
	"github.com/freeeve/gochickpeas/gql/value"
)

// canonRowV encodes one value.Value row to its rowhash/v1 array form, matching
// CanonCell(any([]any(row))) over the equivalent boxed row.
func canonRowV(row []value.Value) (string, error) {
	parts := make([]string, len(row))
	for j, c := range row {
		enc, err := CanonCellV(c)
		if err != nil {
			return "", err
		}
		parts[j] = enc
	}
	return "[" + strings.Join(parts, ",") + "]", nil
}

// LoadNativeManifest reads a native_variants.tsv (tab-separated, 6
// columns, #-prefixed and blank lines skipped) into ManifestRows with an
// empty GQL column -- one manifest row type serves every emitter, so the
// two manifests cannot drift structurally.
func LoadNativeManifest(path string) ([]ManifestRow, error) {
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
		if len(cols) != 6 {
			return nil, fmt.Errorf("%s:%d: %d columns, want 6", path, line, len(cols))
		}
		rows = append(rows, ManifestRow{
			Family: cols[0], Query: cols[1], Variant: cols[2], Graph: cols[3],
			RefHash: cols[4], Norm: cols[5],
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}

// NativeKernelV prepares one LDBC benchmark query on a loaded snapshot and
// returns its runnable. The prepare step is UNTIMED -- it mirrors exactly what
// the rcp-native harness builds outside its timer (seed resolution, and for
// Q19/Q20/IC14 the derived weight maps; the IS short reads' message anchors) so
// the emitted timings measure the same work on both engines. The runnable
// computes the result rows in the reference's column order, cells stored inline
// as value.Value (scalars zero-box); it must be deterministic and side-effect
// free so the emitter can time repeated calls of the same work the parity check
// verified. Kernels verify through VerifyCellV.
type NativeKernelV func(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error)

// simpleKernelV adapts a query with no untimed prepare phase: every run does
// the full work, matching the rcp-native timings whose closures recompute
// everything per iteration.
func simpleKernelV(rows func(g *chickpeas.Snapshot) ([][]value.Value, error)) NativeKernelV {
	return func(g *chickpeas.Snapshot) (func() ([][]value.Value, error), error) {
		return func() ([][]value.Value, error) { return rows(g) }, nil
	}
}

// nativeEntry holds one query's value.Value kernel.
type nativeEntry struct {
	valued NativeKernelV
}

// nativeRegistry maps "Family/Query" to its kernel. Families register
// from their own files' init functions.
var nativeRegistry = map[string]nativeEntry{}

// registerNativeV adds one value.Value kernel at init time; a duplicate id is a
// programming error.
func registerNativeV(family, query string, k NativeKernelV) {
	id := family + "/" + query
	if _, dup := nativeRegistry[id]; dup {
		panic("duplicate native kernel " + id)
	}
	nativeRegistry[id] = nativeEntry{valued: k}
}

// HasNativeKernel reports whether a kernel is registered for (family, query)
// in either representation.
func HasNativeKernel(family, query string) bool {
	_, ok := nativeRegistry[family+"/"+query]
	return ok
}

// NativeKernelCount reports how many per-query kernels are registered.
func NativeKernelCount() int { return len(nativeRegistry) }

// PreparedKernel is a kernel prepared (untimed) on a snapshot. Run executes the
// work (timed loops call it repeatedly); Verify and RowCount read the most
// recent Run.
type PreparedKernel interface {
	Run() error
	Verify(row ManifestRow) (match bool, detail string, err error)
	RowCount() int
	// EncodedRows returns each row's rowhash/v1 canonical string (unsorted) --
	// the per-row multiset the debug diff harness compares.
	EncodedRows() ([]string, error)
}

type valuePrepared struct {
	run   func() ([][]value.Value, error)
	cells [][]value.Value
}

func (p *valuePrepared) Run() error {
	cells, err := p.run()
	if err != nil {
		return err
	}
	p.cells = cells
	return nil
}
func (p *valuePrepared) Verify(row ManifestRow) (bool, string, error) {
	return VerifyCellV(row, p.cells)
}
func (p *valuePrepared) RowCount() int { return len(p.cells) }
func (p *valuePrepared) EncodedRows() ([]string, error) {
	out := make([]string, len(p.cells))
	for i, r := range p.cells {
		s, err := canonRowV(r)
		if err != nil {
			return nil, err
		}
		out[i] = s
	}
	return out, nil
}

// PrepareNative looks up the kernel for a manifest row and runs its untimed
// prepare phase on g. ok=false means no kernel is registered; a non-nil error
// is a prepare failure.
func PrepareNative(row ManifestRow, g *chickpeas.Snapshot) (pk PreparedKernel, ok bool, err error) {
	entry, ok := nativeRegistry[row.Family+"/"+row.Query]
	if !ok {
		return nil, false, nil
	}
	run, err := entry.valued(g)
	if err != nil {
		return nil, true, err
	}
	return &valuePrepared{run: run}, true, nil
}
