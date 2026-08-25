// Plan-shape golden: a diff-friendly plain-text snapshot of every
// manifest query's canonical EXPLAIN plan. It guards plan QUALITY -- a
// planner change that stays correct is invisible to row-level parity but
// shows here as moved plan lines in a git diff, which is the review
// prompt. FormatGolden/ParseGolden round-trip the file; DiffGolden
// reports the drift. Moved from cmd/gqlbench when the parity gate became
// the in-tree test (the benching drivers live in rustychickpeas-ldbc).
package ldbc

import (
	"sort"
	"strings"
)

// GoldenEntry is one query's canonical plan-shape snapshot.
type GoldenEntry struct {
	ID   string
	Plan string
}

const goldenSep = "=== "

// FormatGolden renders the golden file: a header, then one diff-friendly
// section per query, in capture order.
func FormatGolden(entries []GoldenEntry) string {
	var b strings.Builder
	b.WriteString("# gochickpeas canonical plan-shape golden.\n")
	b.WriteString("# Regenerate deliberately after an intended planner change:\n")
	b.WriteString("#   GOCHICKPEAS_PLANS_GOLDEN_CAPTURE=1 go test ./internal/ldbc -run TestGQLParityGate\n")
	b.WriteString("# A diff here is a review prompt: the planner moved a plan.\n")
	for _, e := range entries {
		b.WriteString("\n")
		b.WriteString(goldenSep)
		b.WriteString(e.ID)
		b.WriteString("\n")
		b.WriteString(e.Plan)
		b.WriteString("\n")
	}
	return b.String()
}

// ParseGolden reads a golden file back into id -> plan (the exact
// inverse of FormatGolden for section bodies).
func ParseGolden(text string) map[string]string {
	out := map[string]string{}
	id := ""
	var body []string
	flush := func() {
		if id != "" {
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
			continue
		}
		body = append(body, ln)
	}
	flush()
	return out
}

// DiffGolden compares current canonical plans against the golden: one
// drift line per changed, new, or missing query, sorted. subset
// suppresses the went-missing check for deliberately partial runs.
func DiffGolden(golden map[string]string, current []GoldenEntry, subset bool) []string {
	var drift []string
	seen := map[string]bool{}
	for _, e := range current {
		seen[e.ID] = true
		want, ok := golden[e.ID]
		if !ok {
			drift = append(drift, e.ID+": new query, not in golden")
			continue
		}
		if want != e.Plan {
			drift = append(drift, e.ID+": plan shape changed")
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
