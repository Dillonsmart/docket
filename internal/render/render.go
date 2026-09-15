// Package render presents a docket to a human.
//
// Both renderers here answer the same question in different places: where
// should a reviewer look first? The record is sorted by how little is known
// about each hunk, not by file order, because file order is exactly what makes a
// 900-line agent diff unreviewable.
package render

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Dillonsmart/docket/internal/cer"
)

// Tier groups hunks by how much is known about them.
type Tier int

const (
	// NeedsAttention is a hunk with effectively no verification evidence.
	NeedsAttention Tier = iota
	// Thin has some evidence but not much.
	Thin
	// Covered is well backed.
	Covered
)

// Thresholds for the tiers. They are round numbers on purpose: a reviewer has to
// be able to hold the rule in their head.
const (
	thinAbove    = 0.15
	coveredAbove = 0.6
)

// TierOf classifies a hunk.
func TierOf(h cer.Hunk) Tier {
	switch {
	case h.Density <= thinAbove:
		return NeedsAttention
	case h.Density < coveredAbove:
		return Thin
	default:
		return Covered
	}
}

func (t Tier) String() string {
	switch t {
	case NeedsAttention:
		return "no evidence"
	case Thin:
		return "thin evidence"
	default:
		return "covered"
	}
}

// Risk orders hunks within the report. Density carries most of it, with unknown
// authorship and untouched-by-a-human counting against a hunk, because those are
// the two things a reviewer cannot recover on their own.
func Risk(h cer.Hunk) float64 {
	r := (1 - h.Density) * 0.7
	if h.Origin.Actor == cer.ActorUnknown {
		r += 0.2
	}
	if h.HumanContact == cer.ContactNone {
		r += 0.1
	}
	// Size breaks ties: 40 unexplained lines matter more than one.
	return r + float64(minInt(h.AddedLines, 200))/2000
}

// Ordered returns the hunks sorted most-risky first.
func Ordered(rec *cer.Record) []cer.Hunk {
	out := append([]cer.Hunk(nil), rec.Hunks...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := Risk(out[i]), Risk(out[j])
		if ri != rj {
			return ri > rj
		}
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Range[0] < out[j].Range[0]
	})
	return out
}

// ---------------------------------------------------------------- terminal

type style struct{ on bool }

func newStyle() style {
	if os.Getenv("NO_COLOR") != "" {
		return style{}
	}
	if os.Getenv("TERM") == "dumb" {
		return style{}
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return style{}
	}
	return style{on: fi.Mode()&os.ModeCharDevice != 0}
}

func (s style) wrap(code, text string) string {
	if !s.on {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s style) bold(t string) string  { return s.wrap("1", t) }
func (s style) dim(t string) string   { return s.wrap("2", t) }
func (s style) red(t string) string   { return s.wrap("31", t) }
func (s style) green(t string) string { return s.wrap("32", t) }
func (s style) amber(t string) string { return s.wrap("33", t) }

func (s style) tier(t Tier) string {
	switch t {
	case NeedsAttention:
		return s.red("no evidence")
	case Thin:
		return s.amber("thin evidence")
	default:
		return s.green("covered")
	}
}

// ShowOptions configures the terminal view.
type ShowOptions struct {
	// All shows every hunk. By default well-covered hunks are summarised, which
	// is the whole point: attention is the scarce resource.
	All bool
	// Limit caps how many hunks are printed in detail.
	Limit int
	// Verify, when set, is appended as a verification line.
	Verify *cer.VerifyResult
}

// Show renders a record for a terminal.
func Show(rec *cer.Record, o ShowOptions) string {
	s := newStyle()
	var b strings.Builder

	commit := rec.Commit
	if commit == "" {
		commit = "(staged, not yet committed)"
	}
	fmt.Fprintf(&b, "%s %s\n", s.bold("docket"), commit)
	fmt.Fprintf(&b, "  record     %s\n", rec.PayloadHash)
	fmt.Fprintf(&b, "  trust      %s%s\n", rec.Trust, signerNote(rec))
	fmt.Fprintf(&b, "  base       %s\n", short(rec.Base))
	if o.Verify != nil {
		fmt.Fprintf(&b, "  verified   %s\n", verifyLine(s, o.Verify))
	}
	for _, sess := range rec.Sessions {
		fmt.Fprintf(&b, "  session    %s %s\n", short(sess.ID), s.dim(sessionNote(sess)))
	}

	t := rec.Totals
	fmt.Fprintf(&b, "\n  %d hunks in %d files, %d added lines\n", t.Hunks, t.Files, t.AddedLines)
	fmt.Fprintf(&b, "  %s\n", attributionLine(s, t))
	fmt.Fprintf(&b, "  mean evidence density %.2f\n", t.MeanDensity)
	if t.ZeroEvidence > 0 {
		fmt.Fprintf(&b, "  %s\n", s.red(fmt.Sprintf("%d hunks (%d lines) have no verification evidence at all",
			t.ZeroEvidence, t.ZeroEvidenceAdd)))
	}

	ordered := Ordered(rec)
	shown, hidden := 0, 0
	limit := o.Limit
	if limit <= 0 {
		limit = len(ordered)
	}
	b.WriteString("\n")
	for _, h := range ordered {
		if !o.All && TierOf(h) == Covered {
			hidden++
			continue
		}
		if shown >= limit {
			hidden++
			continue
		}
		b.WriteString(hunkText(s, h))
		shown++
	}
	if hidden > 0 {
		fmt.Fprintf(&b, "%s\n", s.dim(fmt.Sprintf("  %d further hunks with evidence behind them, hidden. Use --all to see them.", hidden)))
	}
	return b.String()
}

func hunkText(s style, h cer.Hunk) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s  %s  density %.2f\n",
		s.bold(fmt.Sprintf("%s:%d-%d", h.File, h.Range[0], h.Range[1])),
		s.dim(fmt.Sprintf("(%d lines)", h.AddedLines)),
		s.tier(TierOf(h)), h.Density)

	switch h.Origin.Actor {
	case cer.ActorUnknown:
		fmt.Fprintf(&b, "  origin      %s\n", s.red("unknown — nothing recorded accounts for these lines"))
		if h.Unknown != nil {
			if len(h.Unknown.Reasons) > 0 {
				fmt.Fprintf(&b, "              %s\n", s.dim("because: "+reasons(h.Unknown.Reasons)))
			}
			for _, c := range h.Unknown.Candidates {
				label := "possibly"
				if c.PathMentioned {
					label = "names this file"
				}
				fmt.Fprintf(&b, "              %s\n", s.dim(fmt.Sprintf("%s: %s", label, c.Command)))
			}
		}
	default:
		who := h.Origin.AgentID
		if who == "" {
			who = h.Origin.Actor
		}
		fmt.Fprintf(&b, "  origin      %s via %s%s\n", who, h.Origin.Tool, sourceNote(h.Origin.Source))
		if h.Origin.Model != "" {
			fmt.Fprintf(&b, "              %s\n", s.dim(h.Origin.Model))
		}
		if h.Origin.Task != "" {
			fmt.Fprintf(&b, "  task        %s\n", h.Origin.Task)
		}
		if h.Origin.Intent != "" {
			label := "intent      "
			if h.Origin.IntentSource == cer.IntentThought {
				label = "reasoning   " // thought, not said: the harness kept no narration
			}
			fmt.Fprintf(&b, "  %s%s\n", label, s.dim(h.Origin.Intent))
		}
		if h.Origin.Command != "" {
			fmt.Fprintf(&b, "  command     %s\n", s.dim(h.Origin.Command))
		}
	}
	if len(h.Contributors) > 0 {
		var parts []string
		for _, c := range h.Contributors {
			parts = append(parts, fmt.Sprintf("%s (%d lines)", c.AgentID, c.Lines))
		}
		fmt.Fprintf(&b, "  also        %s\n", strings.Join(parts, ", "))
	}
	for _, a := range h.Attempts {
		fmt.Fprintf(&b, "  attempt     %s [%s]\n", a.Summary, a.Outcome)
		if a.Reason != "" {
			fmt.Fprintf(&b, "              %s\n", s.amber(a.Reason))
		}
		if a.ReplacedBy != "" && a.ReplacedBy != h.Origin.Intent && a.ReplacedBy != a.Summary {
			// Already on screen as the intent line, or the same thought carried over two edits.
			fmt.Fprintf(&b, "              replaced by: %s\n", a.ReplacedBy)
		}
	}
	if len(h.Evidence) == 0 {
		fmt.Fprintf(&b, "  evidence    %s\n", s.red("none"))
	}
	for _, e := range h.Evidence {
		fmt.Fprintf(&b, "  evidence    %s\n", evidenceLine(s, e))
	}
	fmt.Fprintf(&b, "  human       %s\n", humanLine(s, h.HumanContact, h.ContactBasis))
	b.WriteString("\n")
	return b.String()
}

func evidenceLine(s style, e cer.Evidence) string {
	switch e.Kind {
	case "coverage":
		txt := fmt.Sprintf("coverage: %d of %d lines executed (%s)", e.CoveredLines, e.TotalLines, e.Ref)
		if !e.Observed {
			return s.dim(txt + " — report predates the code, not counted")
		}
		if e.CoveredLines == 0 {
			return s.red(txt)
		}
		return s.green(txt)
	default:
		txt := fmt.Sprintf("%s: %s (%s)", e.Kind, e.Result, e.Ref)
		if e.Transitioned {
			txt += " — failed before this change"
		}
		if strings.Contains(e.Confidence, "superseded_by_later_edit") {
			txt += " — file changed again afterwards"
		}
		switch e.Result {
		case "pass":
			return s.green(txt)
		case "fail":
			return s.red(txt)
		default:
			return s.amber(txt)
		}
	}
}

// The basis is uncoloured either way: it is the part a reader weighs.
func humanLine(s style, contact, basis string) string {
	var line string
	if contact == cer.ContactNone {
		line = s.amber("no recorded human contact with these lines")
	} else {
		line = s.green(contact)
	}
	if basis != "" {
		line += " — " + basis
	}
	return line
}

func attributionLine(s style, t cer.Totals) string {
	if t.AddedLines == 0 {
		return "no added lines"
	}
	pctAttr := 100 * float64(t.AttributedLines) / float64(t.AddedLines)
	txt := fmt.Sprintf("%.0f%% of added lines attributed to a recorded edit", pctAttr)
	if t.UnknownHunks > 0 {
		txt += fmt.Sprintf(", %d hunks unattributed", t.UnknownHunks)
	}
	return txt
}

func verifyLine(s style, v *cer.VerifyResult) string {
	switch {
	case !v.DigestMatches:
		return s.red("digest does not match the record contents")
	case v.SignaturePresent && v.SignatureValid:
		return s.green("digest and signature check out (" + v.KeyID + ")")
	case v.SignaturePresent:
		return s.red("signature does not verify")
	default:
		return s.amber("digest matches, record is unsigned")
	}
}

func signerNote(rec *cer.Record) string {
	if rec.Signature == nil {
		return " (unsigned)"
	}
	if rec.Signature.Signer == "" {
		return ""
	}
	return " (signed on " + rec.Signature.Signer + ")"
}

func sessionNote(s cer.Session) string {
	parts := []string{}
	if s.Agent != "" {
		parts = append(parts, s.Agent)
	}
	if s.Model != "" {
		parts = append(parts, s.Model)
	}
	if s.Edits > 0 {
		parts = append(parts, fmt.Sprintf("%d reported edits", s.Edits))
	}
	if s.Observed > 0 {
		parts = append(parts, fmt.Sprintf("%d observed edits", s.Observed))
	}
	if s.Commands > 0 {
		parts = append(parts, fmt.Sprintf("%d commands", s.Commands))
	}
	if s.Lossy > 0 {
		parts = append(parts, fmt.Sprintf("%d edits only partly recoverable", s.Lossy))
	}
	return strings.Join(parts, ", ")
}

func sourceNote(src string) string {
	switch src {
	case "observed":
		return " (docket read the file before and after)"
	case "transcript":
		return " (reported by the harness)"
	default:
		return ""
	}
}

func reasons(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] })
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s × %d", strings.ReplaceAll(k, "_", " "), m[k]))
	}
	return strings.Join(parts, ", ")
}

func short(s string) string {
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
