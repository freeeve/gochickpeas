// gqlbench runs the rustychickpeas-ldbc GQL parity manifest
// (viz/data/gql_variants.tsv, their task 258) against the gql engine:
// each row's query executes on the row's graph, result rows are
// normalized and hashed per rowhash/v1, and only a hash-equal (MATCH) run
// may emit a timing -- as engine "gochickpeas (gql)" in the suite's JSONL
// schema (gochickpeas task 012, deliverable 3). Rows the engine cannot
// run yet skip loudly (coverage is incremental by design); a DIFF fails
// the run.
//
// Alongside each timing it emits the viz artifacts of task 027: the
// engine's EXPLAIN plan for the manifest text (plans_gochickpeas.jsonl)
// and a per-query allocation profile (profiles_gochickpeas.jsonl), both
// in the schemas the ldbc side's import_gochickpeas.sh folds (tasks/266).
//
//	go run ./cmd/gqlbench -manifest .../viz/data/gql_variants.tsv
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	chickpeas "github.com/freeeve/gochickpeas"
	"github.com/freeeve/gochickpeas/gql"
	"github.com/freeeve/gochickpeas/gql/value"
	"github.com/freeeve/gochickpeas/internal/ldbc"
)

// cellOf converts one result value into the rowhash cell domain (nil,
// bool, int64, float64, string, []any). Nodes, rels, paths, maps, and
// temporals are hard errors -- the query text must project scalars.
func cellOf(v value.Value) (any, error) {
	switch v.Kind() {
	case value.KindNull:
		return nil, nil
	case value.KindBool:
		b, _ := v.AsBool()
		return b, nil
	case value.KindInt:
		i, _ := v.AsInt()
		return i, nil
	case value.KindFloat:
		f, _ := v.AsFloat()
		return f, nil
	case value.KindStr:
		s, _ := v.AsStr()
		return s, nil
	case value.KindList:
		vs, _ := v.AsList()
		out := make([]any, len(vs))
		for i, c := range vs {
			cell, err := cellOf(c)
			if err != nil {
				return nil, err
			}
			out[i] = cell
		}
		return out, nil
	}
	return nil, fmt.Errorf("cell kind %d unsupported by rowhash; project a scalar", v.Kind())
}

// resultCells drains a result into rowhash rows (positional cells).
func resultCells(rs *gql.Rows) ([][]any, error) {
	var out [][]any
	for row := range rs.All() {
		vals := row.Values()
		cells := make([]any, len(vals))
		for i, v := range vals {
			cell, err := cellOf(v)
			if err != nil {
				return nil, fmt.Errorf("column %d: %w", i, err)
			}
			cells[i] = cell
		}
		out = append(out, cells)
	}
	return out, nil
}

// outcome is one manifest row's verdict for the run summary.
type outcome struct {
	row    ldbc.ManifestRow
	status string // MATCH / DIFF / SKIP
	detail string
	rows   int
}

func run() error {
	manifest := flag.String("manifest", os.Getenv("GOCHICKPEAS_GQL_MANIFEST"),
		"path to gql_variants.tsv (default $GOCHICKPEAS_GQL_MANIFEST)")
	out := flag.String("out", "bench-out/emitted_gochickpeas.jsonl", "append-only JSONL output")
	plansOut := flag.String("plans-out", "bench-out/plans_gochickpeas.jsonl", "append-only EXPLAIN-plan JSONL output")
	profilesOut := flag.String("profiles-out", "bench-out/profiles_gochickpeas.jsonl", "append-only alloc-profile JSONL output")
	runs := flag.Int("runs", 5, "timed runs per matched query (median emitted)")
	only := flag.String("only", "", "comma-separated query ids (e.g. Q1,IC3); empty = all")
	verifyOnly := flag.Bool("verify-only", false, "check parity only; no timings, no emission")
	gqlVersion := flag.String("gql-version", "v0.2.0", "gql engine version stamped into meta")
	flag.Parse()
	if *manifest == "" {
		return fmt.Errorf("no manifest: pass -manifest or set GOCHICKPEAS_GQL_MANIFEST")
	}

	rows, err := ldbc.LoadManifest(*manifest)
	if err != nil {
		return err
	}
	if *only != "" {
		keep := map[string]bool{}
		for id := range strings.SplitSeq(*only, ",") {
			keep[strings.TrimSpace(id)] = true
		}
		rows = slices.DeleteFunc(rows, func(r ldbc.ManifestRow) bool { return !keep[r.Query] })
	}
	if len(rows) == 0 {
		return fmt.Errorf("no manifest rows selected")
	}
	stamp, err := ldbc.HeadStamp()
	if err != nil {
		return err
	}

	var f, plf, prf *os.File
	var enc, planEnc, profEnc *json.Encoder
	if !*verifyOnly {
		if f, enc, err = ldbc.AppendJSONL(*out); err != nil {
			return err
		}
		defer f.Close()
		if plf, planEnc, err = ldbc.AppendJSONL(*plansOut); err != nil {
			return err
		}
		defer plf.Close()
		if prf, profEnc, err = ldbc.AppendJSONL(*profilesOut); err != nil {
			return err
		}
		defer prf.Close()
	}

	// One load per distinct graph, in first-seen manifest order.
	graphs := map[string]*chickpeas.Snapshot{}
	var outcomes []outcome
	emitted := 0
	for _, row := range rows {
		g, ok := graphs[row.Graph]
		if !ok {
			t0 := time.Now()
			g, err = chickpeas.ReadRCPGFile(row.Graph)
			if err != nil {
				return fmt.Errorf("loading %s: %w", row.Graph, err)
			}
			fmt.Printf("loaded %s in %.1fs: %d nodes, %d rels\n",
				filepath.Base(row.Graph), time.Since(t0).Seconds(), g.NodeCount(), g.RelCount())
			graphs[row.Graph] = g
		}

		id := row.Family + "/" + row.Query
		if row.Blocked() {
			outcomes = append(outcomes, outcome{row: row, status: "SKIP", detail: "blocked by manifest"})
			fmt.Printf("%-16s SKIP  blocked by manifest\n", id)
			continue
		}
		res, err := gql.Run(g, row.GQL)
		if err != nil {
			outcomes = append(outcomes, outcome{row: row, status: "SKIP", detail: err.Error()})
			fmt.Printf("%-16s SKIP  %v\n", id, err)
			continue
		}
		cells, err := resultCells(res)
		if err != nil {
			outcomes = append(outcomes, outcome{row: row, status: "SKIP", detail: err.Error()})
			fmt.Printf("%-16s SKIP  %v\n", id, err)
			continue
		}
		normed, err := ldbc.ApplyNorm(cells, row.Norm)
		if err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		hash, err := ldbc.RowsHash(normed)
		if err != nil {
			outcomes = append(outcomes, outcome{row: row, status: "SKIP", detail: err.Error()})
			fmt.Printf("%-16s SKIP  %v\n", id, err)
			continue
		}
		if hash != row.RefHash {
			detail := fmt.Sprintf("hash %s != ref %s (%d rows)", hash, row.RefHash, len(cells))
			outcomes = append(outcomes, outcome{row: row, status: "DIFF", detail: detail})
			fmt.Printf("%-16s DIFF  %s\n", id, detail)
			continue
		}
		outcomes = append(outcomes, outcome{row: row, status: "MATCH", rows: len(cells)})
		if *verifyOnly {
			fmt.Printf("%-16s MATCH (%d rows)\n", id, len(cells))
			continue
		}

		samples := make([]float64, *runs)
		for i := range samples {
			t0 := time.Now()
			if _, err := gql.Run(g, row.GQL); err != nil {
				return fmt.Errorf("%s (timed run): %w", id, err)
			}
			samples[i] = float64(time.Since(t0).Microseconds()) / 1000.0
		}
		// The suite runs FinBench at SF10, everything else here at SF1
		// (the manifest's graph column implies it; the record schema
		// wants it explicit).
		sf := 1
		if row.Family == "FinBench" {
			sf = 10
		}
		slices.Sort(samples)
		rec := ldbc.Record{
			Family:         row.Family,
			Query:          row.Query,
			Variant:        row.Variant,
			Engine:         "gochickpeas (gql)",
			Warmth:         "warm",
			Ms:             ldbc.Percentile(samples, 0.5),
			Rows:           len(cells),
			SF:             sf,
			Shape:          "gqlv0",
			Parity:         "MATCH",
			EngineCommit:   stamp.Commit,
			EngineDate:     stamp.Date,
			EngineDateTime: stamp.DateTime,
			EngineSubject:  stamp.Subject,
			MeasuredDate:   time.Now().UTC().Format("2006-01-02"),
			Source:         "emitted",
			MsMin:          samples[0],
			MsP25:          ldbc.Percentile(samples, 0.25),
			MsP75:          ldbc.Percentile(samples, 0.75),
			MsN:            len(samples),
			Meta: ldbc.Meta{
				Port:       "gochickpeas",
				GQLVersion: *gqlVersion,
				Graph:      filepath.Base(row.Graph),
				GoVersion:  runtime.Version(),
				Nodes:      g.NodeCount(),
				Rels:       g.RelCount(),
			},
		}
		if err := enc.Encode(rec); err != nil {
			return err
		}
		emitted++
		fmt.Printf("%-16s MATCH %9.3f ms  (min %.3f, p75 %.3f, n=%d, %d rows)\n",
			id, rec.Ms, rec.MsMin, rec.MsP75, rec.MsN, len(cells))

		planText, err := gql.Explain(g, row.GQL)
		if err != nil {
			return fmt.Errorf("%s EXPLAIN: %w", id, err)
		}
		if err := planEnc.Encode(ldbc.PlanRecord{
			Family: row.Family, Query: row.Query, Variant: row.Variant,
			Engine: "gochickpeas (gql)", Cypher: row.GQL, Plan: planText,
			EngineCommit: stamp.Commit, EngineDate: stamp.Date,
		}); err != nil {
			return err
		}
		nAllocs, nBytes, err := ldbc.MeasureAllocs(func() error { _, err := gql.Run(g, row.GQL); return err })
		if err != nil {
			return fmt.Errorf("%s (profiled run): %w", id, err)
		}
		if err := profEnc.Encode(ldbc.ProfileRecord{
			Family: row.Family, Query: row.Query, Engine: "gochickpeas (gql)",
			Allocs: nAllocs, Bytes: nBytes, Rows: len(cells), Measure: ldbc.ProfileMeasure,
			EngineCommit: stamp.Commit, EngineDate: stamp.Date,
		}); err != nil {
			return err
		}
	}

	match, diff, skip := 0, 0, 0
	for _, o := range outcomes {
		switch o.status {
		case "MATCH":
			match++
		case "DIFF":
			diff++
		case "SKIP":
			skip++
		}
	}
	fmt.Printf("\n%d/%d MATCH, %d DIFF, %d SKIP; %d timing+plan+profile triples emitted at %s\n",
		match, len(outcomes), diff, skip, emitted, stamp.Commit)
	if diff > 0 {
		return fmt.Errorf("%d queries DIFFed against their reference hashes", diff)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gqlbench:", err)
		os.Exit(1)
	}
}
