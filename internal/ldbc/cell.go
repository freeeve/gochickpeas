// Shared manifest-cell flow for the bench emitters (gqlbench,
// ldbcnativebench, gabench, loadbench): the parity stamp, timed sampling,
// scale-factor rule, and emission Record schema live here once, so no
// emitter can drift its manifest schema from the others' and silently
// corrupt cross-engine comparisons.

package ldbc

import (
	"path/filepath"
	"runtime"
	"slices"
	"time"

	chickpeas "github.com/freeeve/gochickpeas"
)

// TimeSamples executes run `runs` times, returning the sorted per-run
// millisecond samples the Record percentile block reads.
func TimeSamples(runs int, run func() error) ([]float64, error) {
	samples := make([]float64, runs)
	for i := range samples {
		t0 := time.Now()
		if err := run(); err != nil {
			return nil, err
		}
		samples[i] = float64(time.Since(t0).Microseconds()) / 1000.0
	}
	slices.Sort(samples)
	return samples, nil
}

// ManifestSF is the suite's scale-factor rule: FinBench runs at SF10,
// everything else at SF1 (the manifest's graph column implies it; the
// record schema wants it explicit).
func ManifestSF(family string) int {
	if family == "FinBench" {
		return 10
	}
	return 1
}

// RecordSpec is the per-cell identity NewRecord stamps into the shared
// Record schema.
type RecordSpec struct {
	Family, Query, Variant string
	Engine, Shape          string
	SF                     int
	Parity                 string
	Rows                   int
	Meta                   Meta
}

// NewRecord assembles the emission Record every bench shares: warm/emitted
// framing, the measured date, the percentile block over sorted samples,
// and the repo head stamp. Meta's Port and GoVersion default when unset.
func NewRecord(spec RecordSpec, stamp Stamp, samples []float64) Record {
	if spec.Meta.Port == "" {
		spec.Meta.Port = "gochickpeas"
	}
	if spec.Meta.GoVersion == "" {
		spec.Meta.GoVersion = runtime.Version()
	}
	return Record{
		Family:         spec.Family,
		Query:          spec.Query,
		Variant:        spec.Variant,
		Engine:         spec.Engine,
		Warmth:         "warm",
		Ms:             Percentile(samples, 0.5),
		Rows:           spec.Rows,
		SF:             spec.SF,
		Shape:          spec.Shape,
		Parity:         spec.Parity,
		EngineCommit:   stamp.Commit,
		EngineDate:     stamp.Date,
		EngineDateTime: stamp.DateTime,
		EngineSubject:  stamp.Subject,
		MeasuredDate:   time.Now().UTC().Format("2006-01-02"),
		Source:         "emitted",
		MsMin:          samples[0],
		MsP25:          Percentile(samples, 0.25),
		MsP75:          Percentile(samples, 0.75),
		MsN:            len(samples),
		Meta:           spec.Meta,
	}
}

// CellIdentity is a manifest-driven emitter's identity strings.
type CellIdentity struct {
	Engine     string // e.g. "gochickpeas (go)", "gochickpeas (gql)"
	Shape      string // e.g. "native kernel", "gqlv0"
	GQLVersion string // stamped into meta when non-empty (gql cells)
}

// CellRecord is NewRecord for one manifest row: identity from id, SF from
// the manifest rule, parity MATCH (emission is parity-gated), graph meta
// from the loaded snapshot.
func CellRecord(row ManifestRow, id CellIdentity, stamp Stamp, samples []float64, rows int, g *chickpeas.Snapshot) Record {
	return NewRecord(RecordSpec{
		Family: row.Family, Query: row.Query, Variant: row.Variant,
		Engine: id.Engine, Shape: id.Shape,
		SF: ManifestSF(row.Family), Parity: "MATCH", Rows: rows,
		Meta: Meta{
			GQLVersion: id.GQLVersion,
			Graph:      filepath.Base(row.Graph),
			Nodes:      g.NodeCount(),
			Rels:       g.RelCount(),
		},
	}, stamp, samples)
}
