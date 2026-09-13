// Package build turns a commit's diff, an agent's transcript and whatever
// checks ran into one Commit Evidence Record.
//
// It is the only place that decides what goes into a docket, and it is written
// to be boring: every field is either read from something recorded or explicitly
// marked unknown.
package build

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Dillonsmart/docket/internal/attribute"
	"github.com/Dillonsmart/docket/internal/cer"
	"github.com/Dillonsmart/docket/internal/collect"
	"github.com/Dillonsmart/docket/internal/diffx"
	"github.com/Dillonsmart/docket/internal/evidence"
	"github.com/Dillonsmart/docket/internal/gitx"
	"github.com/Dillonsmart/docket/internal/redact"
	"github.com/Dillonsmart/docket/internal/timeline"
	"github.com/Dillonsmart/docket/internal/transcript"
)

// Version is the docket build's version, recorded in every record.
var Version = "0.1.0-dev"

// maxAttempts caps how many abandoned approaches are recorded per hunk.
const maxAttempts = 4

// Options configures a build.
type Options struct {
	Repo *gitx.Repo
	// Rev is the commit to describe. Empty means the staged change, which is what
	// the prepare-commit-msg hook builds.
	Rev string
	// Base overrides the revision to diff against.
	Base string
	// Coverage lists coverage reports; empty means discover the usual paths.
	Coverage []string
	// NoCoverage skips coverage entirely.
	NoCoverage bool
	// Transcripts overrides transcript discovery.
	Transcripts []string
	// Signer signs the record. Without one the record is unsigned, which
	// `docket verify` reports rather than tolerating silently.
	Signer *cer.Signer
	// Now fixes the clock, for tests.
	Now time.Time
}

// Result is a built record plus the working data behind it, which the local
// viewer and `docket explain` use to show more than the record stores.
type Result struct {
	Record   *cer.Record
	Hunks    []attribute.Hunk
	Timeline *timeline.Timeline
	Coverage *evidence.Coverage
	Summary  attribute.Summary
	// Staged is true when the record describes the index rather than a commit.
	Staged bool
}

// Build assembles the record.
func Build(o Options) (*Result, error) {
	repo := o.Repo
	staged := o.Rev == ""

	head := o.Rev
	base := o.Base
	commitAt := o.Now
	if commitAt.IsZero() {
		commitAt = time.Now().UTC()
	}
	commitSHA := ""

	if staged {
		if base == "" {
			if repo.HasRev("HEAD") {
				base = "HEAD"
			} else {
				base = gitx.EmptyTree
			}
		}
	} else {
		c, err := repo.Commit(head)
		if err != nil {
			return nil, err
		}
		commitSHA = c.SHA
		if base == "" {
			base, err = repo.BaseOf(c.SHA)
			if err != nil {
				return nil, err
			}
		}
		if t, err := time.Parse(time.RFC3339, c.When); err == nil {
			commitAt = t.UTC()
		}
	}

	diff, err := repo.Diff(gitx.DiffOpts{Base: base, Head: head, Staged: staged})
	if err != nil {
		return nil, err
	}
	files := diffx.ParseUnified(diff)

	sessions, sessionInfo, err := LoadSessions(repo, o.Transcripts)
	if err != nil {
		return nil, err
	}
	tl := timeline.BuildUntil(repo.Root, sessions, commitAt)

	var cov *evidence.Coverage
	if !o.NoCoverage {
		paths := o.Coverage
		if len(paths) == 0 {
			paths = evidence.Discover(repo.Root)
		}
		cov, _ = evidence.Load(repo.Root, paths)
	}
	corr := evidence.NewCorrelator(tl, cov, commitAt)

	trust := cer.TrustLocalClaimed
	if o.Signer != nil {
		trust = o.Signer.Trust()
	}

	rec := &cer.Record{
		DocketVersion: cer.Version,
		Spec:          cer.SpecID,
		Trust:         trust,
		Base:          resolveBase(repo, base),
		Commit:        commitSHA,
		Generator:     cer.Generator{Name: "docket", Version: Version},
		Redaction:     cer.Redaction{Version: redact.Version, Mode: "aggressive"},
		Sessions:      sessionInfo,
		GeneratedAt:   commitAt.UTC().Format(time.RFC3339),
	}

	var rules []string
	var summary attribute.Summary
	var allHunks []attribute.Hunk
	densitySum := 0.0

	for _, fd := range files {
		if fd.Binary || fd.Deleted {
			continue
		}
		content := headContent(repo, head, staged, fd.Path)
		ft := tl.EditsFor(fd.Path)
		hunks := attribute.File(fd.Path, ft, content, fd.Hunks, tl.Edits)
		summary.Add(hunks)
		allHunks = append(allHunks, hunks...)

		for _, h := range hunks {
			if h.DeletionOnly {
				continue
			}
			ch, hunkRules := recordHunk(tl, corr, h, trust, base, commitAt)
			rules = append(rules, hunkRules...)
			densitySum += ch.Density
			rec.Hunks = append(rec.Hunks, ch)
			if ch.Density == 0 {
				rec.Totals.ZeroEvidence++
				rec.Totals.ZeroEvidenceAdd += ch.AddedLines
			}
		}
	}

	rec.Totals.Files = countFiles(rec.Hunks)
	rec.Totals.Hunks = len(rec.Hunks)
	rec.Totals.AddedLines = summary.AddedLines
	rec.Totals.AttributedLines = summary.Attributed
	rec.Totals.VerifiedLines = summary.Verified
	rec.Totals.UnknownHunks = summary.HunksUnknown
	if len(rec.Hunks) > 0 {
		rec.Totals.MeanDensity = round2(densitySum / float64(len(rec.Hunks)))
	}
	rec.Redaction.Rules = redact.Merge(rules)

	// Sort for reproducibility: the same inputs must produce the same bytes, or
	// the trailer written before the commit will not match the record written
	// after it.
	sort.SliceStable(rec.Hunks, func(i, j int) bool {
		if rec.Hunks[i].File != rec.Hunks[j].File {
			return rec.Hunks[i].File < rec.Hunks[j].File
		}
		return rec.Hunks[i].Range[0] < rec.Hunks[j].Range[0]
	})

	if o.Signer != nil {
		if err := o.Signer.Sign(rec); err != nil {
			return nil, err
		}
	} else {
		digest, err := rec.Digest()
		if err != nil {
			return nil, err
		}
		rec.PayloadHash = digest
	}

	return &Result{Record: rec, Hunks: allHunks, Timeline: tl, Coverage: cov, Summary: summary, Staged: staged}, nil
}

func recordHunk(tl *timeline.Timeline, corr *evidence.Correlator, h attribute.Hunk, trust, base string, commitAt time.Time) (cer.Hunk, []string) {
	var rules []string
	ch := cer.Hunk{
		File: h.Path, Range: h.NewRange, AddedLines: h.AddedLines,
		Attributed: h.Attributed, Verified: h.Verified,
		Confidence: string(h.Confidence), HumanContact: cer.ContactNone,
	}

	if h.Primary != nil {
		e := h.Primary
		task := redact.Excerpt(e.Task, 200)
		intent := redact.Excerpt(e.Intent, 200)
		cmd := redact.Excerpt(e.Command, 200)
		rules = append(rules, task.Rules...)
		rules = append(rules, intent.Rules...)
		rules = append(rules, cmd.Rules...)
		ch.Origin = cer.Origin{
			Actor: string(e.Actor), AgentID: e.AgentID, Model: e.Model,
			Session: e.SessionID, Task: task.Text, Tool: e.Tool,
			Source: e.Source, At: stamp(e.At), Intent: intent.Text, Command: cmd.Text,
		}
		if e.Actor == transcript.ActorHuman {
			ch.HumanContact = cer.ContactEdited
		}
	} else {
		ch.Origin = cer.Origin{Actor: cer.ActorUnknown}
		ch.Unknown = &cer.Unknown{Reasons: reasonMap(h.Reasons)}
		ch.Unknown.Candidates = candidates(tl, h, base, commitAt)
	}

	for _, c := range h.Contributions[minInt(1, len(h.Contributions)):] {
		e := tl.Edits[c.EditID]
		if e == nil {
			continue
		}
		task := redact.Excerpt(e.Task, 120)
		rules = append(rules, task.Rules...)
		ch.Contributors = append(ch.Contributors, cer.Contributor{
			AgentID: e.AgentID, Session: e.SessionID, Tool: e.Tool,
			Lines: c.Lines, At: stamp(e.At), Task: task.Text,
		})
	}

	if h.HumanEdited && ch.HumanContact == cer.ContactNone {
		ch.HumanContact = cer.ContactEdited
	}

	for _, a := range h.Attempts {
		att, attRules := attempt(tl, a)
		rules = append(rules, attRules...)
		ch.Attempts = append(ch.Attempts, att)
	}
	// An attempt with a failing check behind it is the one a reviewer wants: it
	// says the obvious fix was tried and what happened to it. Ordinary
	// self-revision is kept but ranked below, and the list is capped so a heavily
	// rewritten hunk does not bury its own evidence.
	sort.SliceStable(ch.Attempts, func(i, j int) bool {
		if (ch.Attempts[i].Reason != "") != (ch.Attempts[j].Reason != "") {
			return ch.Attempts[i].Reason != ""
		}
		return ch.Attempts[i].Lines > ch.Attempts[j].Lines
	})
	if len(ch.Attempts) > maxAttempts {
		ch.Attempts = ch.Attempts[:maxAttempts]
	}

	ev, evRules := corr.ForHunk(h)
	rules = append(rules, evRules...)
	ch.Evidence = ev
	ch.Density = evidence.Density(h, ev, ch.HumanContact, trust)
	return ch, redact.Merge(rules)
}

// attempt describes an edit whose lines were taken out again, and looks for the
// failing check that explains why.
func attempt(tl *timeline.Timeline, r timeline.Removal) (cer.Attempt, []string) {
	orig := tl.Edits[r.EditID]
	remover := tl.Edits[r.ByEditID]
	a := cer.Attempt{Outcome: "abandoned", Lines: r.Lines, At: stamp(r.At)}

	summary := fmt.Sprintf("%d lines written in %s and later removed", r.Lines, r.Path)
	if orig != nil {
		intent := redact.Excerpt(orig.Intent, 160)
		if intent.Text != "" {
			summary = intent.Text
		} else if orig.Command != "" {
			cmd := redact.Excerpt(orig.Command, 120)
			summary = "written by: " + cmd.Text
		}
	}
	if remover != nil && orig != nil && remover.ID != orig.ID {
		a.Outcome = "superseded"
	}
	sum := redact.Excerpt(summary, 200)
	a.Summary = sum.Text

	sample := redact.Excerpt(strings.Join(r.Sample, " ⏎ "), 200)
	a.Sample = sample.Text

	var rules []string
	rules = append(rules, sum.Rules...)
	rules = append(rules, sample.Rules...)

	if orig != nil {
		if reason, reasonRules := failureBetween(tl, orig.At, r.At); reason != "" {
			a.Reason = reason
			rules = append(rules, reasonRules...)
		}
	}
	return a, rules
}

// failureBetween finds a check that failed between an edit and its removal. That
// is the highest-value line in a docket: it says the obvious fix was tried and
// what happened to it.
func failureBetween(tl *timeline.Timeline, from, to time.Time) (string, []string) {
	if from.IsZero() || to.IsZero() {
		return "", nil
	}
	for _, c := range tl.Commands {
		if c.Test == nil || c.Test.Outcome != "fail" {
			continue
		}
		if c.At.Before(from) || c.At.After(to) {
			continue
		}
		ref := redact.Excerpt(c.Command, 140)
		return fmt.Sprintf("%s failed after this change: %s", c.Test.Runner, ref.Text), ref.Rules
	}
	return "", nil
}

// candidates lists mutating commands that could account for an unattributed
// hunk. They are labelled as candidates in the schema and rendered as such,
// because docket did not see them write these lines.
func candidates(tl *timeline.Timeline, h attribute.Hunk, base string, commitAt time.Time) []cer.Candidate {
	name := h.Path
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	var mentioned, others []cer.Candidate
	for _, c := range tl.Commands {
		if !c.MayMutateFiles {
			continue
		}
		if !commitAt.IsZero() && c.At.After(commitAt) {
			continue
		}
		ex := redact.Excerpt(c.Command, 160)
		cand := cer.Candidate{Command: ex.Text, Hint: c.MutationHint, At: stamp(c.At)}
		if strings.Contains(c.Command, h.Path) || (name != "" && strings.Contains(c.Command, name)) {
			cand.PathMentioned = true
			mentioned = append(mentioned, cand)
			continue
		}
		others = append(others, cand)
	}
	out := mentioned
	// Only fall back to commands that do not name the file when nothing else is
	// available, and keep it to the most recent few.
	if len(out) == 0 && len(others) > 0 {
		start := len(others) - 3
		if start < 0 {
			start = 0
		}
		out = others[start:]
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

// LoadSessions gathers every source of evidence about this repository: the agent
// transcripts, and docket's own observations of shell-driven edits. Both the
// record builder and the gate measurement go through here, so they can never
// disagree about what was known.
func LoadSessions(repo *gitx.Repo, override []string) ([]*transcript.Session, []cer.Session, error) {
	paths := override
	if len(paths) == 0 {
		found, err := transcript.Find(repo.Root)
		if err == nil {
			paths = found
		}
	}
	var sessions []*transcript.Session
	var info []cer.Session

	for _, p := range paths {
		s, stats, err := transcript.Parse(p)
		if err != nil {
			continue
		}
		if len(s.Edits) == 0 && len(s.Commands) == 0 {
			continue
		}
		sessions = append(sessions, s)
		info = append(info, cer.Session{
			ID: s.ID, Agent: "claude-code", Model: s.Model, Harness: s.Version,
			Started: stamp(s.Started), Ended: stamp(s.Ended),
			Edits: len(s.Edits), Commands: len(s.Commands), Lossy: stats.EditsRecovered,
		})
	}

	// Collector events: edits docket observed itself, including everything the
	// agent did through the shell.
	if store, err := collect.Open(repo); err == nil {
		observed, err := store.Sessions()
		if err == nil {
			for _, s := range observed {
				if len(s.Edits) == 0 {
					continue
				}
				sessions = append(sessions, s)
				info = append(info, cer.Session{
					ID: s.ID, Agent: "docket-collector", Started: stamp(s.Started), Ended: stamp(s.Ended),
					Observed: len(s.Edits),
				})
			}
		}
	}
	sort.SliceStable(info, func(i, j int) bool { return info[i].ID < info[j].ID })
	return sessions, info, nil
}

func headContent(repo *gitx.Repo, rev string, staged bool, path string) []string {
	if staged {
		if data, ok := repo.FileAt("", path); ok {
			return diffx.SplitLines(string(data))
		}
		if data, ok := repo.WorktreeFile(path); ok {
			return diffx.SplitLines(string(data))
		}
		return nil
	}
	if data, ok := repo.FileAt(rev, path); ok {
		return diffx.SplitLines(string(data))
	}
	return nil
}

func resolveBase(repo *gitx.Repo, base string) string {
	if sha, err := repo.RevParse(base); err == nil {
		return sha
	}
	return base
}

func reasonMap(in map[timeline.Reason]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[string(k)] = v
	}
	return out
}

func countFiles(hunks []cer.Hunk) int {
	seen := map[string]bool{}
	for _, h := range hunks {
		seen[h.File] = true
	}
	return len(seen)
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }
