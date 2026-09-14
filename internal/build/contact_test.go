package build

import (
	"strings"
	"testing"

	"github.com/Dillonsmart/docket/internal/attribute"
	"github.com/Dillonsmart/docket/internal/cer"
	"github.com/Dillonsmart/docket/internal/timeline"
	"github.com/Dillonsmart/docket/internal/transcript"
)

func TestContactIsEarnedAndExplained(t *testing.T) {
	edit := func(gate, detail string) *transcript.FileEdit {
		return &transcript.FileEdit{Actor: transcript.ActorAgent, AgentID: "claude-code/main",
			Source: transcript.SourceTranscript, Gate: gate, GateDetail: detail}
	}
	cases := []struct {
		name    string
		hunk    attribute.Hunk
		contact string
		basis   string
	}{
		{
			name:    "lines that changed under the harness",
			hunk:    attribute.Hunk{Reasons: map[timeline.Reason]int{timeline.ReasonHumanEdit: 3}, Primary: edit(transcript.GateAuto, "acceptEdits")},
			contact: cer.ContactEdited,
			basis:   "3 of these lines",
		},
		{
			name:    "the file changed under the harness, but not here",
			hunk:    attribute.Hunk{Reasons: map[timeline.Reason]int{}, HumanEdited: true},
			contact: cer.ContactNone,
			basis:   "not on these lines",
		},
		{
			name:    "an edit through a permission prompt",
			hunk:    attribute.Hunk{Reasons: map[timeline.Reason]int{}, Primary: edit(transcript.GatePrompted, "default")},
			contact: cer.ContactApproved,
			basis:   "permission prompt (claude-code default)",
		},
		{
			name:    "an edit the harness wrote without asking",
			hunk:    attribute.Hunk{Reasons: map[timeline.Reason]int{}, Primary: edit(transcript.GateAuto, "acceptEdits")},
			contact: cer.ContactNone,
			basis:   "without a prompt (claude-code acceptEdits)",
		},
		{
			name:    "a transcript that never said",
			hunk:    attribute.Hunk{Reasons: map[timeline.Reason]int{}, Primary: edit("", "")},
			contact: cer.ContactNone,
			basis:   "does not record whether a prompt was in force",
		},
		{
			name: "an edit docket watched happen through the shell",
			hunk: attribute.Hunk{Reasons: map[timeline.Reason]int{}, Primary: &transcript.FileEdit{
				Actor: transcript.ActorAgent, AgentID: "claude-code/main", Source: transcript.SourceObserved}},
			contact: cer.ContactNone,
			basis:   "through the shell",
		},
		{
			name: "a change docket watched the human make",
			hunk: attribute.Hunk{Reasons: map[timeline.Reason]int{}, Primary: &transcript.FileEdit{
				Actor: transcript.ActorHuman, Source: transcript.SourceObserved}},
			contact: cer.ContactEdited,
			basis:   "command the human ran",
		},
		{
			name:    "nothing at all",
			hunk:    attribute.Hunk{Reasons: map[timeline.Reason]int{}},
			contact: cer.ContactNone,
			basis:   "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			contact, basis := contact(c.hunk)
			if contact != c.contact {
				t.Errorf("contact = %q, want %q", contact, c.contact)
			}
			if !strings.Contains(basis, c.basis) {
				t.Errorf("basis = %q, want it to mention %q", basis, c.basis)
			}
		})
	}
}

// The old behaviour: one hand-edited line marked every hunk in the file.
func TestFileLevelHumanEditDoesNotClaimUntouchedHunks(t *testing.T) {
	h := attribute.Hunk{Reasons: map[timeline.Reason]int{}, HumanEdited: true,
		Primary: &transcript.FileEdit{Actor: transcript.ActorAgent, AgentID: "claude-code/main",
			Source: transcript.SourceTranscript, Gate: transcript.GateAuto, GateDetail: "auto"}}
	if got, _ := contact(h); got != cer.ContactNone {
		t.Errorf("contact = %q, want none: the file changed, these lines did not", got)
	}
}
