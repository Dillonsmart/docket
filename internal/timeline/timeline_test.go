package timeline

import (
	"strings"
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

// Patch-based agents (Codex, opencode) record no pre-image, so the replay has
// to start from the file as the base revision left it.
func TestSeedLetsAPatchOnlyEditReplay(t *testing.T) {
	base := []string{"one", "two", "three"}
	e := &transcript.FileEdit{
		ID: "p1", Tool: "apply_patch", Path: "/repo/a.go", Sequence: 1, At: at(1),
		Actor: transcript.ActorAgent, SessionID: "s1",
		Patch: []transcript.EditOp{{
			OldText: []string{"two"},
			NewText: []string{"two", "two and a half"},
		}},
	}
	tl := BuildWith("/repo", []*transcript.Session{session(e)}, Options{
		Seed: func(path string) ([]string, bool) { return base, true },
	})
	ft := tl.EditsFor("a.go")
	if ft == nil || len(ft.Lossy) != 0 {
		t.Fatalf("edit could not be replayed: %+v", ft)
	}
	if got := strings.Join(ft.Lines, "|"); got != "one|two|two and a half|three" {
		t.Fatalf("replayed content = %q", got)
	}
	if !ft.Prov[2].Known() || ft.Prov[2].EditID != "p1" {
		t.Errorf("the added line should belong to the patch: %+v", ft.Prov[2])
	}
	if ft.Prov[0].Known() {
		t.Errorf("a line that was already there must not be credited to the edit: %+v", ft.Prov[0])
	}
}

// Without the base content there is nothing to apply a patch to, and inventing
// one would be the worst possible answer.
func TestPatchWithoutSeedIsLossyNotGuessed(t *testing.T) {
	e := &transcript.FileEdit{
		ID: "p1", Tool: "apply_patch", Path: "/repo/a.go", Sequence: 1, At: at(1),
		Actor: transcript.ActorAgent, SessionID: "s1",
		Patch: []transcript.EditOp{{OldText: []string{"two"}, NewText: []string{"TWO"}}},
	}
	tl := BuildWith("/repo", []*transcript.Session{session(e)}, Options{})
	ft := tl.EditsFor("a.go")
	if ft == nil || len(ft.Lossy) != 1 {
		t.Fatalf("expected the edit to be recorded as lossy, got %+v", ft)
	}
}

// An edit written against the base rather than against the replay still counts,
// but re-seeding is a divergence and must be reported as one.
func TestEditAgainstTheBaseReSeedsAndRecordsDrift(t *testing.T) {
	base := []string{"alpha", "beta"}
	first := &transcript.FileEdit{
		ID: "e1", Tool: "Write", Path: "/repo/a.go", Sequence: 1, At: at(1),
		Post: []string{"something", "entirely", "different"}, HasPost: true, HasPre: true,
		Actor: transcript.ActorAgent, SessionID: "s1",
	}
	// This one only makes sense against the base content.
	second := &transcript.FileEdit{
		ID: "e2", Tool: "apply_patch", Path: "/repo/a.go", Sequence: 2, At: at(2),
		Actor: transcript.ActorAgent, SessionID: "s1",
		Patch: []transcript.EditOp{{OldText: []string{"beta"}, NewText: []string{"beta", "gamma"}}},
	}
	tl := BuildWith("/repo", []*transcript.Session{session(first, second)}, Options{
		Seed: func(path string) ([]string, bool) { return base, true },
	})
	ft := tl.EditsFor("a.go")
	if len(ft.Lossy) != 0 {
		t.Fatalf("edit was given up on: %+v", ft.Lossy)
	}
	if got := strings.Join(ft.Lines, "|"); got != "alpha|beta|gamma" {
		t.Fatalf("content = %q", got)
	}
	if len(ft.Drifts) != 1 {
		t.Errorf("re-seeding from the base is a divergence and should be recorded: %+v", ft.Drifts)
	}
	if !ft.Prov[2].Known() || ft.Prov[2].EditID != "e2" {
		t.Errorf("the added line = %+v", ft.Prov[2])
	}
}

// A substring swap is how most edit tools describe themselves; the replay has
// to apply it to whatever the file actually said at the time.
func TestReplacementEditAppliesToTheReplayedContent(t *testing.T) {
	first := &transcript.FileEdit{
		ID: "e1", Tool: "write", Path: "/repo/a.go", Sequence: 1, At: at(1),
		Post: []string{"const a = 1;"}, HasPost: true, HasPre: true, Created: true,
		Actor: transcript.ActorAgent, SessionID: "s1",
	}
	second := &transcript.FileEdit{
		ID: "e2", Tool: "edit", Path: "/repo/a.go", Sequence: 2, At: at(2),
		Replace: &transcript.Replacement{Old: "const a = 1;", New: "const a = 2;"},
		Actor:   transcript.ActorAgent, SessionID: "s1",
	}
	tl := BuildWith("/repo", []*transcript.Session{session(first, second)}, Options{})
	ft := tl.EditsFor("a.go")
	if len(ft.Lines) != 1 || ft.Lines[0] != "const a = 2;" {
		t.Fatalf("content = %v", ft.Lines)
	}
	if ft.Prov[0].EditID != "e2" {
		t.Errorf("the rewritten line belongs to the second edit, got %+v", ft.Prov[0])
	}
}
