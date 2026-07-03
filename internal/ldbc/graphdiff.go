// Structural graph-diff for the load benchmark (task 029), mirroring
// rustychickpeas-ldbc python/cypher/graph_diff.py: a load benchmark is
// only meaningful if every format reconstructs the *same* graph, so a
// reloaded snapshot is diffed against the canonical baseline before its
// throughput counts. The comparison is layered cheapest-first and
// returns on the first mismatch:
//
//  1. totals        -- NodeCount + RelCount.
//  2. per-label     -- node count per label; rel count per type.
//  3. sampled props -- for K nodes per label (ordered by external id),
//     every property matches. Catches value-level corruption a count
//     can't see.
//  4. full (opt)    -- the complete external-id set per label. Strongest
//     identity check; O(nodes).
//
// Dense NodeIDs are assigned per load and differ between snapshots, so
// everything keys off the external id property (DiffOpts.IDProp), never
// the internal id -- the diff is order- and id-assignment-independent.
// Nodes without the id property (e.g. RDF blank nodes) are covered by
// the count layers but skipped by the id-keyed ones.

package ldbc

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	chickpeas "github.com/freeeve/gochickpeas"
)

// DiffOpts configures DiffGraphs. IDProp is the external id property
// every identity-keyed layer uses ("uri" for SPB, "id" for BI-style
// graphs). Labels and RelTypes gate layer 2 and default to the union of
// both snapshots' schemas; NodeProps ({label: [prop, ...]}) gates layer
// 3 and defaults to every property a sampled node carries; Sample is K
// per label (default 50); Full enables the exhaustive layer 4.
type DiffOpts struct {
	IDProp    string
	Labels    []string
	RelTypes  []string
	NodeProps map[string][]string
	Sample    int
	Full      bool
}

// DiffGraphs compares test against the ref baseline. ok is true on a
// full match; detail is "MATCH" or a "<layer>: <ref> vs <test>" string
// naming the first divergence.
func DiffGraphs(ref, test *chickpeas.Snapshot, opts DiffOpts) (ok bool, detail string) {
	if rn, tn := ref.NodeCount(), test.NodeCount(); rn != tn {
		return false, fmt.Sprintf("node_count: %d vs %d", rn, tn)
	}
	if rr, tr := ref.RelCount(), test.RelCount(); rr != tr {
		return false, fmt.Sprintf("rel_count: %d vs %d", rr, tr)
	}

	labels := opts.Labels
	if labels == nil {
		labels = diffUnion(ref.Labels(), test.Labels())
	}
	for _, lbl := range labels {
		if a, b := diffLabelCount(ref, lbl), diffLabelCount(test, lbl); a != b {
			return false, fmt.Sprintf("label %s: %d vs %d", lbl, a, b)
		}
	}

	relTypes := opts.RelTypes
	if relTypes == nil {
		relTypes = diffUnion(ref.RelTypes(), test.RelTypes())
	}
	for _, rt := range relTypes {
		if a, b := ref.RelTypeCount(rt), test.RelTypeCount(rt); a != b {
			return false, fmt.Sprintf("rel %s: %d vs %d", rt, a, b)
		}
	}

	sample := opts.Sample
	if sample <= 0 {
		sample = 50
	}
	propLabels, propsOf := labels, func(string) []string { return nil }
	if opts.NodeProps != nil {
		propLabels = slices.Sorted(maps.Keys(opts.NodeProps))
		propsOf = func(lbl string) []string { return opts.NodeProps[lbl] }
	}
	for _, lbl := range propLabels {
		a := diffSampleRows(ref, lbl, opts.IDProp, propsOf(lbl), sample)
		b := diffSampleRows(test, lbl, opts.IDProp, propsOf(lbl), sample)
		if !slices.Equal(a, b) {
			return false, fmt.Sprintf("props %s: sample of %d rows differs", lbl, len(a))
		}
	}

	if opts.Full {
		for _, lbl := range labels {
			a, b := diffIDSet(ref, lbl, opts.IDProp), diffIDSet(test, lbl, opts.IDProp)
			if onlyRef, onlyTest := diffSetGap(a, b), diffSetGap(b, a); onlyRef+onlyTest > 0 {
				return false, fmt.Sprintf("id-set %s: %d ref-only, %d test-only", lbl, onlyRef, onlyTest)
			}
		}
	}

	return true, "MATCH"
}

// diffUnion merges two sorted name lists into one sorted, deduplicated
// list, so layer 2 covers a label/type present in either snapshot.
func diffUnion(a, b []string) []string {
	out := append(append(make([]string, 0, len(a)+len(b)), a...), b...)
	slices.Sort(out)
	return slices.Compact(out)
}

// diffLabelCount is the node count for a label, 0 when absent.
func diffLabelCount(g *chickpeas.Snapshot, label string) int {
	set, ok := g.NodesWithLabel(label)
	if !ok {
		return 0
	}
	return set.Len()
}

// diffSampleRows renders the first k nodes of label ordered by external
// id as canonical "extid|key=value|..." rows. props nil means every key
// the node carries; nodes without the id property are skipped (they have
// no cross-snapshot identity to sample under).
func diffSampleRows(g *chickpeas.Snapshot, label, idProp string, props []string, k int) []string {
	ids := diffIDSet(g, label, idProp)
	extIDs := slices.Sorted(maps.Keys(ids))
	if len(extIDs) > k {
		extIDs = extIDs[:k]
	}
	rows := make([]string, 0, len(extIDs))
	for _, ext := range extIDs {
		n := ids[ext]
		keys := props
		if keys == nil {
			keys = g.NodePropertyKeys(n)
		}
		var sb strings.Builder
		sb.WriteString(ext)
		for _, key := range keys {
			sb.WriteByte('|')
			sb.WriteString(key)
			sb.WriteByte('=')
			sb.WriteString(diffPropString(g, n, key))
		}
		rows = append(rows, sb.String())
	}
	return rows
}

// diffIDSet maps each external id under label to its node (layer 3/4
// key space). A duplicate external id keeps the first node seen; the
// count layers already catch cardinality drift.
func diffIDSet(g *chickpeas.Snapshot, label, idProp string) map[string]chickpeas.NodeID {
	set, ok := g.NodesWithLabel(label)
	if !ok {
		return nil
	}
	ids := make(map[string]chickpeas.NodeID, set.Len())
	for raw := range set.Iter() {
		n := chickpeas.NodeID(raw)
		ext := diffPropString(g, n, idProp)
		if ext == "" {
			continue
		}
		if _, dup := ids[ext]; !dup {
			ids[ext] = n
		}
	}
	return ids
}

// diffSetGap counts keys of a absent from b.
func diffSetGap(a, b map[string]chickpeas.NodeID) int {
	gap := 0
	for k := range a {
		if _, ok := b[k]; !ok {
			gap++
		}
	}
	return gap
}

// diffPropString renders a property value in a canonical kind-tagged
// form comparable across snapshots (string atoms are interned per
// snapshot, so raw Values never compare across two). Absent renders "".
func diffPropString(g *chickpeas.Snapshot, n chickpeas.NodeID, key string) string {
	p := g.Prop(n, key)
	v, ok := p.Value()
	if !ok {
		return ""
	}
	switch v.Kind() {
	case chickpeas.KindStr:
		s, _ := p.Str()
		return "s:" + s
	case chickpeas.KindI64:
		i, _ := v.I64()
		return "i:" + strconv.FormatInt(i, 10)
	case chickpeas.KindF64:
		f, _ := v.F64()
		return "f:" + strconv.FormatFloat(f, 'g', -1, 64)
	case chickpeas.KindBool:
		b, _ := v.Bool()
		return "b:" + strconv.FormatBool(b)
	}
	return ""
}
