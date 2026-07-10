package chickpeas_test

import (
	"errors"
	"slices"
	"sort"
	"testing"

	chickpeas "github.com/freeeve/gochickpeas"
)

// aggFixture: 6 Person nodes with age/score/city, 2 Bot nodes, and LIKES
// rels for Through/Hop.
func aggFixture(t *testing.T) *chickpeas.Snapshot {
	t.Helper()
	b := chickpeas.NewBuilder(16, 16)
	ages := []int64{25, 30, 35, 40, 45, 50}
	scores := []int64{1, 2, 3, 4, 5, 6}
	cities := []string{"oslo", "oslo", "bergen", "bergen", "oslo", "bergen"}
	for i, age := range ages {
		id := chickpeas.NodeID(i)
		b.AddNodeWithID(id, "Person")
		b.SetProp(id, "age", age)
		b.SetProp(id, "score", scores[i])
		b.SetProp(id, "city", cities[i])
	}
	b.AddNodeWithID(6, "Bot")
	b.AddNodeWithID(7, "Bot")
	b.SetProp(6, "age", int64(99))
	b.SetProp(7, "age", int64(98))
	// LIKES: 0->1, 0->2, 1->2, 3->2
	for _, r := range [][2]chickpeas.NodeID{{0, 1}, {0, 2}, {1, 2}, {3, 2}} {
		b.AddRel(r[0], r[1], "LIKES")
	}
	// creationDate on node 0: 2010-03-15T07:30:45Z = 1268638245000 ms.
	b.SetProp(0, "created", int64(1268638245000))
	return b.Finalize()
}

// sumOf unwraps a row's Sum, with a sentinel for the nil (overflow) case
// so mismatch failures print recognizably.
func sumOf(r chickpeas.AggRow) int64 {
	if r.Sum == nil {
		return -1 << 62
	}
	return *r.Sum
}

func rowsByKey(res *chickpeas.AggResult) map[int64]chickpeas.AggRow {
	out := map[int64]chickpeas.AggRow{}
	for _, r := range res.Rows {
		k := int64(-1)
		if len(r.Key) > 0 {
			k = r.Key[0]
		}
		out[k] = r
	}
	return out
}

func TestAggregateFilterGroupSum(t *testing.T) {
	g := aggFixture(t)
	res, err := g.Aggregate("Person").
		Filter("age", chickpeas.OpGe, 30).
		Bin("age", 35, 45).
		Sum("score").
		Run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 5 {
		t.Fatalf("total: %d", res.Total)
	}
	if !slices.Equal(res.Fields, []string{"age_bin"}) {
		t.Fatalf("fields: %v", res.Fields)
	}
	rows := rowsByKey(res)
	// bins: <35 -> 0 {30}, [35,45) -> 1 {35,40}, >=45 -> 2 {45,50}
	if rows[0].Count != 1 || sumOf(rows[0]) != 2 {
		t.Fatalf("bin 0: %+v", rows[0])
	}
	if rows[1].Count != 2 || sumOf(rows[1]) != 3+4 {
		t.Fatalf("bin 1: %+v", rows[1])
	}
	if rows[2].Count != 2 || sumOf(rows[2]) != 5+6 {
		t.Fatalf("bin 2: %+v", rows[2])
	}
}

func TestAggregateByLabelAndHaving(t *testing.T) {
	g := aggFixture(t)
	res, err := g.Aggregate("Person", "Bot").ByLabel().Having("age", chickpeas.OpGt, 40).Run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 8 {
		t.Fatalf("total: %d", res.Total)
	}
	rows := rowsByKey(res)
	if rows[0].Count != 2 { // persons 45, 50
		t.Fatalf("person row: %+v", rows[0])
	}
	if rows[1].Count != 2 { // both bots
		t.Fatalf("bot row: %+v", rows[1])
	}
	// ByLabelMembership: group persons by having the Bot label (all 0).
	res, err = g.Aggregate("Person").ByLabelMembership("Bot").Run()
	if err != nil {
		t.Fatal(err)
	}
	rows = rowsByKey(res)
	if rows[0].Count != 6 || len(res.Rows) != 1 {
		t.Fatalf("membership rows: %v", res.Rows)
	}
}

func TestAggregateByColumnAndPresence(t *testing.T) {
	g := aggFixture(t)
	// Group by raw score works when every node carries it.
	res, err := g.Aggregate("Person").By("score").Run()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 6 {
		t.Fatalf("rows: %v", res.Rows)
	}
	// Grouping by a column with absent values bails with ErrSchema.
	if _, err := g.Aggregate("Person").By("created").Run(); !errors.Is(err, chickpeas.ErrSchema) {
		t.Fatalf("expected schema bail, got %v", err)
	}
	// RequirePresent keeps only carriers.
	res, err = g.Aggregate("Person").RequirePresent("created").Run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 {
		t.Fatalf("present total: %d", res.Total)
	}
	// Unknown column/label errors.
	if _, err := g.Aggregate("Person").By("nope").Run(); !errors.Is(err, chickpeas.ErrSchema) {
		t.Fatal("unknown column accepted")
	}
	if _, err := g.Aggregate("Nope").Run(); !errors.Is(err, chickpeas.ErrSchema) {
		t.Fatal("unknown label accepted")
	}
	// A non-i64 column errors.
	if _, err := g.Aggregate("Person").By("city").Run(); !errors.Is(err, chickpeas.ErrSchema) {
		t.Fatal("str column accepted as i64 group")
	}
}

func TestAggregateTemporalComponent(t *testing.T) {
	g := aggFixture(t)
	res, err := g.Aggregate("Person").
		RequirePresent("created").
		TemporalComponent("created", chickpeas.UnitYear).
		TemporalComponent("created", chickpeas.UnitMonth).
		TemporalComponent("created", chickpeas.UnitDay).
		TemporalComponent("created", chickpeas.UnitHour).
		TemporalComponent("created", chickpeas.UnitMinute).
		Run()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows: %v", res.Rows)
	}
	// 2010-03-15T07:30:45Z; five dims exercise the >4 key overflow too.
	if !slices.Equal(res.Rows[0].Key, []int64{2010, 3, 15, 7, 30}) {
		t.Fatalf("components: %v", res.Rows[0].Key)
	}
	if !slices.Equal(res.Fields, []string{
		"created_year", "created_month", "created_day", "created_hour", "created_minute",
	}) {
		t.Fatalf("fields: %v", res.Fields)
	}
}

func TestAggregateThrough(t *testing.T) {
	g := aggFixture(t)
	res, err := g.Aggregate("Person").Through("LIKES", chickpeas.Outgoing).Run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 6 || !slices.Equal(res.Fields, []string{"neighbor"}) {
		t.Fatalf("through shape: total %d fields %v", res.Total, res.Fields)
	}
	rows := rowsByKey(res)
	if rows[2].Count != 3 || rows[1].Count != 1 {
		t.Fatalf("through rows: %v", res.Rows)
	}
	// Neighbor restriction.
	res, err = g.Aggregate("Person").Through("LIKES", chickpeas.Outgoing).OnlyNeighbors(1).Run()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0].Count != 1 {
		t.Fatalf("only-neighbors rows: %v", res.Rows)
	}
}

func TestAggregateFilterVia(t *testing.T) {
	g := threadFixture(t)
	creator, _ := g.RelType("CREATOR")
	toCreator := g.NeighborVia(creator, chickpeas.Outgoing)
	// Keep messages whose creator's day... creator nodes lack "day"; use
	// the projection to the thread root instead and filter on its day.
	replyOf, _ := g.RelType("REPLY_OF")
	roots := g.RootsVia(replyOf, chickpeas.Outgoing)
	day1, _ := g.Prop(0, "day").Value()
	res, err := g.Aggregate("Message").FilterVia(roots, "day", day1).Run()
	if err != nil {
		t.Fatal(err)
	}
	// Roots with day=1: root 0 (day 1) and root 1 (day 1) -- all 5 messages.
	if res.Total != 5 {
		t.Fatalf("filter via total: %d", res.Total)
	}
	day2, _ := g.Prop(2, "day").Value()
	res, err = g.Aggregate("Message").FilterVia(roots, "day", day2).Run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 0 {
		t.Fatalf("no root has day 2, got %d", res.Total)
	}
	_ = toCreator
}

func TestAggregateHops(t *testing.T) {
	g := aggFixture(t)
	// Two LIKES hops from every person: walks 0->1->2 (one walk).
	res, err := g.Aggregate("Person").
		Hop("LIKES", chickpeas.Outgoing).
		Hop("LIKES", chickpeas.Outgoing).
		Run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 6 || !slices.Equal(res.Fields, []string{"endpoint"}) {
		t.Fatalf("hops shape: %d %v", res.Total, res.Fields)
	}
	if len(res.Rows) != 1 || res.Rows[0].Key[0] != 2 || res.Rows[0].Count != 1 {
		t.Fatalf("hop rows: %v", res.Rows)
	}
	// One hop counts each rel once, grouped by endpoint.
	res, err = g.Aggregate("Person").Hop("LIKES", chickpeas.Outgoing).Run()
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(res.Rows, func(i, j int) bool { return res.Rows[i].Key[0] < res.Rows[j].Key[0] })
	if len(res.Rows) != 2 || res.Rows[0].Count != 1 || res.Rows[1].Count != 3 {
		t.Fatalf("one-hop rows: %v", res.Rows)
	}
	// Unsupported combinations error instead of silently ignoring.
	if _, err := g.Aggregate("Person").Hop("LIKES", chickpeas.Outgoing).Sum("score").Run(); !errors.Is(err, chickpeas.ErrSchema) {
		t.Fatal("hop+sum accepted")
	}
	// A missing hop type yields no walks.
	res, err = g.Aggregate("Person").Hop("NOPE", chickpeas.Outgoing).Run()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 0 {
		t.Fatalf("missing type walked: %v", res.Rows)
	}
}

func TestParseAggOp(t *testing.T) {
	for s, want := range map[string]chickpeas.AggOp{
		"<": chickpeas.OpLt, "<=": chickpeas.OpLe, ">": chickpeas.OpGt,
		">=": chickpeas.OpGe, "==": chickpeas.OpEq, "!=": chickpeas.OpNe,
	} {
		got, err := chickpeas.ParseAggOp(s)
		if err != nil || got != want {
			t.Fatalf("parse %q: %v/%v", s, got, err)
		}
	}
	if _, err := chickpeas.ParseAggOp("~"); !errors.Is(err, chickpeas.ErrSchema) {
		t.Fatal("bad op accepted")
	}
	if !chickpeas.OpNe.Test(1, 2) || chickpeas.OpEq.Test(1, 2) {
		t.Fatal("op semantics wrong")
	}
}
