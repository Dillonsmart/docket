package timeline

import (
	"testing"
	"time"

	"github.com/Dillonsmart/docket/internal/transcript"
)

func at(n int) time.Time { return time.Date(2026, 9, 13, 10, 0, n, 0, time.UTC) }

func edit(id string, seq int, path string, pre, post []string, hasPre bool) *transcript.FileEdit {
	return &transcript.FileEdit{
		ID: id, Tool: "Edit", Path: path, Sequence: seq, At: at(seq),
		Pre: pre, Post: post, HasPre: hasPre, HasPost: true,
		Actor: transcript.ActorAgent, AgentID: "claude-code/main",
		SessionID: "s1", Source: transcript.SourceTranscript,
	}
}

func session(events ...transcript.Event) *transcript.Session {
	s := &transcript.Session{ID: "s1", Events: events}
	for _, e := range events {
		if fe, ok := e.(*transcript.FileEdit); ok {
			s.Edits = append(s.Edits, fe)
		}
	}
	return s
}

func TestProvenanceFollowsLinesThroughLaterEdits(t *testing.T) {
	path := "/repo/a.go"
	e1 := edit("e1", 1, path, nil, []string{"one", "two"}, true)
	e2 := edit("e2", 2, path, []string{"one", "two"}, []string{"one", "inserted", "two"}, true)

	tl := Build("/repo", []*transcript.Session{session(e1, e2)})
	ft := tl.EditsFor("a.go")
	if ft == nil {
		t.Fatal("no timeline for a.go")
	}
	want := []string{"e1", "e2", "e1"}
	for i, w := range want {
		if ft.Prov[i].EditID != w {
			t.Errorf("line %d attributed to %q, want %q", i, ft.Prov[i].EditID, w)
		}
	}
}

// The pre-image of an edit is the ground truth. When it disagrees with the
// replay, the lines that changed underneath must become unknown rather than
// staying credited to whoever last wrote them.
func TestUntrackedMutationMarksOnlyTheChangedLines(t *testing.T) {
	path := "/repo/a.go"
	e1 := edit("e1", 1, path, nil, []string{"one", "two", "three"}, true)
	// Something outside docket rewrote line 2 before this edit ran.
	e2 := edit("e2", 2, path,
		[]string{"one", "MUTATED", "three"},
		[]string{"one", "MUTATED", "three", "four"}, true)

	tl := Build("/repo", []*transcript.Session{session(e1, e2)})
	ft := tl.EditsFor("a.go")
	if len(ft.Drifts) != 1 {
		t.Fatalf("got %d drifts, want 1", len(ft.Drifts))
	}
	if ft.Drifts[0].Reason != ReasonUntrackedMutation {
		t.Errorf("drift reason = %q", ft.Drifts[0].Reason)
	}
	if ft.Prov[0].EditID != "e1" || ft.Prov[2].EditID != "e1" {
		t.Errorf("surviving lines lost their provenance: %+v", ft.Prov)
	}
	if ft.Prov[1].Known() || ft.Prov[1].Reason != ReasonUntrackedMutation {
		t.Errorf("mutated line should be unknown, got %+v", ft.Prov[1])
	}
	if ft.Prov[3].EditID != "e2" {
		t.Errorf("new line should belong to e2, got %+v", ft.Prov[3])
	}
}

func TestHumanEditIsDistinguishedFromAnUnknownMutation(t *testing.T) {
	path := "/repo/a.go"
	e1 := edit("e1", 1, path, nil, []string{"one"}, true)
	e2 := edit("e2", 2, path, []string{"HUMAN"}, []string{"HUMAN", "two"}, true)
	e2.UserModified = true

	tl := Build("/repo", []*transcript.Session{session(e1, e2)})
	ft := tl.EditsFor("a.go")
	if !ft.HumanEdited {
		t.Error("file should be marked as edited by a human")
	}
	if ft.Drifts[0].Reason != ReasonHumanEdit {
		t.Errorf("drift reason = %q, want %q", ft.Drifts[0].Reason, ReasonHumanEdit)
	}
}

func TestRemovedLinesAreRecordedAsAttempts(t *testing.T) {
	path := "/repo/a.go"
	e1 := edit("e1", 1, path, nil, []string{"keep", "first attempt", "keep2"}, true)
	e2 := edit("e2", 2, path,
		[]string{"keep", "first attempt", "keep2"},
		[]string{"keep", "second attempt", "keep2"}, true)

	tl := Build("/repo", []*transcript.Session{session(e1, e2)})
	ft := tl.EditsFor("a.go")
	if len(ft.Removed) != 1 {
		t.Fatalf("got %d removals, want 1: %+v", len(ft.Removed), ft.Removed)
	}
	r := ft.Removed[0]
	if r.EditID != "e1" || r.ByEditID != "e2" || r.Lines != 1 {
		t.Errorf("removal = %+v", r)
	}
	if len(r.Sample) == 0 || r.Sample[0] != "first attempt" {
		t.Errorf("sample = %+v", r.Sample)
	}
	// The attempt must be anchored where the replacement now sits, so a hunk can
	// find it without scanning the whole file's history.
	anchored := false
	for i, anchors := range ft.Anchors {
		if len(anchors) > 0 && i <= 2 {
			anchored = true
		}
	}
	if !anchored {
		t.Errorf("removal was not anchored near its replacement: %+v", ft.Anchors)
	}
}

// A commit's cutoff must exclude edits that had not happened yet, or a line can
// be credited to an edit that merely wrote the same text later.
func TestCutoffExcludesLaterEdits(t *testing.T) {
	path := "/repo/a.go"
	e1 := edit("e1", 1, path, nil, []string{"one"}, true)
	e2 := edit("e2", 30, path, []string{"one"}, []string{"one", "later"}, true)

	tl := BuildUntil("/repo", []*transcript.Session{session(e1, e2)}, at(2))
	ft := tl.EditsFor("a.go")
	if len(ft.Lines) != 1 {
		t.Errorf("replay should stop at the cutoff, got %v", ft.Lines)
	}
}

func TestEditsOutsideTheRepositoryAreIgnored(t *testing.T) {
	e := edit("e1", 1, "/elsewhere/a.go", nil, []string{"one"}, true)
	tl := Build("/repo", []*transcript.Session{session(e)})
	if len(tl.Files) != 0 {
		t.Errorf("expected no files, got %v", tl.Files)
	}
}
