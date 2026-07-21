package ldbc

import (
	"strings"
	"testing"
)

// TestNewStampDirtyMarking pins the build-integrity contract (task 209):
// a clean tree stamps the plain 7-hex commit, a dirty tree appends the
// `-dirty` marker so an emitted artifact cannot be mistaken for -- or
// deduped against -- the clean commit's canonical run. Only the commit
// field is touched; date/datetime/subject pass through verbatim.
func TestNewStampDirtyMarking(t *testing.T) {
	const sha, date, dt, subj = "1a865ff", "2026-07-21", "2026-07-21T10:00:00-04:00", "some subject"
	clean := newStamp(sha, date, dt, subj, false)
	if clean.Commit != sha {
		t.Errorf("clean commit = %q, want %q", clean.Commit, sha)
	}
	dirty := newStamp(sha, date, dt, subj, true)
	if dirty.Commit != sha+"-dirty" {
		t.Errorf("dirty commit = %q, want %q", dirty.Commit, sha+"-dirty")
	}
	// A dirty stamp must not equal the clean commit under the string key
	// canary/dedup use, and must still carry the full sha as a prefix.
	if dirty.Commit == clean.Commit {
		t.Error("dirty and clean commits collide")
	}
	if !strings.HasPrefix(dirty.Commit, sha) {
		t.Errorf("dirty commit %q lost the sha prefix", dirty.Commit)
	}
	for _, tc := range []Stamp{clean, dirty} {
		if tc.Date != date || tc.DateTime != dt || tc.Subject != subj {
			t.Errorf("non-commit fields altered: %+v", tc)
		}
	}
}

// TestHeadStampRuns exercises the live git path in-repo: it must return a
// non-empty commit and never error inside a working checkout. (The tree
// may or may not be dirty during a run, so the marker is not asserted
// here -- newStamp's test pins the marking contract deterministically.)
func TestHeadStampRuns(t *testing.T) {
	s, err := HeadStamp()
	if err != nil {
		t.Fatalf("HeadStamp: %v", err)
	}
	if s.Commit == "" {
		t.Error("empty commit")
	}
	// The 7-hex core (before any -dirty suffix) must be present.
	core := strings.TrimSuffix(s.Commit, "-dirty")
	if len(core) != 7 {
		t.Errorf("commit core = %q, want 7 hex chars", core)
	}
}
