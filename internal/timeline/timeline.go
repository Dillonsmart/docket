// Package timeline replays recorded edits into a per-file provenance vector:
// one origin per line of the file as the session left it.
//
// This is the component the plan calls the hard spike, and its whole design
// follows from one rule: a wrong attribution is worse than no attribution. So
// every step is verified against recorded content. When the reconstruction and
// the recorded pre-image of the next edit disagree — because a Bash command, an
// external editor or the human changed the file in between — the disagreement
// is detected, the affected lines are marked unknown, and the replay re-seeds
// from the recorded truth instead of carrying a fiction forward.
package timeline

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Dillonsmart/docket/internal/diffx"
	"github.com/Dillonsmart/docket/internal/transcript"
)

// Reason explains why a line's origin is not a known edit.
type Reason string

const (
	// ReasonPreExisting means the line was already in the file when the session
	// first touched it.
	ReasonPreExisting Reason = "pre_existing"
	// ReasonUntrackedMutation means the file changed outside any recorded edit.
	ReasonUntrackedMutation Reason = "untracked_mutation"
	// ReasonHumanEdit means the harness reported the human had modified the file.
	ReasonHumanEdit Reason = "human_edit"
	// ReasonLossyEdit means an edit was recorded but could not be reconstructed.
	ReasonLossyEdit Reason = "lossy_edit"
	// ReasonNotInTimeline means the committed line does not appear anywhere in
	// the replayed file: it was written after the session, or by something the
	// session never saw.
	ReasonNotInTimeline Reason = "not_in_timeline"
)

// Origin is a line's provenance.
type Origin struct {
	EditID string
	Reason Reason // empty when EditID is set
}

// Known reports whether the origin names an edit.
func (o Origin) Known() bool { return o.EditID != "" }

// Drift is a detected disagreement between the replay and recorded reality.
type Drift struct {
	Path       string
	EditID     string // the edit whose pre-image exposed the drift
	At         time.Time
	LinesDirty int
	Reason     Reason
	Hints      []string // mutating commands seen in the window
}

// Removal records that lines attributed to one edit were later taken out. These
// are the raw material for the "attempts" field: an edit whose lines no longer
// exist in the committed file is something the agent tried and moved away from.
type Removal struct {
	Path     string
	EditID   string // whose lines were removed
	ByEditID string // the edit that removed them
	At       time.Time
	Lines    int
	Sample   []string
}

// FileTimeline is the replay state for one file.
type FileTimeline struct {
	Path  string // repository-relative
	Abs   string
	Lines []string
	Prov  []Origin
	// Anchors is parallel to Lines: Anchors[i] holds indexes into Removed for
	// attempts that were taken out immediately before line i. Carrying these
	// forward through later edits is what lets a hunk say "two things were tried
	// here before this" rather than listing every abandoned edit in the file.
	Anchors [][]int
	// TailAnchors are attempts removed from the end of the file.
	TailAnchors []int
	Edits       []*transcript.FileEdit
	Drifts      []Drift
	Removed     []Removal
	// HumanEdited records that the harness saw the human change this file
	// during the session.
	HumanEdited bool
	// Lossy records edits that were recorded but could not be replayed.
	Lossy []string
}

// EditByID indexes every edit the timeline replayed.
type EditByID map[string]*transcript.FileEdit

// Timeline is the replay of a whole working tree.
type Timeline struct {
	Files map[string]*FileTimeline
	Edits EditByID
	// Commands is every command seen, in order, so evidence correlation can ask
	// what ran after a given edit.
	Commands []*transcript.Command
	Sessions []*transcript.Session
}

// Merge orders the events of several sessions into one stream. Sessions can
// overlap in time (two agents, two terminals), and the only ordering that is
// meaningful across them is wall clock.
func Merge(sessions []*transcript.Session) []transcript.Event {
	var all []transcript.Event
	for _, s := range sessions {
		all = append(all, s.Events...)
	}
	// Events with no timestamp inherit the previous event's, per session, so a
	// missing timestamp cannot reorder a session's own sequence.
	last := map[string]time.Time{}
	for _, e := range all {
		switch v := e.(type) {
		case *transcript.FileEdit:
			if v.At.IsZero() {
				v.At = last[v.SessionID]
			} else {
				last[v.SessionID] = v.At
			}
		case *transcript.Command:
			if v.At.IsZero() {
				v.At = last[v.SessionID]
			} else {
				last[v.SessionID] = v.At
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		ti, tj := all[i].When(), all[j].When()
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return all[i].Seq() < all[j].Seq()
	})
	return all
}

// Build replays a stream of events against a repository root.
func Build(root string, sessions []*transcript.Session) *Timeline {
	return BuildUntil(root, sessions, time.Time{})
}

// CutoffGrace widens the cutoff by one second.
//
// Git records commit times to the second, so a commit stamped 14:48:23 was
// actually made somewhere in [14:48:23, 14:48:24). An edit recorded at
// 14:48:23.7 is on the wrong side of a naive comparison and would be dropped,
// even though the commit is its direct consequence.
const CutoffGrace = time.Second

// BuildUntil replays only events at or before cutoff (zero means everything).
//
// The cutoff matters for correctness, not performance: a docket for a commit
// must be built from what was known when that commit was made. Replaying later
// edits could credit a line to an edit that had not happened yet, simply because
// it wrote the same text.
func BuildUntil(root string, sessions []*transcript.Session, cutoff time.Time) *Timeline {
	tl := &Timeline{Files: map[string]*FileTimeline{}, Edits: EditByID{}, Sessions: sessions}
	events := Merge(sessions)
	if !cutoff.IsZero() {
		limit := cutoff.Add(CutoffGrace)
		kept := events[:0]
		for _, ev := range events {
			if t := ev.When(); t.IsZero() || !t.After(limit) {
				kept = append(kept, ev)
			}
		}
		events = kept
	}

	// Mutating commands since the last recorded edit, used to explain drift.
	var openMutations []string

	for _, ev := range events {
		switch e := ev.(type) {
		case *transcript.Command:
			tl.Commands = append(tl.Commands, e)
			if e.MayMutateFiles {
				hint := e.MutationHint
				if e.Actor == transcript.ActorHuman {
					hint = "human:" + hint
				}
				openMutations = append(openMutations, hint)
			}
		case *transcript.FileEdit:
			rel, ok := relTo(root, e.Path)
			if !ok {
				continue // an edit to a file outside this repository
			}
			tl.Edits[e.ID] = e
			ft := tl.Files[rel]
			if ft == nil {
				ft = &FileTimeline{Path: rel, Abs: e.Path}
				tl.Files[rel] = ft
			}
			ft.Edits = append(ft.Edits, e)
			if e.UserModified {
				ft.HumanEdited = true
			}
			applyEdit(ft, e, openMutations)
			openMutations = nil
		}
	}
	return tl
}

func applyEdit(ft *FileTimeline, e *transcript.FileEdit, mutations []string) {
	seeded := len(ft.Edits) == 1

	// Step 1: reconcile the replay with the pre-image this edit actually saw.
	if e.HasPre {
		pre := e.Pre
		if seeded {
			ft.Lines = append([]string(nil), pre...)
			ft.Prov = make([]Origin, len(pre))
			ft.Anchors = make([][]int, len(pre))
			for i := range ft.Prov {
				ft.Prov[i] = Origin{Reason: ReasonPreExisting}
			}
		} else if !sameLines(ft.Lines, pre) {
			reason := ReasonUntrackedMutation
			switch {
			case e.UserModified:
				reason = ReasonHumanEdit
			case len(mutations) > 0 && strings.HasPrefix(mutations[0], "human:"):
				reason = ReasonHumanEdit
			}
			dirty := reseed(ft, pre, reason)
			ft.Drifts = append(ft.Drifts, Drift{
				Path: ft.Path, EditID: e.ID, At: e.At,
				LinesDirty: dirty, Reason: reason, Hints: dedupe(mutations),
			})
		}
	} else if seeded {
		// No pre-image and nothing replayed yet: the file's prior content is
		// unknowable from the transcript. Start empty and let the post-image
		// alignment mark what it can.
		ft.Lines, ft.Prov, ft.Anchors = nil, nil, nil
	}

	// Step 2: apply the post-image.
	if !e.HasPost {
		ft.Lossy = append(ft.Lossy, e.ID)
		return
	}
	post := e.Post
	ops := diffx.Align(ft.Lines, post)
	newProv := make([]Origin, len(post))
	newAnchors := make([][]int, len(post)+1) // last slot is the file tail

	// Deletions are grouped per (removed-edit, position in the new file) so that
	// one logical rewrite is one attempt rather than one per line.
	type key struct {
		editID string
		anchor int
	}
	groups := map[key][]string{}
	var order []key
	emitted := 0
	for _, op := range ops {
		switch op.Kind {
		case diffx.Equal:
			if op.AIdx < len(ft.Prov) && op.BIdx < len(newProv) {
				newProv[op.BIdx] = ft.Prov[op.AIdx]
			}
			if op.AIdx < len(ft.Anchors) && op.BIdx < len(newAnchors) {
				newAnchors[op.BIdx] = append(newAnchors[op.BIdx], ft.Anchors[op.AIdx]...)
			}
			emitted = op.BIdx + 1
		case diffx.Insert:
			if op.BIdx < len(newProv) {
				newProv[op.BIdx] = Origin{EditID: e.ID}
			}
			emitted = op.BIdx + 1
		case diffx.Delete:
			if op.AIdx >= len(ft.Prov) {
				continue
			}
			// Anchors on a deleted line move to where the gap now sits.
			if op.AIdx < len(ft.Anchors) && len(ft.Anchors[op.AIdx]) > 0 {
				newAnchors[min(emitted, len(newAnchors)-1)] = append(newAnchors[min(emitted, len(newAnchors)-1)], ft.Anchors[op.AIdx]...)
			}
			o := ft.Prov[op.AIdx]
			if !o.Known() {
				continue
			}
			k := key{o.EditID, min(emitted, len(post))}
			if _, seen := groups[k]; !seen {
				order = append(order, k)
			}
			groups[k] = append(groups[k], ft.Lines[op.AIdx])
		}
	}
	for _, k := range order {
		lines := groups[k]
		ft.Removed = append(ft.Removed, Removal{
			Path: ft.Path, EditID: k.editID, ByEditID: e.ID, At: e.At,
			Lines: len(lines), Sample: sample(lines, 6),
		})
		idx := len(ft.Removed) - 1
		slot := k.anchor
		if slot > len(post) {
			slot = len(post)
		}
		newAnchors[slot] = append(newAnchors[slot], idx)
	}

	ft.Lines = append([]string(nil), post...)
	ft.Prov = newProv
	ft.Anchors = newAnchors[:len(post)]
	ft.TailAnchors = dedupeInts(append(ft.TailAnchors, newAnchors[len(post)]...))
	for i := range ft.Anchors {
		ft.Anchors[i] = dedupeInts(ft.Anchors[i])
	}
}

func dedupeInts(in []int) []int {
	if len(in) < 2 {
		return in
	}
	seen := map[int]bool{}
	out := in[:0]
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

// reseed replaces the replayed content with the recorded pre-image, keeping the
// provenance of lines that survived the untracked change and marking the rest
// unknown. Surviving lines keep their attribution because their content is
// unchanged; anything else is no longer explainable and says so.
func reseed(ft *FileTimeline, pre []string, reason Reason) int {
	ops := diffx.Align(ft.Lines, pre)
	prov := make([]Origin, len(pre))
	anchors := make([][]int, len(pre))
	dirty := 0
	for _, op := range ops {
		switch op.Kind {
		case diffx.Equal:
			if op.AIdx < len(ft.Prov) && op.BIdx < len(prov) {
				prov[op.BIdx] = ft.Prov[op.AIdx]
			}
			if op.AIdx < len(ft.Anchors) && op.BIdx < len(anchors) {
				anchors[op.BIdx] = ft.Anchors[op.AIdx]
			}
		case diffx.Insert:
			if op.BIdx < len(prov) {
				prov[op.BIdx] = Origin{Reason: reason}
				dirty++
			}
		}
	}
	ft.Lines = append([]string(nil), pre...)
	ft.Prov = prov
	ft.Anchors = anchors
	return dirty
}

func sameLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sample(lines []string, n int) []string {
	out := make([]string, 0, n)
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		out = append(out, t)
		if len(out) == n {
			break
		}
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func relTo(root, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// CommandsAfter returns commands that ran after t, in order.
func (tl *Timeline) CommandsAfter(t time.Time) []*transcript.Command {
	var out []*transcript.Command
	for _, c := range tl.Commands {
		if c.At.After(t) || c.At.Equal(t) {
			out = append(out, c)
		}
	}
	return out
}

// EditsFor returns the replay for a path, or nil.
func (tl *Timeline) EditsFor(path string) *FileTimeline { return tl.Files[path] }
