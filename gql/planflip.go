// Plan-flip detection for the cache: a template planned blind (constants
// lifted to parameters) can choose a structurally different operator tree
// than value-sighted planning would -- exact scan cardinalities, resolved
// anchor degrees, and rewrite gates priced from those exacts all go blind
// under auto-parameterization, and a flipped plan can be arbitrarily worse
// (the census measured one 17.5x allocation regression). The cache detects
// the flip once, when a template is first planned, by also planning the
// literal text and comparing the trees value-blind; a flipped template is
// marked so execution routes through sighted planning instead of the
// cached tree. Non-flipped templates -- the overwhelming majority -- keep
// the full cache win.
package gql

import (
	"regexp"
	"strings"

	"github.com/freeeve/gochickpeas/gql/internal/explain"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

var (
	blindStrLit = regexp.MustCompile(`'(?:[^'\\]|\\.)*'`)
	blindParam  = regexp.MustCompile(`\$\w+`)
	blindNumber = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
)

// valueBlind normalizes a canonical plan rendering so a literal and its
// lifted parameter compare equal: strings, params, and numbers all become
// `?`, and the [anchor: ...] provenance note drops entirely -- it records
// WHICH SIGNAL decided the anchor (exact vs average), which legitimately
// differs between sighted and blind planning even when the chosen
// operator tree is identical. Both sides normalize identically, so
// structure alone decides.
func valueBlind(lines []string) string {
	kept := lines[:0]
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "[anchor:") {
			continue
		}
		kept = append(kept, ln)
	}
	lines = kept
	s := strings.Join(lines, "\n")
	s = blindStrLit.ReplaceAllString(s, "?")
	s = blindParam.ReplaceAllString(s, "?")
	s = blindNumber.ReplaceAllString(s, "?")
	return s
}

// planFlipped reports whether the template plan the cache would execute
// (post adaptive anchor choice for this call's values) differs
// structurally from the plan the literal query text produces.
func planFlipped(query string, chosen *plan.Plan, gr *graph.SnapshotGraph) bool {
	qs, err := parseDesugar(query)
	if err != nil {
		return false
	}
	sighted, err := plan.Build(qs, gr)
	if err != nil {
		return false
	}
	return valueBlind(explain.Canonical(sighted, plan.Estimate(sighted, gr))) !=
		valueBlind(explain.Canonical(chosen, plan.Estimate(chosen, gr)))
}
