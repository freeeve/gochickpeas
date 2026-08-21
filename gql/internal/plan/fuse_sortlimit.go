// Sort-limit fusion: a trailing segment that only orders, paginates, and
// re-projects the previous segment's columns forces that segment to
// materialize every row it produces, even though at most skip+limit can
// ever surface. Donating the ORDER BY / OFFSET / LIMIT upstream lets the
// producing segment's bounded sink retain skip+limit rows and refuse the
// rest before they are built (the NEXT-authored ORDER BY form of the
// LDBC IC queries). The donor keeps its projection, now over the already
// ordered, already truncated rows.
package plan

import "github.com/freeeve/gochickpeas/gql/internal/ast"

// DisableSortLimitFusion pins result identity in tests: the unfused
// pipeline must produce exactly the rows the fused one does, in the
// same order.
var DisableSortLimitFusion bool

// fuseSortLimit hoists each qualifying segment's ordering and pagination
// into its producer, scanning producers first so an aligned chain can
// cascade. The donor must be a pure projection (no stages, no
// post-where, not distinct, not aggregated) whose ORDER BY keys are bare
// references to the producer's output columns and whose LIMIT bounds the
// retention; the receiver must not order, paginate, or post-filter on
// its own (a receiver post-where runs before the donor's sort-limit in
// the unfused pipeline, so hoisting past it would reorder semantics).
func fuseSortLimit(segs []*Segment) {
	if DisableSortLimitFusion {
		return
	}
	for i := len(segs) - 2; i >= 0; i-- {
		b, c := segs[i], segs[i+1]
		if !sortLimitDonor(c) || !sortLimitReceiver(b) {
			continue
		}
		if !orderKeysAreColumns(c.Proj.OrderBy, &b.Proj) {
			continue
		}
		b.Proj.OrderBy = c.Proj.OrderBy
		b.Proj.Skip = c.Proj.Skip
		b.Proj.Limit = c.Proj.Limit
		c.Proj.OrderBy = nil
		c.Proj.Skip = nil
		c.Proj.Limit = nil
	}
}

// sortLimitDonor: a stage-less, filter-less, non-distinct,
// non-aggregated projection segment carrying both an ORDER BY and a
// LIMIT (a bound; ORDER BY alone retains everything and buys nothing).
func sortLimitDonor(c *Segment) bool {
	return len(c.Stages) == 0 && c.PostWhere == nil &&
		!c.Proj.Distinct && !c.Proj.Aggregated &&
		len(c.Proj.Post) == 0 && c.Proj.NHidden == 0 &&
		len(c.Proj.OrderBy) > 0 && c.Proj.Limit != nil
}

// sortLimitReceiver: no ordering or pagination of its own to compete
// with the donated one, and no post-where the donation would leapfrog.
func sortLimitReceiver(b *Segment) bool {
	return len(b.Proj.OrderBy) == 0 && b.Proj.Limit == nil &&
		b.Proj.Skip == nil && b.PostWhere == nil
}

// orderKeysAreColumns reports whether every sort key is a bare variable
// naming one of the receiver's output columns -- the only form whose
// meaning is unchanged by evaluation in the receiver's scope (an
// arbitrary expression could collide with a receiver match variable of
// the same name).
func orderKeysAreColumns(order []ast.SortItem, b *ProjPlan) bool {
	for i := range order {
		v, ok := order[i].Expr.(*ast.Var)
		if !ok {
			return false
		}
		found := false
		for _, c := range b.Columns {
			if c == v.Name {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
