package attribute

import (
	"testing"
	"time"

	"github.com/Dillonsmart/docket/internal/diffx"
	"github.com/Dillonsmart/docket/internal/timeline"
	"github.com/Dillonsmart/docket/internal/transcript"
)

func edit(id string, seq int, path string, pre, post []string) *transcript.FileEdit {
	return &transcript.FileEdit{
		ID: id, Tool: "Edit", Path: path, Sequence: seq,
		At:  time.Date(2026, 9, 13, 10, 0, seq, 0, time.UTC),
		Pre: pre, Post: post, HasPre: true, HasPost: true,
		Actor: transcript.ActorAgent, AgentID: "claude-code/main", SessionID: "s1",
	}
}

func sessionOf(edits ...*transcript.FileEdit) []*transcript.Session {
	s := &transcript.Session{ID: "s1"}
	for _, e := range edits {
		s.Edits = append(s.Edits, e)
		s.Events = append(s.Events, e)
	}
	return []*transcript.Session{s}
}

const diff = `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -1,0 +1,2 @@
+package main
+// written by the agent
`

func TestAttributesCommittedLinesToTheEditThatWroteThem(t *testing.T) {
	head := []string{"package main", "// written by the agent"}
	e1 := edit("e1", 1, "/repo/a.go", nil, head)
	tl := timeline.Build("/repo", sessionOf(e1))

	hunks := diffx.ParseUnified(diff)[0].Hunks
	got := File("a.go", tl.EditsFor("a.go"), head, hunks, tl.Edits)
	if len(got) != 1 {
		t.Fatalf("got %d hunks", len(got))
	}
	h := got[0]
	if h.Attributed != 2 || h.Unknown != 0 {
		t.Errorf("attributed %d, unknown %d", h.Attributed, h.Unknown)
	}
	if h.Confidence != High {
		t.Errorf("confidence = %q, want high", h.Confidence)
	}
	if h.Primary == nil || h.Primary.ID != "e1" {
		t.Errorf("primary = %+v", h.Primary)
	}
	if h.Verified != 2 {
		t.Errorf("verified = %d, want 2 (both lines found in the edit's own output)", h.Verified)
	}
}

// Nothing in the transcript wrote this file, so the only correct answer is that
// docket does not know.
func TestUnexplainedLinesAreUnknownNotGuessed(t *testing.T) {
	head := []string{"package main", "// written by the agent"}
	other := edit("e1", 1, "/repo/other.go", nil, []string{"something else"})
	tl := timeline.Build("/repo", sessionOf(other))

	hunks := diffx.ParseUnified(diff)[0].Hunks
	got := File("a.go", tl.EditsFor("a.go"), head, hunks, tl.Edits)
	h := got[0]
	if h.Attributed != 0 || h.Unknown != 2 {
		t.Errorf("attributed %d, unknown %d — nothing should have been attributed", h.Attributed, h.Unknown)
	}
	if h.Confidence != None {
		t.Errorf("confidence = %q, want none", h.Confidence)
	}
	if h.Reasons[timeline.ReasonNotInTimeline] != 2 {
		t.Errorf("reasons = %+v", h.Reasons)
	}
	if !h.NoTimeline {
		t.Error("hunk should be flagged as having no timeline at all")
	}
}

// The committed file is the arbiter: content written in the session but changed
// again afterwards must not be credited to the session's edit.
func TestLinesChangedAfterTheSessionAreNotAttributed(t *testing.T) {
	e1 := edit("e1", 1, "/repo/a.go", nil, []string{"package main", "// written by the agent"})
	tl := timeline.Build("/repo", sessionOf(e1))

	head := []string{"package main", "// hand-edited afterwards"}
	hunks := diffx.ParseUnified(diff)[0].Hunks
	got := File("a.go", tl.EditsFor("a.go"), head, hunks, tl.Edits)
	h := got[0]
	if h.Attributed != 1 {
		t.Errorf("attributed = %d, want 1 (only the unchanged line)", h.Attributed)
	}
	if h.Unknown != 1 {
		t.Errorf("unknown = %d, want 1", h.Unknown)
	}
}

func TestAttemptsSurfaceOnTheHunkThatReplacedThem(t *testing.T) {
	first := []string{"package main", "// first attempt"}
	final := []string{"package main", "// written by the agent"}
	e1 := edit("e1", 1, "/repo/a.go", nil, first)
	e2 := edit("e2", 2, "/repo/a.go", first, final)
	tl := timeline.Build("/repo", sessionOf(e1, e2))

	hunks := diffx.ParseUnified(diff)[0].Hunks
	got := File("a.go", tl.EditsFor("a.go"), final, hunks, tl.Edits)
	if len(got[0].Attempts) != 1 {
		t.Fatalf("got %d attempts, want 1: %+v", len(got[0].Attempts), got[0].Attempts)
	}
	if got[0].Attempts[0].EditID != "e1" {
		t.Errorf("attempt = %+v", got[0].Attempts[0])
	}
}

func TestSummaryRates(t *testing.T) {
	var s Summary
	s.Add([]Hunk{
		{AddedLines: 10, Attributed: 10, Verified: 10},
		{AddedLines: 10, Attributed: 0, Unknown: 10, NoTimeline: true},
		{DeletionOnly: true},
	})
	if s.Hunks != 2 || s.DeletionHunks != 1 {
		t.Errorf("hunks = %d, deletion-only = %d", s.Hunks, s.DeletionHunks)
	}
	if s.HunkRate() != 0.5 || s.LineRate() != 0.5 || s.VerifyRate() != 1 {
		t.Errorf("rates: hunk %.2f line %.2f verify %.2f", s.HunkRate(), s.LineRate(), s.VerifyRate())
	}
	// The in-scope rate ignores the file the session never touched.
	if s.InScopeHunkRate() != 1 {
		t.Errorf("in-scope rate = %.2f, want 1", s.InScopeHunkRate())
	}
}
