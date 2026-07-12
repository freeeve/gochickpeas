// Package explain renders a plan as text for EXPLAIN/PROFILE (port of the
// Rust explain.rs, trimmed of the recognizer-kernel operators and rank-mode
// gating -- estimates always render). PROFILE per-operator counts arrive
// via the Profile seam, populated by the executor in a later milestone.
package explain

import (
	"fmt"
	"strings"
	"time"

	"github.com/freeeve/gochickpeas/gql/internal/ast"
	"github.com/freeeve/gochickpeas/gql/internal/graph"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
)

// Profile carries PROFILE's actual per-operator row counts, in the same
// branch-major segment order the renderer walks.
type Profile struct {
	Segs []SegProf
}

// SegProf profiles one segment.
type SegProf struct {
	Stages        []StageProf
	ProjRows      uint64
	PostWhereRows *uint64
}

// StageProf profiles one stage: Match holds one count per op plus a
// trailing one for the stage WHERE; Single is a one-output stage.
type StageProf struct {
	Match  []uint64
	Single *uint64
}

// Render renders the plan tree with estimates (always) and PROFILE counts
// (when prof is non-nil), as one line per operator.
func Render(p *plan.Plan, prof *Profile, planTime time.Duration, est *plan.Estimates) []string {
	var out []string
	if prof != nil {
		out = append(out, "PROFILE")
	} else {
		out = append(out, "EXPLAIN")
	}
	out = append(out, fmt.Sprintf("Planning: %.3f ms", planTime.Seconds()*1000))
	if est != nil {
		if prof != nil {
			out = append(out, fmt.Sprintf("%-54s %10s %10s", "", "est", "rows"))
		} else {
			out = append(out, fmt.Sprintf("%-54s %16s", "", "est"))
		}
	}
	render(&out, p, prof, est)
	return out
}

func render(out *[]string, p *plan.Plan, prof *Profile, est *plan.Estimates) {
	multiBranch := len(p.Branches) > 1
	gseg := 0
	for bi, segments := range p.Branches {
		bind := ""
		if multiBranch {
			if bi > 0 {
				if p.Union[bi-1] == ast.UnionAll {
					*out = append(*out, "UNION ALL")
				} else {
					*out = append(*out, "UNION")
				}
			}
			*out = append(*out, fmt.Sprintf("Branch %d", bi))
			bind = "  "
		}
		multi := len(segments) > 1
		for si, seg := range segments {
			var sp *SegProf
			if prof != nil && gseg < len(prof.Segs) {
				sp = &prof.Segs[gseg]
			}
			var se *plan.SegEst
			if est != nil && gseg < len(est.Segs) {
				se = &est.Segs[gseg]
			}
			gseg++
			ind := bind
			if multi {
				*out = append(*out, fmt.Sprintf("%sSegment %d", bind, si))
				ind = bind + "  "
			}
			renderSegment(out, seg, ind, sp, se)
		}
	}
}

func renderSegment(out *[]string, seg *plan.Segment, ind string, sp *SegProf, se *plan.SegEst) {
	// slot -> variable name (anonymous slots render as _).
	names := make([]string, seg.RowWidth)
	for name, slot := range seg.Slots {
		if slot < len(names) {
			names[slot] = name
		}
	}

	for ti, stage := range seg.Stages {
		switch s := stage.(type) {
		case *plan.MatchStage:
			var opRows, opEsts []uint64
			if sp != nil && ti < len(sp.Stages) {
				opRows = sp.Stages[ti].Match
			}
			if se != nil && ti < len(se.Stages) {
				opEsts = se.Stages[ti].Match
			}
			note := ""
			if se != nil && ti < len(se.AnchorNotes) {
				note = se.AnchorNotes[ti]
			}
			for oi := range s.Ops {
				line(out, ind, opLabel(&s.Ops[oi], names), at(opEsts, oi), at(opRows, oi))
				if oi == 0 && note != "" {
					*out = append(*out, ind+"  "+note)
				}
			}
			if s.Where != nil {
				line(out, ind, "Filter ("+fmtExpr(s.Where)+")", at(opEsts, len(s.Ops)), at(opRows, len(s.Ops)))
			}
		case *plan.HashJoinStage:
			label := fmt.Sprintf("HashJoin (key=%s, probe %s)", nameOf(s.KeySlot, names),
				fmtHop(nameOf(s.Probe.From, names), s.Probe.Dir, s.Probe.Types, nameOf(s.Probe.To, names), ""))
			line(out, ind, label, singleEst(se, ti), singleCount(sp, ti))
			for _, bs := range s.Build {
				for oi := range bs.Ops {
					line(out, ind+"  build: ", opLabel(&bs.Ops[oi], names), nil, nil)
				}
				if bs.Where != nil {
					line(out, ind+"  build: ", "Filter ("+fmtExpr(bs.Where)+")", nil, nil)
				}
			}
			if s.Where != nil {
				line(out, ind, "Filter ("+fmtExpr(s.Where)+")", nil, nil)
			}
		case *plan.SpStage:
			kind := "ShortestPath"
			if s.All {
				kind = "AllShortestPaths"
			} else if s.Weight != nil {
				kind = "WeightedShortestPath"
			}
			label := fmt.Sprintf("%s (%s)-[*]-(%s)", kind, nameOf(s.From, names), nameOf(s.To, names))
			line(out, ind, label, singleEst(se, ti), singleCount(sp, ti))
		case *plan.GateStage:
			kind := "ShortestPath"
			if s.Sp.Weight != nil {
				kind = "WeightedShortestPath"
			}
			label := fmt.Sprintf("Gate (%s (%s)-[*]-(%s), %s)",
				kind, nameOf(s.Sp.From, names), nameOf(s.Sp.To, names), fmtExpr(s.Where))
			line(out, ind, label, singleEst(se, ti), singleCount(sp, ti))
		case *plan.CallStage:
			label := callLabel(&s.Proc)
			if s.ProcName != "" {
				label = fmt.Sprintf("%s(…) [correlated, %d args]", s.ProcName, len(s.ArgExprs))
			}
			line(out, ind, "Call "+label, singleEst(se, ti), singleCount(sp, ti))
		case *plan.UnwindStage:
			label := fmt.Sprintf("Unwind (%s AS %s)", fmtExpr(s.List), nameOf(s.OutSlot, names))
			line(out, ind, label, singleEst(se, ti), singleCount(sp, ti))
		case *plan.CallSubqueryStage:
			line(out, ind, "CallSubquery ["+strings.Join(s.Sub.Columns, ", ")+"]", singleEst(se, ti), singleCount(sp, ti))
			// Render the nested sub-plan indented under the operator,
			// dropping the sub-render's own header lines.
			sub := Render(s.Sub, nil, 0, nil)
			for _, l := range sub[2:] {
				*out = append(*out, ind+"    "+l)
			}
		}
	}

	proj := &seg.Proj
	var pc, pe *uint64
	if sp != nil {
		pc = &sp.ProjRows
	}
	if se != nil {
		pe = se.ProjRows
	}
	if proj.Aggregated {
		var groups []string
		for _, gi := range proj.GroupIdx {
			groups = append(groups, proj.Returns[gi].Name)
		}
		var aggs []string
		for i := range proj.Aggs {
			aggs = append(aggs, fmtAgg(&proj.Aggs[i]))
		}
		line(out, ind, fmt.Sprintf("Aggregate (group=[%s]; %s)", strings.Join(groups, ", "), strings.Join(aggs, ", ")), pe, pc)
	} else {
		kw := ""
		if proj.Distinct {
			kw = "Distinct "
		}
		line(out, ind, fmt.Sprintf("%sProject [%s]", kw, strings.Join(proj.Columns, ", ")), pe, pc)
	}
	if len(proj.OrderBy) > 0 {
		var keys []string
		for _, s := range proj.OrderBy {
			keys = append(keys, fmtSort(s))
		}
		line(out, ind, "OrderBy ["+strings.Join(keys, ", ")+"]", nil, nil)
	}
	if proj.Skip != nil {
		line(out, ind, fmt.Sprintf("Offset %d", *proj.Skip), nil, nil)
	}
	if proj.Limit != nil {
		line(out, ind, fmt.Sprintf("Limit %d", *proj.Limit), nil, nil)
	}
	if seg.PostWhere != nil {
		var e, c *uint64
		if se != nil {
			e = se.PostWhereRows
		}
		if sp != nil {
			c = sp.PostWhereRows
		}
		line(out, ind, "Filter ("+fmtExpr(seg.PostWhere)+")", e, c)
	}
}

// at is a bounds-checked element pointer into a count slice.
func at(v []uint64, i int) *uint64 {
	if i < 0 || i >= len(v) {
		return nil
	}
	return &v[i]
}

func singleCount(sp *SegProf, ti int) *uint64 {
	if sp == nil || ti >= len(sp.Stages) {
		return nil
	}
	return sp.Stages[ti].Single
}

func singleEst(se *plan.SegEst, ti int) *uint64 {
	if se == nil || ti >= len(se.Stages) {
		return nil
	}
	return se.Stages[ti].Single
}

// line appends one operator line with up to two right-aligned numeric
// columns (est, then the PROFILE actual).
func line(out *[]string, indent, label string, est, actual *uint64) {
	switch {
	case est == nil && actual == nil:
		*out = append(*out, indent+label)
	case est != nil && actual != nil:
		*out = append(*out, fmt.Sprintf("%s%-54s %10s %10s", indent, label, plan.GroupDigits(*est), plan.GroupDigits(*actual)))
	case est != nil:
		*out = append(*out, fmt.Sprintf("%s%-54s %16s", indent, label, plan.GroupDigits(*est)))
	default:
		*out = append(*out, fmt.Sprintf("%s%-54s %16s", indent, label, plan.GroupDigits(*actual)))
	}
}

func nameOf(slot int, names []string) string {
	if slot >= 0 && slot < len(names) && names[slot] != "" {
		return names[slot]
	}
	return "_"
}

func opLabel(op *plan.BindOp, names []string) string {
	switch op.Kind {
	case plan.OpScan:
		v := nameOf(op.Slot, names)
		src := &op.Source
		switch src.Kind {
		case plan.ScanProperty:
			return fmt.Sprintf("NodeByProperty (%s:%s {%s = %s})", v, src.Label, src.Key, fmtLit(src.Value))
		case plan.ScanLabel:
			return fmt.Sprintf("NodeScan (%s:%s%s)", v, src.Label, fmtProps(op.Props))
		case plan.ScanNodeID:
			return fmt.Sprintf("NodeBySeek (%s = id %s)", v, fmtLit(src.Value))
		case plan.ScanNodeIDVar:
			return fmt.Sprintf("NodeBySeek (%s = id %s)", v, nameOf(src.Slot, names))
		case plan.ScanTextMatch:
			return fmt.Sprintf("NodeByTextIndex (%s:%s {%s %s %s})", v, src.Label, src.Field, binopStr(src.Mode), fmtLit(src.Value))
		case plan.ScanExistsSeed:
			label := src.Label
			if label == "" {
				label = "*"
			}
			return fmt.Sprintf("NodeByExistsSeed (%s:%s, %d chain(s))", v, label, len(src.Seeds))
		case plan.ScanArg:
			return fmt.Sprintf("Argument (%s%s%s)", v, fmtLabels(op.Labels), fmtProps(op.Props))
		default:
			return fmt.Sprintf("AllNodesScan (%s)", v)
		}
	case plan.OpExpand:
		pat := fmtHop(nameOf(op.From, names), op.Dir, op.Types, nameOf(op.To, names), "")
		if op.Rebind {
			return "Expand " + pat + " [into bound]"
		}
		return "Expand " + pat
	default: // OpVarExpand
		length := fmt.Sprintf("*%d..", op.Min)
		if op.Max != nil {
			length = fmt.Sprintf("*%d..%d", op.Min, *op.Max)
		}
		mono := ""
		if op.MonoHop != nil {
			ad := "desc"
			if op.MonoHop.Ascending {
				ad = "asc"
			}
			np := ""
			if op.MonoHop.NullsPass {
				np = " nullspass"
			}
			mono = fmt.Sprintf(" [mono %s %s%s]", op.MonoHop.RelKey, ad, np)
		}
		return "VarExpand " + fmtHop(nameOf(op.From, names), op.Dir, op.Types, nameOf(op.To, names), length) + mono
	}
}

func fmtHop(from string, dir graph.Direction, types []string, to, length string) string {
	t := ""
	if len(types) > 0 {
		t = ":" + strings.Join(types, "|")
	}
	body := "[" + t + length + "]"
	switch dir {
	case graph.Outgoing:
		return fmt.Sprintf("(%s)-%s->(%s)", from, body, to)
	case graph.Incoming:
		return fmt.Sprintf("(%s)<-%s-(%s)", from, body, to)
	}
	return fmt.Sprintf("(%s)-%s-(%s)", from, body, to)
}

func fmtLabels(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	return ":" + strings.Join(labels, ":")
}

func fmtProps(props []ast.PropEntry) string {
	if len(props) == 0 {
		return ""
	}
	kv := make([]string, len(props))
	for i, p := range props {
		kv[i] = p.Key + ": " + fmtLit(p.Val)
	}
	return " {" + strings.Join(kv, ", ") + "}"
}

// callLabel is a short human-readable label for a CALL procedure.
func callLabel(p *plan.CallProc) string {
	switch p.Kind {
	case plan.ProcWcc:
		return fmt.Sprintf("wcc('%s')", p.RelType)
	case plan.ProcFtsSearch:
		return fmt.Sprintf("fts.search('%s', '%s', '%s')", p.Label, p.Field, p.Query)
	case plan.ProcGeoWithinRadius:
		return fmt.Sprintf("geo.withinRadius('%s', …)", p.Label)
	case plan.ProcGeoWithinBBox:
		return fmt.Sprintf("geo.withinBBox('%s', …)", p.Label)
	case plan.ProcBfs:
		return fmt.Sprintf("algo.bfs(%d, …)", p.Source)
	case plan.ProcPageRank:
		return "algo.pagerank(…)"
	case plan.ProcWccAll:
		return "algo.wcc()"
	case plan.ProcCdlp:
		return "algo.cdlp(…)"
	case plan.ProcLcc:
		return "algo.lcc(…)"
	case plan.ProcPropagate:
		return fmt.Sprintf("algo.propagate(%d seeds, %s, depth %d, …)", len(p.Seeds), strings.Join(p.RelTypes, "|"), p.MaxDepth)
	default:
		return fmt.Sprintf("algo.sssp(%d, …)", p.Source)
	}
}
