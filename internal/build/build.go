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

	"github.com/Dillonsmart/docket/internal/agents"
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
	tl := timeline.BuildWith(repo.Root, sessions, timeline.Options{
		Cutoff: commitAt,
		Seed:   seedFrom(repo, base),
	})

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
		Confidence: string(h.Confidence),
	}
	ch.HumanContact, ch.ContactBasis = contact(h)

	if h.Primary != nil {
		e := h.Primary
		task := redact.Excerpt(e.Task, 200)
		said, intent := intentOf(e, 200)
		cmd := redact.Excerpt(e.Command, 200)
		rules = append(rules, task.Rules...)
		rules = append(rules, intent.Rules...)
		rules = append(rules, cmd.Rules...)
		ch.Origin = cer.Origin{
			Actor: string(e.Actor), AgentID: e.AgentID, Model: e.Model,
			Session: e.SessionID, Task: task.Text, Tool: e.Tool,
			Source: e.Source, At: stamp(e.At), Intent: intent.Text, IntentSource: said, Command: cmd.Text,
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

	for _, a := range h.Attempts {
		att, attRules := attempt(tl, a)
		rules = append(rules, attRules...)
		ch.Attempts = append(ch.Attempts, att)
	}
	ch.Attempts = mergeAttempts(ch.Attempts)
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
	// Chosen by weight, told in order: the attempts read as the sequence of
	// decisions they were.
	sort.SliceStable(ch.Attempts, func(i, j int) bool { return ch.Attempts[i].At < ch.Attempts[j].At })

	ev, evRules := corr.ForHunk(h)
	rules = append(rules, evRules...)
	ch.Evidence = ev
	ch.Density = evidence.Density(h, ev, ch.HumanContact, trust)
	return ch, redact.Merge(rules)
}

// mergeAttempts folds removals that tell the same story — same edit's account,
// same replacement, same failing check — into one. A rewrite of several
// regions of a file is one decision, and listing it once per region pushed
// the smaller, later decisions off the capped list.
func mergeAttempts(in []cer.Attempt) []cer.Attempt {
	type key struct{ summary, reason, by string }
	idx := map[key]int{}
	var out []cer.Attempt
	for _, a := range in {
		k := key{a.Summary, a.Reason, a.ReplacedBy}
		if i, ok := idx[k]; ok {
			out[i].Lines += a.Lines
			if a.At < out[i].At {
				out[i].At = a.At
			}
			continue
		}
		idx[k] = len(out)
		out = append(out, a)
	}
	return out
}

// intentOf is what the agent said before the edit, or failing that what it
// thought. The harness does not always keep the former, and the latter is
// where the reason for a change of approach usually is — but the record says
// which it was, since one was addressed to the human and the other was not.
func intentOf(e *transcript.FileEdit, limit int) (string, redact.Result) {
	if strings.TrimSpace(e.Intent) != "" {
		return cer.IntentSaid, redact.Excerpt(e.Intent, limit)
	}
	if strings.TrimSpace(e.Reasoning) != "" {
		return cer.IntentThought, redact.Excerpt(e.Reasoning, limit)
	}
	return "", redact.Excerpt("", limit)
}

// contact ranks what can be claimed about a person and these lines: lines
// that differed in a recorded pre-image beat a prompt that was merely in force.
// Every outcome carries a basis, because "no contact" and "no way to tell" are
// different answers.
func contact(h attribute.Hunk) (string, string) {
	if n := h.Reasons[timeline.ReasonHumanEdit]; n > 0 {
		return cer.ContactEdited, fmt.Sprintf("%d of these lines changed under the harness between its edits", n)
	}
	e := h.Primary
	if e == nil {
		if h.HumanEdited {
			return cer.ContactNone, "the file changed under the harness during the session, but not on these lines"
		}
		return cer.ContactNone, ""
	}
	if e.Actor == transcript.ActorHuman {
		return cer.ContactEdited, "docket watched the file change under a command the human ran"
	}
	harness := e.AgentID
	if i := strings.Index(harness, "/"); i >= 0 {
		harness = harness[:i]
	}
	setting := harness + " " + e.GateDetail
	shell := e.Source == transcript.SourceObserved
	switch {
	case e.Gate == transcript.GatePrompted && shell:
		// The prompt showed a shell command, not these lines.
		return cer.ContactNone, "the command that wrote these lines ran under a permission prompt (" + setting + "), which showed the command rather than the diff"
	case e.Gate == transcript.GatePrompted:
		return cer.ContactApproved, "the edit ran under a permission prompt (" + setting + ")"
	case e.Gate == transcript.GateAuto && shell:
		return cer.ContactNone, "the command that wrote these lines ran without a prompt (" + setting + ")"
	case e.Gate == transcript.GateAuto:
		return cer.ContactNone, "the edit was written without a prompt (" + setting + ")"
	case shell:
		return cer.ContactNone, "the edit was made through the shell, and the command could not be matched to the transcript"
	}
	return cer.ContactNone, "the transcript does not record whether a prompt was in force"
}

// attempt describes an edit whose lines were taken out again, and looks for the
// failing check that explains why.
func attempt(tl *timeline.Timeline, r timeline.Removal) (cer.Attempt, []string) {
	orig := tl.Edits[r.EditID]
	remover := tl.Edits[r.ByEditID]
	a := cer.Attempt{Outcome: "abandoned", Lines: r.Lines, At: stamp(r.At)}

	summary := fmt.Sprintf("%d lines written in %s and later removed", r.Lines, r.Path)
	if orig != nil {
		_, intent := intentOf(orig, 160)
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
	if a.Outcome == "superseded" && remover != nil {
		// What the replacing edit said it was doing is the closest thing to a
		// reason when no check failed in between.
		_, by := intentOf(remover, 160)
		a.ReplacedBy = by.Text
		rules = append(rules, by.Rules...)
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
		cand := cer.Candidate{Hint: c.MutationHint, At: stamp(c.At)}
		if line := namingLine(c.Command, h.Path, name); line != "" {
			cand.Command = redact.Excerpt(line, 160).Text
			cand.PathMentioned = true
			mentioned = append(mentioned, cand)
			continue
		}
		cand.Command = redact.Excerpt(c.Command, 160).Text
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

// namingLine returns the line of a command that names the file, or "". In a
// script of many commands the line that matters is the one that touched the
// file, not the first one.
func namingLine(cmd, path, name string) string {
	for _, line := range transcript.CommandLines(cmd) {
		if mentions(line, path) || (name != "" && name != path && mentions(line, name)) {
			return line
		}
	}
	return ""
}

// mentions reports whether a path appears in a line as itself or as the tail
// of an absolute or dot-relative path — so action/README.md is not taken for
// README.md, but /home/me/repo/README.md and ./README.md are.
func mentions(line, path string) bool {
	const boundary = " \t'\"=:(,<>|;&"
	for i := strings.Index(line, path); i >= 0; {
		start := strings.LastIndexAny(line[:i], boundary) + 1
		prefix := line[start:i]
		if prefix == "" || prefix[0] == '/' || prefix[0] == '.' || prefix[0] == '~' {
			return true
		}
		next := strings.Index(line[i+1:], path)
		if next < 0 {
			return false
		}
		i += 1 + next
	}
	return false
}

// LoadSessions gathers every source of evidence about this repository: the agent
// transcripts, and docket's own observations of shell-driven edits. Both the
// record builder and the gate measurement go through here, so they can never
// disagree about what was known.
func LoadSessions(repo *gitx.Repo, override []string) ([]*transcript.Session, []cer.Session, error) {
	var sessions []*transcript.Session
	var info []cer.Session

	if len(override) > 0 {
		// An explicit path is always read as a Claude Code transcript: it is the
		// only agent whose sessions are addressed by file.
		for _, p := range override {
			s, stats, err := transcript.Parse(p)
			if err != nil {
				continue
			}
			sessions = append(sessions, s)
			info = append(info, sessionInfo(s, stats))
		}
	} else {
		discovered, problems := agents.Discover(repo.Root)
		for _, p := range problems {
			// A source docket could not read is not the same as an agent that
			// wrote nothing, and the record should not imply otherwise.
			info = append(info, cer.Session{ID: p.Agent + ":unreadable", Agent: p.Agent, Unreadable: p.Detail})
		}
		for _, f := range discovered {
			s := f.Session
			if len(s.Edits) == 0 && len(s.Commands) == 0 {
				continue
			}
			sessions = append(sessions, s)
			info = append(info, sessionInfo(s, f.Stats))
		}
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
	link(sessions)
	sort.SliceStable(info, func(i, j int) bool { return info[i].ID < info[j].ID })
	return sessions, info, nil
}

// link gives each observed edit the transcript's account of the shell command
// that made it. The collector sees the file change but not the conversation;
// the transcript has the request, the agent's stated intent and the permission
// setting, but not the file. Both record the tool call's id, and that is the
// join. Without it an agent that edits through the shell leaves no reasoning
// in the record at all.
func link(sessions []*transcript.Session) {
	byID := map[string]*transcript.Command{}
	for _, s := range sessions {
		for _, c := range s.Commands {
			if c.ID != "" {
				byID[c.ID] = c
			}
		}
	}
	for _, s := range sessions {
		for _, e := range s.Edits {
			if e.Source != transcript.SourceObserved || e.CommandID == "" {
				continue
			}
			c := byID[e.CommandID]
			if c == nil {
				continue
			}
			e.Task, e.Intent, e.Reasoning = c.Task, c.Intent, c.Reasoning
			e.Gate, e.GateDetail = c.Gate, c.GateDetail
			if e.Model == "" {
				e.Model = c.Model
			}
		}
	}
}

// seedFrom lets the replay start from the file as it was before this change.
//
// Agents that send patches rather than whole files — Codex, opencode — record
// no pre-image, so without this their first edit to a file could not be
// replayed at all.
func seedFrom(repo *gitx.Repo, base string) func(string) ([]string, bool) {
	return func(path string) ([]string, bool) {
		data, ok := repo.FileAt(base, path)
		if !ok {
			return nil, false
		}
		return diffx.SplitLines(string(data)), true
	}
}

func sessionInfo(s *transcript.Session, stats transcript.ParseStats) cer.Session {
	return cer.Session{
		ID: s.ID, Agent: s.Agent, Model: s.Model, Harness: s.Version,
		Started: stamp(s.Started), Ended: stamp(s.Ended),
		Edits: len(s.Edits), Commands: len(s.Commands), Lossy: stats.EditsRecovered,
	}
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
