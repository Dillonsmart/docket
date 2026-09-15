package evidence

import (
	"strings"
	"testing"
	"time"

	"github.com/Dillonsmart/docket/internal/attribute"
	"github.com/Dillonsmart/docket/internal/timeline"
	"github.com/Dillonsmart/docket/internal/transcript"
)

func at(n int) time.Time { return time.Date(2026, 9, 14, 10, 0, n, 0, time.UTC) }

func goTest(id string, sec int, cmd, outcome string) *transcript.Command {
	c := &transcript.Command{ID: id, At: at(sec), Command: cmd, Stdout: map[string]string{
		"pass": "ok  \tpkg\t0.1s", "fail": "--- FAIL: TestX\nFAIL",
	}[outcome]}
	transcript.ClassifyCommand(c)
	return c
}

// An observed edit is stamped when its command finished, a check when the
// command started. A check in the same shell call as the write therefore
// sorts before it, and only the call's id says it is evidence.
func TestCheckInTheSameShellCallAsTheWriteCounts(t *testing.T) {
	cmd := goTest("toolu_1", 0, "python3 - <<'EOF'\nedit()\nEOF\ngo test ./pkg/", "pass")
	e := &transcript.FileEdit{ID: "obs:1", Path: "/repo/x.go", At: at(5), Source: transcript.SourceObserved, CommandID: "toolu_1",
		Pre: []string{}, HasPre: true, Post: []string{"a"}, HasPost: true, Created: true}
	s := &transcript.Session{ID: "s1", Commands: []*transcript.Command{cmd}, Edits: []*transcript.FileEdit{e}, Events: []transcript.Event{cmd, e}}
	tl := timeline.BuildWith("/repo", []*transcript.Session{s}, timeline.Options{})
	h := attribute.Hunk{Path: "x.go", Contributions: []attribute.Contribution{{EditID: "obs:1", Lines: 1}}}

	ev, _ := NewCorrelator(tl, nil, time.Time{}).ForHunk(h)
	if len(ev) != 1 {
		t.Fatalf("got %d evidence, want the same-call check: %+v", len(ev), ev)
	}
	if ev[0].Result != "pass" || !ev[0].Observed || !strings.Contains(ev[0].Confidence, "same_command_as_edit") {
		t.Errorf("evidence = %+v", ev[0])
	}
	if ev[0].Ref != "go test ./pkg/" {
		t.Errorf("ref = %q, want the line that ran the check", ev[0].Ref)
	}
	if strings.Contains(ev[0].Confidence, "superseded") {
		t.Errorf("the edit in the same call is not a later edit: %s", ev[0].Confidence)
	}
}

// A failure in one package says nothing about a pass in another.
func TestTransitionNeedsTheSameInvocation(t *testing.T) {
	other := goTest("c1", 0, "go test ./other/", "fail")
	same := goTest("c2", 1, "go test ./pkg/", "fail")
	e := &transcript.FileEdit{ID: "e1", Path: "/repo/x.go", At: at(2), Source: transcript.SourceTranscript,
		Pre: []string{}, HasPre: true, Post: []string{"a"}, HasPost: true, Created: true}
	pass := goTest("c3", 3, "go test ./pkg/", "pass")
	h := attribute.Hunk{Path: "x.go", Contributions: []attribute.Contribution{{EditID: "e1", Lines: 1}}}

	for _, tc := range []struct {
		name   string
		before *transcript.Command
		want   bool
	}{
		{"a different invocation failed", other, false},
		{"the same invocation failed", same, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &transcript.Session{ID: "s1", Commands: []*transcript.Command{tc.before, pass}, Edits: []*transcript.FileEdit{e},
				Events: []transcript.Event{tc.before, e, pass}}
			tl := timeline.BuildWith("/repo", []*transcript.Session{s}, timeline.Options{})
			ev, _ := NewCorrelator(tl, nil, time.Time{}).ForHunk(h)
			if len(ev) != 1 {
				t.Fatalf("got %d evidence: %+v", len(ev), ev)
			}
			if ev[0].Transitioned != tc.want {
				t.Errorf("transitioned = %v, want %v", ev[0].Transitioned, tc.want)
			}
		})
	}
}
