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
	"runtime/pprof"
	"slices"
	"sort"
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

// planDistinct plans query n times in this one process and returns how many
// DISTINCT plan texts result. Go randomizes map iteration order on purpose,
// so any planner decision that leaks map order produces more than one plan
// here -- the plan-stability question a bandwidth-bound query's timing cannot
// answer on a shared box: it costs nothing, needs no quiet machine, and a
// count above 1 identifies a nondeterministic planner outright, before any
// timing anomaly can be mistaken for a regression.
func planDistinct(g *chickpeas.Snapshot, query string, n int) (int, error) {
	seen := map[string]struct{}{}
	for range n {
		plan, err := gql.Explain(g, query)
		if err != nil {
			return 0, err
		}
		seen[stripVolatilePlanLines(plan)] = struct{}{}
	}
	return len(seen), nil
}

// stripVolatilePlanLines drops a rendered plan's non-structural lines so the
// stability comparison sees only the plan's shape. The "Planning: N ms" header
// is wall-clock and jitters every run -- keeping it would report every plan as
// unstable and hide a genuine map-order divergence in the noise.
func stripVolatilePlanLines(plan string) string {
	lines := strings.Split(plan, "\n")
	kept := lines[:0]
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "Planning:") {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n")
}

// goldenEntry is one query's canonical plan-shape snapshot in the golden.
type goldenEntry struct {
	id   string
	plan string
}

const goldenSep = "=== "

// formatGolden renders the golden file: a header, then one diff-friendly
// section per query (its id and canonical plan lines), in capture order. Plain
// text (not JSONL) so a planner change shows the moved plan lines directly in a
// git diff -- the review prompt is the point.
func formatGolden(entries []goldenEntry) string {
	var b strings.Builder
	b.WriteString("# gochickpeas canonical plan-shape golden.\n")
	b.WriteString("# Regenerate deliberately after an intended planner change:\n")
	b.WriteString("#   gqlbench -manifest <...> -plans-golden <this file> -plans-golden-capture\n")
	b.WriteString("# A diff here is a review prompt: the planner moved a plan.\n")
	for _, e := range entries {
		b.WriteString("\n")
		b.WriteString(goldenSep)
		b.WriteString(e.id)
		b.WriteString("\n")
		b.WriteString(e.plan)
		b.WriteString("\n")
	}
	return b.String()
}

// parseGolden reads a golden file back into id -> plan. It is the exact inverse
// of formatGolden for the section body (comment/blank lines outside a section
// are ignored), so a capture then parse round-trips.
func parseGolden(text string) map[string]string {
	out := map[string]string{}
	id := ""
	var body []string
	flush := func() {
		if id != "" {
			// Drop the single trailing blank line formatGolden writes.
			for len(body) > 0 && body[len(body)-1] == "" {
				body = body[:len(body)-1]
			}
			out[id] = strings.Join(body, "\n")
		}
	}
	for _, ln := range strings.Split(text, "\n") {
		if strings.HasPrefix(ln, goldenSep) {
			flush()
			id = strings.TrimSpace(strings.TrimPrefix(ln, goldenSep))
			body = nil
			continue
		}
		if id == "" {
			continue // header/comment preamble before the first section
		}
		body = append(body, ln)
	}
	flush()
	return out
}

// diffGolden compares the current canonical plans against the golden, returning
// one drift line per query whose plan changed, is new, or went missing -- sorted
// for a stable report. Empty means the plans are unchanged.
func diffGolden(golden map[string]string, current []goldenEntry) []string {
	var drift []string
	seen := map[string]bool{}
	for _, e := range current {
		seen[e.id] = true
		want, ok := golden[e.id]
		if !ok {
			drift = append(drift, e.id+": new query, not in golden")
			continue
		}
		if want != e.plan {
			drift = append(drift, e.id+": plan shape changed")
		}
	}
	for id := range golden {
		if !seen[id] {
			drift = append(drift, id+": in golden but absent from this run")
		}
	}
	sort.Strings(drift)
	return drift
}

// cpuProfiling tracks the lazily-started -cpuprofile capture, begun at the
// first timed run so graph loads and parity checks stay out of the
// profile.
var cpuProfiling bool

// startCPUProfile begins the CPU capture once; empty path = off.
func startCPUProfile(path string) error {
	if path == "" || cpuProfiling {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		return err
	}
	cpuProfiling = true
	return nil
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
	cachedParity := flag.Bool("cached-parity", false, "also run each query through the PlanCache (auto-parameterized) path and verify it matches the same reference hash; catches divergence between the literal and cached plans")
	planStability := flag.Int("plan-stability", 0, "if >1, plan each matched query this many times in-process and fail the run for any query whose plan text is not identical across all plannings (catches map-order-dependent plan nondeterminism); needs no quiet box, and unstable queries are never emitted a timing")
	plansGolden := flag.String("plans-golden", "", "path to a canonical plan-shape golden; verify each matched query's plan against it and fail the run on drift (a diff is a review prompt that a planner change moved a plan -- invisible to row-level parity). Needs no quiet box")
	plansGoldenCapture := flag.Bool("plans-golden-capture", false, "with -plans-golden, (re)write the golden from the current plans instead of verifying -- the deliberate regeneration step after an intended planner change")
	gqlVersion := flag.String("gql-version", "v0.2.0", "gql engine version stamped into meta")
	cpuProfile := flag.String("cpuprofile", "", "write a CPU profile covering the timed runs")
	memProfile := flag.String("memprofile", "", "write an allocs profile at exit (alloc-site attribution)")
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
	cellID := ldbc.CellIdentity{Engine: "gochickpeas (gql)", Shape: "gqlv0", GQLVersion: *gqlVersion}

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
	// One PlanCache per graph for the -cached-parity cross-check, so the
	// auto-parameterized/cached path is exercised (and its plan reused) the
	// way a real caller hits it.
	caches := map[string]*gql.PlanCache{}
	var outcomes []outcome
	var unstable []string
	var golden []goldenEntry
	var planDrift []string
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
		match, detail, err := ldbc.VerifyCell(row, cells)
		if err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		if !match {
			outcomes = append(outcomes, outcome{row: row, status: "DIFF", detail: detail})
			fmt.Printf("%-16s DIFF  %s\n", id, detail)
			continue
		}
		// Cached-path parity: the PlanCache auto-parameterizes before
		// planning, which can pick a different anchor than the literal plan.
		// The RESULT must stay identical -- verify the cached path against the
		// same reference hash. Run twice so the cache-hit replay (not only the
		// first miss) is checked. A divergence here is a real correctness bug
		// in the cached path that -verify-only over gql.Run cannot see.
		if *cachedParity {
			c, ok := caches[row.Graph]
			if !ok {
				c = gql.NewPlanCache(1 << 26)
				caches[row.Graph] = c
			}
			cdiff := ""
			for pass := 0; pass < 2 && cdiff == ""; pass++ {
				cres, cerr := c.Run(g, row.GQL)
				if cerr != nil {
					cdiff = "cached run: " + cerr.Error()
					break
				}
				ccells, cerr := resultCells(cres)
				if cerr != nil {
					cdiff = "cached cells: " + cerr.Error()
					break
				}
				cmatch, cdetail, cerr := ldbc.VerifyCell(row, ccells)
				if cerr != nil {
					return fmt.Errorf("%s (cached parity): %w", id, cerr)
				}
				if !cmatch {
					cdiff = cdetail
				}
			}
			if cdiff != "" {
				outcomes = append(outcomes, outcome{row: row, status: "CDIFF", detail: cdiff})
				fmt.Printf("%-16s CDIFF %s\n", id, cdiff)
				continue
			}
		}

		// Plan-shape golden: snapshot the canonical plan for the corpus. Guards
		// plan QUALITY, which the row-level parity above cannot see -- a planner
		// regression that stays correct is invisible to it. Collected here for a
		// MATCHed query; written or diffed after the loop.
		if *plansGolden != "" {
			canon, cerr := gql.ExplainCanonical(g, row.GQL)
			if cerr != nil {
				return fmt.Errorf("%s (plans-golden): %w", id, cerr)
			}
			golden = append(golden, goldenEntry{id: id, plan: canon})
		}

		// Plan-stability: a timing means nothing if the plan behind it varies
		// run to run, so ask this before ever timing the query (it needs no
		// quiet box). An unstable plan is not a parity failure -- the result
		// still MATCHed -- but the run fails at the end and the query is never
		// emitted a timing.
		planUnstable := false
		if *planStability > 1 {
			distinct, perr := planDistinct(g, row.GQL, *planStability)
			if perr != nil {
				return fmt.Errorf("%s (plan-stability): %w", id, perr)
			}
			if distinct > 1 {
				planUnstable = true
				unstable = append(unstable, fmt.Sprintf("%s (%d distinct plans/%d plannings)", id, distinct, *planStability))
				fmt.Printf("%-16s UNSTABLE %d distinct plans across %d plannings\n", id, distinct, *planStability)
			}
		}

		outcomes = append(outcomes, outcome{row: row, status: "MATCH", rows: len(cells)})
		if *verifyOnly {
			fmt.Printf("%-16s MATCH (%d rows)\n", id, len(cells))
			continue
		}
		if planUnstable {
			fmt.Printf("%-16s skip emit -- plan not stable\n", id)
			continue
		}

		if err := startCPUProfile(*cpuProfile); err != nil {
			return err
		}
		samples, err := ldbc.TimeSamples(*runs, func() error {
			_, err := gql.Run(g, row.GQL)
			return err
		})
		if err != nil {
			return fmt.Errorf("%s (timed run): %w", id, err)
		}
		rec := ldbc.CellRecord(row, cellID, stamp, samples, len(cells), g)
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

	match, diff, skip, cdiff := 0, 0, 0, 0
	for _, o := range outcomes {
		switch o.status {
		case "MATCH":
			match++
		case "DIFF":
			diff++
		case "CDIFF":
			cdiff++
		case "SKIP":
			skip++
		}
	}
	if cpuProfiling {
		pprof.StopCPUProfile()
	}
	if *memProfile != "" {
		f, err := os.Create(*memProfile)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := pprof.Lookup("allocs").WriteTo(f, 0); err != nil {
			return err
		}
	}
	cdiffSummary := ""
	if cdiff > 0 {
		cdiffSummary = fmt.Sprintf(", %d CDIFF", cdiff)
	}
	fmt.Printf("\n%d/%d MATCH, %d DIFF%s, %d SKIP; %d timing+plan+profile triples emitted at %s\n",
		match, len(outcomes), diff, cdiffSummary, skip, emitted, stamp.Commit)
	if len(unstable) > 0 {
		fmt.Printf("%d plan-unstable: %s\n", len(unstable), strings.Join(unstable, "; "))
	}
	if *plansGolden != "" {
		if *plansGoldenCapture {
			if err := os.WriteFile(*plansGolden, []byte(formatGolden(golden)), 0o644); err != nil {
				return fmt.Errorf("writing plan-shape golden: %w", err)
			}
			fmt.Printf("captured %d canonical plan-shapes to %s\n", len(golden), *plansGolden)
		} else {
			data, rerr := os.ReadFile(*plansGolden)
			if rerr != nil {
				return fmt.Errorf("reading plan-shape golden (capture it first with -plans-golden-capture): %w", rerr)
			}
			planDrift = diffGolden(parseGolden(string(data)), golden)
			if len(planDrift) > 0 {
				fmt.Printf("%d plan-shape drift vs golden:\n  %s\n", len(planDrift), strings.Join(planDrift, "\n  "))
			} else {
				fmt.Printf("plan-shape golden: %d queries unchanged\n", len(golden))
			}
		}
	}
	if diff > 0 {
		return fmt.Errorf("%d queries DIFFed against their reference hashes", diff)
	}
	if cdiff > 0 {
		return fmt.Errorf("%d queries diverged on the cached/auto-parameterized path (CDIFF)", cdiff)
	}
	if len(unstable) > 0 {
		return fmt.Errorf("%d queries produced nondeterministic plans across plannings", len(unstable))
	}
	if len(planDrift) > 0 {
		return fmt.Errorf("%d queries drifted from the plan-shape golden (review, then regenerate with -plans-golden-capture)", len(planDrift))
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gqlbench:", err)
		os.Exit(1)
	}
}
