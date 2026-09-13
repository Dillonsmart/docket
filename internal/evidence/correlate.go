package evidence

import (
	"fmt"
	"time"

	"github.com/Dillonsmart/docket/internal/attribute"
	"github.com/Dillonsmart/docket/internal/cer"
	"github.com/Dillonsmart/docket/internal/redact"
	"github.com/Dillonsmart/docket/internal/timeline"
	"github.com/Dillonsmart/docket/internal/transcript"
)

// maxChecks bounds how many checks are attached to a hunk. Beyond a handful,
// more green ticks say nothing new and the record just gets heavier.
const maxChecks = 4

// Correlator answers, for a hunk, what ran after it was written.
type Correlator struct {
	tl  *timeline.Timeline
	cov *Coverage
	// commitAt bounds the window: a check that ran after the commit was made is
	// not evidence for that commit.
	commitAt time.Time
}

// NewCorrelator builds a correlator for one commit.
func NewCorrelator(tl *timeline.Timeline, cov *Coverage, commitAt time.Time) *Correlator {
	return &Correlator{tl: tl, cov: cov, commitAt: commitAt}
}

// ForHunk returns the evidence attached to a hunk and the rules its redaction
// fired.
func (c *Correlator) ForHunk(h attribute.Hunk) ([]cer.Evidence, []string) {
	var out []cer.Evidence
	var rules []string

	// Coverage first: it is the only signal that names these lines.
	if ev, ok := c.coverageFor(h); ok {
		out = append(out, ev)
	}

	written := c.writtenAt(h)
	lastTouched := c.lastTouched(h.Path)

	checks := 0
	for _, cmd := range c.tl.Commands {
		if cmd.Test == nil || checks >= maxChecks {
			continue
		}
		if cmd.At.IsZero() || (!c.commitAt.IsZero() && cmd.At.After(c.commitAt)) {
			continue
		}
		observed := !written.IsZero() && !cmd.At.Before(written)
		if !observed {
			// A check that ran before the code existed is not evidence about it.
			continue
		}
		ref := redact.Excerpt(cmd.Command, 160)
		rules = append(rules, ref.Rules...)
		ev := cer.Evidence{
			Kind: kindOf(cmd), Ref: ref.Text, Result: cmd.Test.Outcome,
			Observed: true, Confidence: cmd.Test.Confidence,
			At: stamp(cmd.At),
		}
		ev.Transitioned = c.transitioned(cmd, written)
		if !lastTouched.IsZero() && lastTouched.After(cmd.At) {
			// The file changed again after this check, so the committed content is
			// not what was checked. Recorded rather than dropped: a reviewer wants
			// to know the green tick is out of date.
			ev.Confidence = ev.Confidence + ",superseded_by_later_edit"
		}
		out = append(out, ev)
		checks++
	}
	return out, rules
}

func kindOf(cmd *transcript.Command) string {
	switch cmd.Kind() {
	case "typecheck":
		return "typecheck"
	case "static_check":
		return "static_check"
	default:
		return "test_execution"
	}
}

// coverageFor reports how much of the hunk's added range was executed.
func (c *Correlator) coverageFor(h attribute.Hunk) (cer.Evidence, bool) {
	if c.cov == nil {
		return cer.Evidence{}, false
	}
	written := c.writtenAt(h)
	// A coverage report older than the code cannot have executed it. This is the
	// cheapest defence against a metric inflated by a stale artifact.
	stale := !written.IsZero() && c.cov.Newest.Before(written)

	total, covered, reported := 0, 0, 0
	for ln := h.NewRange[0]; ln <= h.NewRange[1]; ln++ {
		total++
		hits, ok := c.cov.Covered(h.Path, ln)
		if !ok {
			continue
		}
		reported++
		if hits > 0 {
			covered++
		}
	}
	if reported == 0 {
		return cer.Evidence{}, false
	}
	ev := cer.Evidence{
		Kind: "coverage", Ref: coverageRef(c.cov), Result: "pass",
		CoveredLines: covered, TotalLines: total, Observed: !stale,
		Confidence: "line_execution", At: stamp(c.cov.Newest),
	}
	if covered == 0 {
		ev.Result = "uncovered"
	}
	if stale {
		ev.Confidence = "line_execution,report_predates_code"
	}
	return ev, true
}

func coverageRef(cov *Coverage) string {
	if len(cov.Sources) == 0 {
		return "coverage"
	}
	if len(cov.Sources) == 1 {
		return cov.Sources[0]
	}
	return fmt.Sprintf("%s (+%d more)", cov.Sources[0], len(cov.Sources)-1)
}

// writtenAt is when the hunk's newest contributing edit happened.
func (c *Correlator) writtenAt(h attribute.Hunk) time.Time {
	var latest time.Time
	for _, contrib := range h.Contributions {
		e := c.tl.Edits[contrib.EditID]
		if e == nil {
			continue
		}
		if e.At.After(latest) {
			latest = e.At
		}
	}
	return latest
}

// lastTouched is when the file was last edited in the session.
func (c *Correlator) lastTouched(path string) time.Time {
	ft := c.tl.EditsFor(path)
	if ft == nil || len(ft.Edits) == 0 {
		return time.Time{}
	}
	return ft.Edits[len(ft.Edits)-1].At
}

// transitioned reports whether the same runner failed before the code was
// written and passes now. A check that was already green proves much less than
// one this change turned green, and the difference is worth recording.
func (c *Correlator) transitioned(cmd *transcript.Command, written time.Time) bool {
	if cmd.Test == nil || cmd.Test.Outcome != "pass" || written.IsZero() {
		return false
	}
	for _, prev := range c.tl.Commands {
		if prev.Test == nil || prev.At.IsZero() || !prev.At.Before(written) {
			continue
		}
		if prev.Test.Runner == cmd.Test.Runner && prev.Test.Outcome == "fail" {
			return true
		}
	}
	return false
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ---------------------------------------------------------------- density

// Density scores how well a hunk is backed by evidence, between 0 and 1.
//
// The formula is published rather than tuned in private, because a number nobody
// can audit is a number nobody should act on. Its shape is deliberate:
//
//   - Coverage of these exact lines is worth at most half the score, and only
//     from a report written after the code.
//   - A passing check that ran after the edit adds 0.3, and 0.35 if this change
//     turned it from failing to passing.
//   - Type and static checks add 0.05 each, capped at 0.1.
//   - Recorded human contact adds 0.1.
//   - If nothing executed the code at all, the score cannot exceed 0.15,
//     whatever else is true.
//   - If nobody can say who wrote it, the score cannot exceed 0.5.
//   - A locally-claimed record scores 0.9 of what a CI-attested one would, so an
//     organisation's aggregate cannot be inflated from developer laptops.
func Density(h attribute.Hunk, ev []cer.Evidence, contact string, trust string) float64 {
	score := 0.0
	executed := false

	for _, e := range ev {
		switch e.Kind {
		case "coverage":
			if !e.Observed || e.TotalLines == 0 {
				continue // stale report
			}
			frac := float64(e.CoveredLines) / float64(e.TotalLines)
			score += 0.5 * frac
			if e.CoveredLines > 0 {
				executed = true
			}
		case "test_execution":
			if e.Result != "pass" || !e.Observed {
				continue
			}
			add := 0.3
			if e.Transitioned {
				add = 0.35
			}
			score += add
			executed = true
		case "typecheck", "static_check":
			if e.Result == "pass" && e.Observed {
				score += 0.05
			}
		}
	}
	// Cap the contribution of type and static checks.
	if extra := checkBonus(ev); extra > 0.1 {
		score -= extra - 0.1
	}

	switch contact {
	case cer.ContactEdited, cer.ContactApproved:
		score += 0.1
	case cer.ContactViewed:
		score += 0.05
	}

	if !executed {
		score = min(score, 0.15)
	}
	if h.Confidence == attribute.None {
		score = min(score, 0.5)
	}
	if trust != cer.TrustCIAttested {
		score *= 0.9
	}
	return round2(clamp(score))
}

func checkBonus(ev []cer.Evidence) float64 {
	total := 0.0
	for _, e := range ev {
		if (e.Kind == "typecheck" || e.Kind == "static_check") && e.Result == "pass" && e.Observed {
			total += 0.05
		}
	}
	return total
}

func clamp(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }
