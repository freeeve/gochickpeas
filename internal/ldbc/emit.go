// The rustychickpeas-ldbc suite's JSONL timing schema (their
// python/cypher/timings.py) plus the stamping helpers, shared by the
// native-kernel emitter (cmd/ldbcbench) and the GQL runner (cmd/gqlbench).

package ldbc

import (
	"fmt"
	"os/exec"
	"strings"
)

// Record is one emitted timing row in the suite's shared JSONL schema.
type Record struct {
	Family         string  `json:"family"`
	Query          string  `json:"query"`
	Variant        string  `json:"variant"`
	Engine         string  `json:"engine"`
	Warmth         string  `json:"warmth"`
	Ms             float64 `json:"ms"`
	Rows           int     `json:"rows"`
	SF             int     `json:"sf"`
	Shape          string  `json:"shape"`
	Parity         string  `json:"parity"`
	EngineCommit   string  `json:"engineCommit"`
	EngineDate     string  `json:"engineDate"`
	EngineDateTime string  `json:"engineDateTime"`
	EngineSubject  string  `json:"engineSubject"`
	LdbcCommit     *string `json:"ldbcCommit"`
	LdbcDate       *string `json:"ldbcDate"`
	LdbcDirty      bool    `json:"ldbcDirty"`
	MeasuredDate   string  `json:"measuredDate"`
	Source         string  `json:"source"`
	MsMin          float64 `json:"msMin"`
	MsP25          float64 `json:"msP25"`
	MsP75          float64 `json:"msP75"`
	MsN            int     `json:"msN"`
	Meta           Meta    `json:"meta"`
}

// Meta self-describes the emitted point: which port, at what conformance
// level, against which fixture, on which Go toolchain.
type Meta struct {
	Port            string `json:"port"`
	CoreConformance string `json:"coreConformance,omitempty"`
	CoreCommit      string `json:"coreCommit,omitempty"`
	GQLVersion      string `json:"gqlVersion,omitempty"`
	Graph           string `json:"graph,omitempty"`
	GoVersion       string `json:"goVersion"`
	Nodes           uint32 `json:"nodes"`
	Rels            uint64 `json:"rels"`
}

// Stamp is the emitting repo's HEAD identity -- gochickpeas points plot
// on this repo's own commit timeline.
type Stamp struct {
	Commit, Date, DateTime, Subject string
}

// HeadStamp reads the current repo HEAD (7-hex commit, ISO date and
// datetime, subject) for record stamping.
func HeadStamp() (Stamp, error) {
	out, err := exec.Command("git", "log", "-1", "--format=%H%x00%cs%x00%cI%x00%s").Output()
	if err != nil {
		return Stamp{}, fmt.Errorf("git log: %w", err)
	}
	parts := strings.Split(strings.TrimRight(string(out), "\n"), "\x00")
	if len(parts) != 4 {
		return Stamp{}, fmt.Errorf("git log returned %d fields, want 4", len(parts))
	}
	return Stamp{Commit: parts[0][:7], Date: parts[1], DateTime: parts[2], Subject: parts[3]}, nil
}

// Percentile linearly interpolates over an ascending-sorted sample.
func Percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := p * float64(len(sorted)-1)
	lo := int(pos)
	frac := pos - float64(lo)
	if lo+1 >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	return sorted[lo]*(1-frac) + sorted[lo+1]*frac
}
