// Plan-shape golden: a diff-friendly plain-text snapshot of every manifest
// query's canonical EXPLAIN plan. It guards plan QUALITY -- a planner change
// that stays correct is invisible to row-level parity but shows here as moved
// plan lines in a git diff, which is the review prompt. formatGolden/parseGolden
// round-trip the file; diffGolden reports the drift.
package main

import (
	"sort"
	"strings"
)

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
// for a stable report. Empty means the plans are unchanged. subset suppresses
// the went-missing check: a run that deliberately selected a few queries says
// nothing about the ones it never planned, and the absence lines would bury
// the real drift.
func diffGolden(golden map[string]string, current []goldenEntry, subset bool) []string {
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
	if !subset {
		for id := range golden {
			if !seen[id] {
				drift = append(drift, id+": in golden but absent from this run")
			}
		}
	}
	sort.Strings(drift)
	return drift
}
