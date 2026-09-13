// Package attribute folds a commit's diff against a replayed timeline and says,
// per hunk, which recorded edit produced the committed lines — or that nothing
// in the transcript accounts for them.
//
// The committed file is the arbiter. A line is attributed only when the same
// text appears at the aligned position in the replay and in the post-image of
// the edit being credited. That is why a docket can be trusted: attribution is
// a content match, and timestamps only order events.
package attribute

import (
	"sort"

	"github.com/Dillonsmart/docket/internal/diffx"
	"github.com/Dillonsmart/docket/internal/timeline"
	"github.com/Dillonsmart/docket/internal/transcript"
)

// Confidence grades an attribution.
type Confidence string

const (
	// High means every attributed line was found verbatim in the crediting
	// edit's post-image.
	High Confidence = "high"
	// Medium means the lines aligned to the replay but at least one could not be
	// found in the crediting edit's recorded output.
	Medium Confidence = "medium"
	// None means nothing in the transcript accounts for the hunk.
	None Confidence = "none"
)

// Contribution is one edit's share of a hunk.
type Contribution struct {
	EditID string
	Lines  int
}

// Hunk is the attribution of a single diff hunk.
type Hunk struct {
	Path     string
	NewRange [2]int
	OldRange [2]int

	AddedLines int
	Attributed int
	Verified   int
	Unknown    int
	Reasons    map[timeline.Reason]int

	Contributions []Contribution
	Primary       *transcript.FileEdit
	Confidence    Confidence

	// Attempts are edits whose lines used to occupy this region and were taken
	// out again before the commit.
	Attempts []timeline.Removal
	// RemovedText is what the commit itself deleted here.
	RemovedText []string
	// HumanEdited is true when the harness recorded the human modifying this
	// file during the session.
	HumanEdited bool
	// NoTimeline is true when the session never edited this file at all, which
	// is a different and much commoner situation than an edit docket failed to
	// follow: generated lockfiles, scaffolded code, another session's work.
	NoTimeline bool
	// DeletionOnly is true for a hunk that only removes lines. It has no added
	// code to carry provenance, so it is counted separately rather than scored
	// as unattributed.
	DeletionOnly bool
}

// File attributes every hunk of one file.
//
// head is the committed content of the file. ft may be nil, which means the
// transcript never touched this file: every line is then honestly unknown.
func File(path string, ft *timeline.FileTimeline, head []string, hunks []diffx.Hunk, edits timeline.EditByID) []Hunk {
	origin, replayIdx := mapHead(ft, head)
	verifier := newVerifier(edits)

	out := make([]Hunk, 0, len(hunks))
	for _, h := range hunks {
		a := Hunk{
			Path: path, NewRange: h.NewRange(),
			OldRange:    [2]int{h.OldStart, h.OldStart + max(h.OldLines-1, 0)},
			Reasons:     map[timeline.Reason]int{},
			RemovedText: h.RemovedText,
		}
		if ft != nil {
			a.HumanEdited = ft.HumanEdited
		} else {
			a.NoTimeline = true
		}
		a.DeletionOnly = len(h.AddedLines) == 0
		byEdit := map[string]int{}
		var order []string
		for _, ln := range h.AddedLines {
			a.AddedLines++
			idx := ln.Num - 1
			var o timeline.Origin
			if idx >= 0 && idx < len(origin) {
				o = origin[idx]
			} else {
				o = timeline.Origin{Reason: timeline.ReasonNotInTimeline}
			}
			if !o.Known() {
				a.Unknown++
				r := o.Reason
				if r == "" {
					r = timeline.ReasonNotInTimeline
				}
				a.Reasons[r]++
				continue
			}
			a.Attributed++
			if _, seen := byEdit[o.EditID]; !seen {
				order = append(order, o.EditID)
			}
			byEdit[o.EditID]++
			if verifier.contains(o.EditID, ln.Text) {
				a.Verified++
			}
		}
		for _, id := range order {
			a.Contributions = append(a.Contributions, Contribution{EditID: id, Lines: byEdit[id]})
		}
		sort.SliceStable(a.Contributions, func(i, j int) bool {
			if a.Contributions[i].Lines != a.Contributions[j].Lines {
				return a.Contributions[i].Lines > a.Contributions[j].Lines
			}
			return a.Contributions[i].EditID < a.Contributions[j].EditID
		})
		if len(a.Contributions) > 0 {
			a.Primary = edits[a.Contributions[0].EditID]
		}
		switch {
		case a.Attributed == 0:
			a.Confidence = None
		case a.Verified == a.Attributed:
			a.Confidence = High
		default:
			a.Confidence = Medium
		}
		a.Attempts = attemptsFor(ft, replayIdx, h)
		out = append(out, a)
	}
	return out
}

// mapHead aligns the committed content against the replay's final content and
// returns, per committed line, its origin and the replay index it came from
// (-1 when the line is not in the replay at all).
func mapHead(ft *timeline.FileTimeline, head []string) ([]timeline.Origin, []int) {
	origin := make([]timeline.Origin, len(head))
	idxs := make([]int, len(head))
	for i := range origin {
		origin[i] = timeline.Origin{Reason: timeline.ReasonNotInTimeline}
		idxs[i] = -1
	}
	if ft == nil {
		return origin, idxs
	}
	for _, op := range diffx.Align(ft.Lines, head) {
		if op.Kind != diffx.Equal {
			continue
		}
		if op.BIdx >= 0 && op.BIdx < len(origin) && op.AIdx >= 0 && op.AIdx < len(ft.Prov) {
			origin[op.BIdx] = ft.Prov[op.AIdx]
			idxs[op.BIdx] = op.AIdx
		}
	}
	return origin, idxs
}

// attemptsFor collects the abandoned edits anchored inside a hunk's region. A
// hunk's region in the replay is bounded by the replay indexes its committed
// lines mapped to; when none of them mapped, there is no region and therefore
// no attempt can be claimed for it.
func attemptsFor(ft *timeline.FileTimeline, replayIdx []int, h diffx.Hunk) []timeline.Removal {
	if ft == nil || len(ft.Removed) == 0 {
		return nil
	}
	lo, hi := -1, -1
	for _, ln := range h.AddedLines {
		i := ln.Num - 1
		if i < 0 || i >= len(replayIdx) || replayIdx[i] < 0 {
			continue
		}
		if lo < 0 || replayIdx[i] < lo {
			lo = replayIdx[i]
		}
		if replayIdx[i] > hi {
			hi = replayIdx[i]
		}
	}
	if lo < 0 {
		// None of this hunk's lines exist in the replay, so there is no region to
		// look in. Reaching for the nearest anchor by line number would attach
		// someone else's abandoned work to this hunk.
		return nil
	}
	seen := map[int]bool{}
	var out []timeline.Removal
	for i := lo; i <= hi+1 && i <= len(ft.Anchors); i++ {
		var anchors []int
		if i == len(ft.Anchors) {
			anchors = ft.TailAnchors
		} else {
			anchors = ft.Anchors[i]
		}
		for _, r := range anchors {
			if r < 0 || r >= len(ft.Removed) || seen[r] {
				continue
			}
			seen[r] = true
			out = append(out, ft.Removed[r])
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// verifier answers "did this edit actually write this line" without rescanning
// an edit's post-image for every line of every hunk.
type verifier struct {
	edits timeline.EditByID
	sets  map[string]map[string]struct{}
}

func newVerifier(edits timeline.EditByID) *verifier {
	return &verifier{edits: edits, sets: map[string]map[string]struct{}{}}
}

func (v *verifier) contains(editID, line string) bool {
	set, ok := v.sets[editID]
	if !ok {
		e := v.edits[editID]
		set = map[string]struct{}{}
		if e != nil {
			for _, l := range e.Post {
				set[l] = struct{}{}
			}
		}
		v.sets[editID] = set
	}
	_, found := set[line]
	return found
}

// Summary aggregates attribution across a commit, which is what the Phase 1
// gate is measured on.
type Summary struct {
	Files        int
	Hunks        int
	AddedLines   int
	Attributed   int
	Verified     int
	Unknown      int
	HunksKnown   int
	HunksUnknown int
	// DeletionHunks removed lines without adding any, so there is no provenance
	// to resolve; they are excluded from the rates.
	DeletionHunks int
	// The InScope counters cover only files the session actually edited. This is
	// the engine's own accuracy: whether replay can follow an edit it recorded.
	// The headline counters above also carry files nothing in the transcript
	// ever touched, where "unknown" is the correct answer rather than a miss.
	InScopeHunks      int
	InScopeKnown      int
	InScopeAdded      int
	InScopeAttributed int
	Reasons           map[timeline.Reason]int
}

// Add folds one file's hunks into the summary.
func (s *Summary) Add(hunks []Hunk) {
	if s.Reasons == nil {
		s.Reasons = map[timeline.Reason]int{}
	}
	if len(hunks) > 0 {
		s.Files++
	}
	for _, h := range hunks {
		if h.DeletionOnly {
			s.DeletionHunks++
			continue
		}
		s.Hunks++
		s.AddedLines += h.AddedLines
		s.Attributed += h.Attributed
		s.Verified += h.Verified
		s.Unknown += h.Unknown
		// A hunk counts as attributed when the majority of its added lines
		// resolve to a recorded edit. Anything less is reported as unknown
		// rather than quietly averaged away.
		known := h.Attributed*2 > h.AddedLines
		if known {
			s.HunksKnown++
		} else {
			s.HunksUnknown++
		}
		if !h.NoTimeline {
			s.InScopeHunks++
			s.InScopeAdded += h.AddedLines
			s.InScopeAttributed += h.Attributed
			if known {
				s.InScopeKnown++
			}
		}
		for r, n := range h.Reasons {
			s.Reasons[r] += n
		}
	}
}

// LineRate is the fraction of added lines attributed to a recorded edit.
func (s Summary) LineRate() float64 {
	if s.AddedLines == 0 {
		return 0
	}
	return float64(s.Attributed) / float64(s.AddedLines)
}

// HunkRate is the fraction of hunks attributed, which is the number the Phase 1
// gate is written against.
func (s Summary) HunkRate() float64 {
	if s.Hunks == 0 {
		return 0
	}
	return float64(s.HunksKnown) / float64(s.Hunks)
}

// InScopeHunkRate is the attribution rate over files the session edited: the
// measure of whether the replay can follow its own recorded edits. This is the
// number the Phase 1 gate is judged on, with HunkRate printed beside it so the
// difference is never hidden.
func (s Summary) InScopeHunkRate() float64 {
	if s.InScopeHunks == 0 {
		return 0
	}
	return float64(s.InScopeKnown) / float64(s.InScopeHunks)
}

// InScopeLineRate is the line-level equivalent.
func (s Summary) InScopeLineRate() float64 {
	if s.InScopeAdded == 0 {
		return 0
	}
	return float64(s.InScopeAttributed) / float64(s.InScopeAdded)
}

// VerifyRate is the fraction of attributed lines found verbatim in the
// crediting edit's output.
func (s Summary) VerifyRate() float64 {
	if s.Attributed == 0 {
		return 0
	}
	return float64(s.Verified) / float64(s.Attributed)
}
