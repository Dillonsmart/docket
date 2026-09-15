package build

import (
	"testing"

	"github.com/Dillonsmart/docket/internal/cer"
	"github.com/Dillonsmart/docket/internal/transcript"
)

// A thought stands in for narration the harness did not keep, and the record
// says which it was.
func TestIntentFallsBackToReasoningAndSaysSo(t *testing.T) {
	said := &transcript.FileEdit{Intent: "Said this.", Reasoning: "Thought that."}
	if src, x := intentOf(said, 100); src != cer.IntentSaid || x.Text != "Said this." {
		t.Errorf("said: %q %q", src, x.Text)
	}
	thought := &transcript.FileEdit{Reasoning: "Thought that."}
	if src, x := intentOf(thought, 100); src != cer.IntentThought || x.Text != "Thought that." {
		t.Errorf("thought: %q %q", src, x.Text)
	}
	neither := &transcript.FileEdit{}
	if src, x := intentOf(neither, 100); src != "" || x.Text != "" {
		t.Errorf("neither: %q %q", src, x.Text)
	}
}

// One rewrite touching three regions is one decision, not three.
func TestAttemptsTellingTheSameStoryMerge(t *testing.T) {
	in := []cer.Attempt{
		{Summary: "Try A", ReplacedBy: "Try B", Lines: 5, At: "2026-09-14T10:00:02Z"},
		{Summary: "Try A", ReplacedBy: "Try B", Lines: 3, At: "2026-09-14T10:00:01Z"},
		{Summary: "Try B", ReplacedBy: "Try C", Lines: 1, At: "2026-09-14T10:00:03Z"},
		{Summary: "Try A", ReplacedBy: "Try B", Reason: "go test failed", Lines: 2, At: "2026-09-14T10:00:04Z"},
	}
	out := mergeAttempts(in)
	if len(out) != 3 {
		t.Fatalf("got %d attempts, want 3: %+v", len(out), out)
	}
	if out[0].Lines != 8 || out[0].At != "2026-09-14T10:00:01Z" {
		t.Errorf("merged attempt = %+v, want 8 lines from the earliest moment", out[0])
	}
	if out[2].Reason != "go test failed" {
		t.Errorf("an attempt with a failing check is a different story: %+v", out[2])
	}
}
