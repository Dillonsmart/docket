package build

import (
	"testing"
	"time"

	"github.com/Dillonsmart/docket/internal/transcript"
)

// An edit the collector observed carries nothing about why it was made; the
// transcript's record of the same shell call does.
func TestLinkGivesObservedEditsTheCommandsAccount(t *testing.T) {
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	agent := &transcript.Session{ID: "s1", Agent: transcript.AgentClaudeCode, Commands: []*transcript.Command{
		{ID: "toolu_1", At: at, Command: "sed -i '' s/a/b/ x.go", Model: "claude-opus-5",
			Task: "rename a to b", Intent: "The old name collides with the package.",
			Gate: transcript.GateAuto, GateDetail: "acceptEdits"},
	}}
	linked := &transcript.FileEdit{ID: "observed:1", Source: transcript.SourceObserved, CommandID: "toolu_1", At: at.Add(time.Second)}
	orphan := &transcript.FileEdit{ID: "observed:2", Source: transcript.SourceObserved, CommandID: "toolu_gone", At: at.Add(time.Second)}
	reported := &transcript.FileEdit{ID: "toolu_1", Source: transcript.SourceTranscript, Intent: "its own"}
	collector := &transcript.Session{ID: "s1", Agent: transcript.AgentCollector, Edits: []*transcript.FileEdit{linked, orphan, reported}}

	link([]*transcript.Session{agent, collector})

	if linked.Task != "rename a to b" || linked.Intent != "The old name collides with the package." ||
		linked.Gate != transcript.GateAuto || linked.GateDetail != "acceptEdits" || linked.Model != "claude-opus-5" {
		t.Errorf("linked edit = %+v", linked)
	}
	if orphan.Task != "" || orphan.Intent != "" || orphan.Gate != "" {
		t.Errorf("an edit whose command is not in any transcript must stay bare: %+v", orphan)
	}
	if reported.Intent != "its own" {
		t.Errorf("a reported edit keeps its own account: %+v", reported)
	}
}
