// The completeness tool's own pins: the source scanner must agree with
// the kind-case table exactly, and the roll call's core must demonstrably
// discriminate -- a deliberately wrong (incomplete) covered map must be
// reported. Both sibling engines shipped a completeness checker whose
// quiet matching gap silently stopped enforcing; these tests are the
// guard against that class here.
package asttest

import (
	"slices"
	"testing"
)

// TestRollCallScannerAgreesWithTable pins scanner and table against each
// other, two-sided: every scanned ast.Expr kind has a case, every case
// names a scanned kind, and the count is the table's length -- so a
// scanner matching gap (or a table typo) fails as a disagreement instead
// of silently weakening every walker roll call.
func TestRollCallScannerAgreesWithTable(t *testing.T) {
	kinds, err := ExprKinds()
	if err != nil {
		t.Fatalf("scanning ast source: %v", err)
	}
	table := make([]string, 0, len(kinds))
	for _, c := range KindCases("n") {
		table = append(table, c.Kind)
	}
	slices.Sort(table)
	if !slices.Equal(kinds, table) {
		t.Fatalf("scanner and kind-case table disagree:\n scanned: %v\n table:   %v", kinds, table)
	}
	if len(kinds) == 0 {
		t.Fatal("scanner found no Expr kinds -- the tool is not enforcing anything")
	}
}

// TestMissingKindsDiscriminates writes the wrong reading out explicitly:
// a covered map missing exactly one kind must be reported as missing
// exactly that kind, and the complete map must report nothing. A roll
// call that cannot fail on an incomplete map proves nothing.
func TestMissingKindsDiscriminates(t *testing.T) {
	full := map[string]bool{}
	for _, c := range KindCases("n") {
		full[c.Kind] = true
	}
	if missing, err := MissingKinds(full); err != nil || len(missing) != 0 {
		t.Fatalf("complete map reported missing = %v (err %v), want none", missing, err)
	}
	for _, drop := range []string{"HasLabelExpr", "MapProj", "Cost"} {
		partial := make(map[string]bool, len(full))
		for k := range full {
			partial[k] = true
		}
		delete(partial, drop)
		missing, err := MissingKinds(partial)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(missing, []string{drop}) {
			t.Fatalf("dropping %s reported missing = %v, want [%s]", drop, missing, drop)
		}
	}
}
