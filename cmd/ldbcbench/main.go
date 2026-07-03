// ldbcbench cross-checks the six Go kernels against the Rust-exported
// LDBC SF1 fixture, then emits warm timings in the rustychickpeas-ldbc
// suite's JSONL schema (their python/cypher/timings.py; gochickpeas task
// 012). Parity gates emission: every fixture-present kernel must MATCH
// before any timing is written, so a DIFF fails the run instead of
// publishing a green number. Records append to -out; the ldbc side's
// viz/import_gochickpeas.sh picks that file up from this repo on deploy.
//
//	go run ./cmd/ldbcbench -rcpg /path/to/sf1.rcpg
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/internal/ldbc"
)

// record is one emitted timing row in the suite's shared JSONL schema.
type record struct {
	Family         string     `json:"family"`
	Query          string     `json:"query"`
	Variant        string     `json:"variant"`
	Engine         string     `json:"engine"`
	Warmth         string     `json:"warmth"`
	Ms             float64    `json:"ms"`
	Rows           int        `json:"rows"`
	SF             int        `json:"sf"`
	Shape          string     `json:"shape"`
	Parity         string     `json:"parity"`
	EngineCommit   string     `json:"engineCommit"`
	EngineDate     string     `json:"engineDate"`
	EngineDateTime string     `json:"engineDateTime"`
	EngineSubject  string     `json:"engineSubject"`
	LdbcCommit     *string    `json:"ldbcCommit"`
	LdbcDate       *string    `json:"ldbcDate"`
	LdbcDirty      bool       `json:"ldbcDirty"`
	MeasuredDate   string     `json:"measuredDate"`
	Source         string     `json:"source"`
	MsMin          float64    `json:"msMin"`
	MsP25          float64    `json:"msP25"`
	MsP75          float64    `json:"msP75"`
	MsN            int        `json:"msN"`
	Meta           recordMeta `json:"meta"`
}

// recordMeta self-describes the point: which port, at what core
// conformance level, against which fixture core commit.
type recordMeta struct {
	Port            string `json:"port"`
	CoreConformance string `json:"coreConformance"`
	CoreCommit      string `json:"coreCommit"`
	GoVersion       string `json:"goVersion"`
	Nodes           uint32 `json:"nodes"`
	Rels            uint64 `json:"rels"`
}

// gitStamp is the emitting repo's HEAD identity -- gochickpeas points
// plot on this repo's own commit timeline.
type gitStamp struct {
	commit, date, dateTime, subject string
}

func headStamp() (gitStamp, error) {
	out, err := exec.Command("git", "log", "-1", "--format=%H%x00%cs%x00%cI%x00%s").Output()
	if err != nil {
		return gitStamp{}, fmt.Errorf("git log: %w", err)
	}
	parts := strings.Split(strings.TrimRight(string(out), "\n"), "\x00")
	if len(parts) != 4 {
		return gitStamp{}, fmt.Errorf("git log returned %d fields, want 4", len(parts))
	}
	return gitStamp{commit: parts[0][:7], date: parts[1], dateTime: parts[2], subject: parts[3]}, nil
}

// percentile linearly interpolates over an ascending-sorted sample.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := p * float64(len(sorted)-1)
	lo := int(pos)
	frac := pos - float64(lo)
	if lo+1 >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	return sorted[lo]*(1-frac) + sorted[lo+1]*frac
}

// verifyStructural mirrors TestLDBCStructural's checks as an emit gate.
func verifyStructural(g *chickpeas.Snapshot, s *ldbc.Structural) error {
	if s.NodeCount != nil && g.NodeCount() != *s.NodeCount {
		return fmt.Errorf("node count: got %d, want %d", g.NodeCount(), *s.NodeCount)
	}
	if s.RelCount != nil && g.RelCount() != *s.RelCount {
		return fmt.Errorf("rel count: got %d, want %d", g.RelCount(), *s.RelCount)
	}
	if s.CSRIDSpace != nil && g.CSRIDSpace() != *s.CSRIDSpace {
		return fmt.Errorf("csr id space: got %d, want %d", g.CSRIDSpace(), *s.CSRIDSpace)
	}
	for _, pair := range s.RelCountByType {
		if got := g.RelTypeCount(pair.Name); got != pair.Count {
			return fmt.Errorf("rel type %s: got %d, want %d", pair.Name, got, pair.Count)
		}
	}
	for _, pair := range s.LabelCardinalities {
		set, ok := g.NodesWithLabel(pair.Name)
		if !ok {
			return fmt.Errorf("label %s missing", pair.Name)
		}
		if uint64(set.Len()) != pair.Count {
			return fmt.Errorf("label %s: got %d, want %d", pair.Name, set.Len(), pair.Count)
		}
	}
	return nil
}

func run() error {
	rcpg := flag.String("rcpg", os.Getenv("GOCHICKPEAS_SF1_RCPG"), "path to the Rust-exported sf1.rcpg (default $GOCHICKPEAS_SF1_RCPG)")
	fixture := flag.String("fixture", "testdata/ldbc/sf1_expected.json", "expected-results fixture")
	out := flag.String("out", "bench-out/emitted_gochickpeas.jsonl", "append-only JSONL output")
	runs := flag.Int("runs", 5, "timed runs per kernel (median emitted)")
	conformance := flag.String("core-conformance", "v0.1.0", "core conformance level stamped into meta")
	flag.Parse()
	if *rcpg == "" {
		return fmt.Errorf("no rcpg path: pass -rcpg or set GOCHICKPEAS_SF1_RCPG")
	}

	exp, err := ldbc.Load(*fixture)
	if err != nil {
		return err
	}
	stamp, err := headStamp()
	if err != nil {
		return err
	}

	start := time.Now()
	g, err := chickpeas.ReadRCPGFile(*rcpg)
	if err != nil {
		return fmt.Errorf("loading %s: %w", *rcpg, err)
	}
	fmt.Printf("loaded %s in %.1fs: %d nodes, %d rels\n", *rcpg, time.Since(start).Seconds(), g.NodeCount(), g.RelCount())

	// Phase 1: parity. Everything present in the fixture must match
	// before a single timing is written.
	if exp.Structural != nil {
		if err := verifyStructural(g, exp.Structural); err != nil {
			return fmt.Errorf("structural DIFF: %w", err)
		}
		fmt.Println("structural: MATCH")
	}
	type checked struct {
		kernel ldbc.Kernel
		rows   int
	}
	var kernels []checked
	for _, k := range ldbc.Kernels() {
		want, ok := k.Want(exp)
		if !ok {
			fmt.Printf("%-28s fixture section absent; skipped\n", k.Name)
			continue
		}
		got, err := k.Rows(g)
		if err != nil {
			return fmt.Errorf("%s: %w", k.Name, err)
		}
		if err := ldbc.DiffRows(got, want); err != nil {
			return fmt.Errorf("%s DIFF: %w", k.Name, err)
		}
		fmt.Printf("%-28s MATCH (%d rows)\n", k.Name, len(got))
		kernels = append(kernels, checked{kernel: k, rows: len(got)})
	}
	if len(kernels) == 0 {
		return fmt.Errorf("no kernel sections in fixture %s; nothing to emit", *fixture)
	}

	// Phase 2: warm timings (the parity pass above doubles as warmup).
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, c := range kernels {
		samples := make([]float64, *runs)
		for i := range samples {
			t0 := time.Now()
			if _, err := c.kernel.Rows(g); err != nil {
				return fmt.Errorf("%s (timed run): %w", c.kernel.Name, err)
			}
			samples[i] = float64(time.Since(t0).Microseconds()) / 1000.0
		}
		slices.Sort(samples)
		rec := record{
			Family:         "native",
			Query:          c.kernel.Name,
			Variant:        "committed",
			Engine:         "gochickpeas (go)",
			Warmth:         "warm",
			Ms:             percentile(samples, 0.5),
			Rows:           c.rows,
			SF:             1,
			Shape:          "native kernel",
			Parity:         "MATCH",
			EngineCommit:   stamp.commit,
			EngineDate:     stamp.date,
			EngineDateTime: stamp.dateTime,
			EngineSubject:  stamp.subject,
			MeasuredDate:   time.Now().UTC().Format("2006-01-02"),
			Source:         "emitted",
			MsMin:          samples[0],
			MsP25:          percentile(samples, 0.25),
			MsP75:          percentile(samples, 0.75),
			MsN:            len(samples),
			Meta: recordMeta{
				Port:            "gochickpeas",
				CoreConformance: *conformance,
				CoreCommit:      exp.Meta.CoreCommit,
				GoVersion:       runtime.Version(),
				Nodes:           g.NodeCount(),
				Rels:            g.RelCount(),
			},
		}
		if err := enc.Encode(rec); err != nil {
			return err
		}
		fmt.Printf("%-28s %10.3f ms  (min %.3f, p25 %.3f, p75 %.3f, n=%d)\n",
			c.kernel.Name, rec.Ms, rec.MsMin, rec.MsP25, rec.MsP75, rec.MsN)
	}
	fmt.Printf("emitted %d records to %s at %s\n", len(kernels), *out, stamp.commit)
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ldbcbench:", err)
		os.Exit(1)
	}
}
