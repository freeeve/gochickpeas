// The per-query viz artifacts beyond timings (task 027): kernel source,
// gql EXPLAIN plans, and allocation profiles, in the schemas the ldbc
// side's import_gochickpeas.sh folds into the rcptest stores (their
// tasks/266; dedupe key family/query/engine/engineCommit).

package ldbc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
)

// CodeRecord is one kernel's Go source for the viz code panel.
type CodeRecord struct {
	Family       string `json:"family"`
	Query        string `json:"query"`
	Engine       string `json:"engine"`
	Lang         string `json:"lang"`
	Source       string `json:"source"`
	SrcRef       string `json:"srcRef"`
	EngineCommit string `json:"engineCommit"`
	EngineDate   string `json:"engineDate"`
}

// PlanRecord is one query's EXPLAIN plan for the viz plan panel.
type PlanRecord struct {
	Family       string `json:"family"`
	Query        string `json:"query"`
	Variant      string `json:"variant"`
	Engine       string `json:"engine"`
	Cypher       string `json:"cypher"`
	Plan         string `json:"plan"`
	EngineCommit string `json:"engineCommit"`
	EngineDate   string `json:"engineDate"`
}

// ProfileRecord is one query's allocation profile. Allocs and Bytes are
// Go runtime counters -- not the rcp engines' alloc-counter currency --
// so Measure spells out the semantics for the viz to label honestly.
type ProfileRecord struct {
	Family       string `json:"family"`
	Query        string `json:"query"`
	Engine       string `json:"engine"`
	Allocs       uint64 `json:"allocs"`
	Bytes        uint64 `json:"bytes"`
	Rows         int    `json:"rows"`
	Measure      string `json:"measure"`
	EngineCommit string `json:"engineCommit"`
	EngineDate   string `json:"engineDate"`
}

// ProfileMeasure documents the profile currency stamped into every
// emitted ProfileRecord.
const ProfileMeasure = "go runtime.MemStats delta over one warm run: allocs=Mallocs, bytes=TotalAlloc"

// MeasureAllocs runs fn once after settling the collector and returns
// the Go heap allocation count and byte deltas it caused.
func MeasureAllocs(fn func() error) (nAllocs, nBytes uint64, err error) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := fn(); err != nil {
		return 0, 0, err
	}
	runtime.ReadMemStats(&after)
	return after.Mallocs - before.Mallocs, after.TotalAlloc - before.TotalAlloc, nil
}

// AppendJSONL opens path for append-only JSONL emission, creating the
// parent directory; the caller closes the file.
func AppendJSONL(path string) (*os.File, *json.Encoder, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return f, json.NewEncoder(f), nil
}
