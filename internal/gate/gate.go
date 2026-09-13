// Package gate measures attribution quality against real history.
//
// The plan makes Phase 1 a hard gate: at least 85% of hunks correctly
// attributed across real multi-hour sessions, with honest unknowns for the rest.
// That number is worthless if it is asserted rather than measured, so this
// package is the measurement, and `docket gate` prints it.
package gate

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Dillonsmart/docket/internal/attribute"
	"github.com/Dillonsmart/docket/internal/build"
	"github.com/Dillonsmart/docket/internal/diffx"
	"github.com/Dillonsmart/docket/internal/gitx"
	"github.com/Dillonsmart/docket/internal/timeline"
	"github.com/Dillonsmart/docket/internal/transcript"
)

// CommitResult is the measurement for one commit.
type CommitResult struct {
	SHA     string   `json:"commit"`
	Subject string   `json:"subject"`
	When    string   `json:"when"`
	Summary Totals   `json:"summary"`
	Skipped string   `json:"skipped,omitempty"`
	Files   []string `json:"files,omitempty"`
}

// Totals is a flattened attribute.Summary for reporting.
type Totals struct {
	Files         int            `json:"files"`
	Hunks         int            `json:"hunks"`
	HunksKnown    int            `json:"hunks_attributed"`
	HunksUnknown  int            `json:"hunks_unknown"`
	AddedLines    int            `json:"added_lines"`
	Attributed    int            `json:"attributed_lines"`
	Verified      int            `json:"verified_lines"`
	DeletionHunks int            `json:"deletion_only_hunks"`
	InScopeHunks  int            `json:"in_scope_hunks"`
	InScopeKnown  int            `json:"in_scope_attributed"`
	HunkRate      float64        `json:"hunk_rate"`
	LineRate      float64        `json:"line_rate"`
	InScopeRate   float64        `json:"in_scope_hunk_rate"`
	InScopeLine   float64        `json:"in_scope_line_rate"`
	VerifyRate    float64        `json:"verify_rate"`
	Reasons       map[string]int `json:"unknown_reasons,omitempty"`
}

func totals(s attribute.Summary) Totals {
	t := Totals{
		Files: s.Files, Hunks: s.Hunks, HunksKnown: s.HunksKnown, HunksUnknown: s.HunksUnknown,
		DeletionHunks: s.DeletionHunks, InScopeHunks: s.InScopeHunks, InScopeKnown: s.InScopeKnown,
		AddedLines: s.AddedLines, Attributed: s.Attributed, Verified: s.Verified,
		HunkRate: s.HunkRate(), LineRate: s.LineRate(), VerifyRate: s.VerifyRate(),
		InScopeRate: s.InScopeHunkRate(), InScopeLine: s.InScopeLineRate(),
		Reasons: map[string]int{},
	}
	for r, n := range s.Reasons {
		t.Reasons[string(r)] = n
	}
	return t
}

// Report is the whole measurement.
type Report struct {
	Repo      string         `json:"repo"`
	Sessions  []SessionInfo  `json:"sessions"`
	Commits   []CommitResult `json:"commits"`
	Overall   Totals         `json:"overall"`
	Threshold float64        `json:"threshold"`
	Passed    bool           `json:"passed"`
}

// SessionInfo describes a transcript that fed the measurement.
type SessionInfo struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Edits    int    `json:"edits"`
	Commands int    `json:"commands"`
	Started  string `json:"started"`
	Ended    string `json:"ended"`
	Lossy    int    `json:"lossy_edits"`
}

// Options configures a run.
type Options struct {
	Revisions  []string // commits to measure
	Threshold  float64
	Transcript []string // explicit transcript paths; discovered when empty
	// SkipEmpty drops commits whose diff contains no attributable hunks (merges,
	// pure deletions, binary-only changes) from the rates rather than scoring
	// them as failures.
	SkipEmpty bool
}

// Run measures attribution for each revision.
func Run(repo *gitx.Repo, o Options) (*Report, error) {
	rep := &Report{Repo: repo.Root, Threshold: o.Threshold}
	sessions, info, err := build.LoadSessions(repo, o.Transcript)
	if err != nil {
		return nil, err
	}
	byID := map[string]*transcript.Session{}
	for _, s := range sessions {
		byID[s.ID] = s
	}
	for _, i := range info {
		s := byID[i.ID]
		si := SessionInfo{ID: i.ID, Edits: i.Edits + i.Observed, Commands: i.Commands,
			Started: i.Started, Ended: i.Ended, Lossy: i.Lossy}
		if s != nil {
			si.Path = s.Path
		}
		rep.Sessions = append(rep.Sessions, si)
	}

	var overall attribute.Summary
	for _, rev := range o.Revisions {
		res, sum, err := measure(repo, sessions, rev)
		if err != nil {
			rep.Commits = append(rep.Commits, CommitResult{SHA: rev, Skipped: err.Error()})
			continue
		}
		if o.SkipEmpty && sum.Hunks == 0 {
			res.Skipped = "no attributable hunks"
			rep.Commits = append(rep.Commits, *res)
			continue
		}
		rep.Commits = append(rep.Commits, *res)
		overall.Files += sum.Files
		overall.Hunks += sum.Hunks
		overall.HunksKnown += sum.HunksKnown
		overall.HunksUnknown += sum.HunksUnknown
		overall.DeletionHunks += sum.DeletionHunks
		overall.InScopeHunks += sum.InScopeHunks
		overall.InScopeKnown += sum.InScopeKnown
		overall.InScopeAdded += sum.InScopeAdded
		overall.InScopeAttributed += sum.InScopeAttributed
		overall.AddedLines += sum.AddedLines
		overall.Attributed += sum.Attributed
		overall.Verified += sum.Verified
		if overall.Reasons == nil {
			overall.Reasons = map[timeline.Reason]int{}
		}
		for r, n := range sum.Reasons {
			overall.Reasons[r] += n
		}
	}
	rep.Overall = totals(overall)
	rep.Passed = rep.Overall.InScopeRate >= o.Threshold
	return rep, nil
}

func measure(repo *gitx.Repo, sessions []*transcript.Session, rev string) (*CommitResult, attribute.Summary, error) {
	c, err := repo.Commit(rev)
	if err != nil {
		return nil, attribute.Summary{}, err
	}
	base, err := repo.BaseOf(c.SHA)
	if err != nil {
		return nil, attribute.Summary{}, err
	}
	diff, err := repo.Diff(gitx.DiffOpts{Base: base, Head: c.SHA})
	if err != nil {
		return nil, attribute.Summary{}, err
	}
	when, _ := time.Parse(time.RFC3339, c.When)
	tl := timeline.BuildUntil(repo.Root, sessions, when)

	var sum attribute.Summary
	res := &CommitResult{SHA: c.SHA, Subject: c.Subject, When: c.When}
	for _, fd := range diffx.ParseUnified(diff) {
		if fd.Binary || fd.Deleted {
			continue
		}
		head := []string{}
		if data, ok := repo.FileAt(c.SHA, fd.Path); ok {
			head = diffx.SplitLines(string(data))
		}
		hunks := attribute.File(fd.Path, tl.EditsFor(fd.Path), head, fd.Hunks, tl.Edits)
		sum.Add(hunks)
		if len(hunks) > 0 {
			res.Files = append(res.Files, fd.Path)
		}
	}
	res.Summary = totals(sum)
	return res, sum, nil
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// Text renders the report for a terminal.
func (r *Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "repository   %s\n", r.Repo)
	fmt.Fprintf(&b, "transcripts  %d\n", len(r.Sessions))
	for _, s := range r.Sessions {
		fmt.Fprintf(&b, "  %s  %3d edits  %4d commands  %s → %s\n", short(s.ID), s.Edits, s.Commands, s.Started, s.Ended)
	}
	fmt.Fprintf(&b, "\n%-10s %-6s %-7s %-6s %-7s %-7s %s\n", "commit", "hunks", "all%", "edited", "in-sc%", "verif%", "subject")
	for _, c := range r.Commits {
		if c.Skipped != "" {
			fmt.Fprintf(&b, "%-10s %-6s %-7s %-6s %-7s %-7s %s (%s)\n", short(c.SHA), "-", "-", "-", "-", "-", c.Subject, c.Skipped)
			continue
		}
		fmt.Fprintf(&b, "%-10s %-6d %-7s %-6d %-7s %-7s %s\n", short(c.SHA), c.Summary.Hunks,
			pct(c.Summary.HunkRate), c.Summary.InScopeHunks, pct(c.Summary.InScopeRate),
			pct(c.Summary.VerifyRate), c.Subject)
	}
	o := r.Overall
	fmt.Fprintf(&b, "\noverall      %d hunks in %d files, %d added lines\n", o.Hunks, o.Files, o.AddedLines)
	fmt.Fprintf(&b, "  hunks attributed        %s (%d of %d, every file in the diff)\n", pct(o.HunkRate), o.HunksKnown, o.Hunks)
	fmt.Fprintf(&b, "  lines attributed        %s (%d of %d)\n", pct(o.LineRate), o.Attributed, o.AddedLines)
	fmt.Fprintf(&b, "  in edited files, hunks  %s (%d of %d)\n", pct(o.InScopeRate), o.InScopeKnown, o.InScopeHunks)
	fmt.Fprintf(&b, "  in edited files, lines  %s\n", pct(o.InScopeLine))
	fmt.Fprintf(&b, "  content-verified        %s of attributed lines\n", pct(o.VerifyRate))
	fmt.Fprintf(&b, "  deletion-only hunks     %d (excluded: no added lines to attribute)\n", o.DeletionHunks)
	if len(o.Reasons) > 0 {
		keys := make([]string, 0, len(o.Reasons))
		for k := range o.Reasons {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return o.Reasons[keys[i]] > o.Reasons[keys[j]] })
		b.WriteString("  unknown lines by reason\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "    %-22s %d\n", k, o.Reasons[k])
		}
	}
	verdict := "FAIL"
	if r.Passed {
		verdict = "PASS"
	}
	fmt.Fprintf(&b, "\ngate %s: %s of hunks in edited files attributed, threshold %s\n", verdict, pct(o.InScopeRate), pct(r.Threshold))
	return b.String()
}

func pct(f float64) string { return fmt.Sprintf("%.1f%%", f*100) }

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
