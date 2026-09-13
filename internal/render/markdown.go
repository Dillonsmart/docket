package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Dillonsmart/docket/internal/cer"
)

// Marker lets the GitHub Action find its own previous comment and update it
// rather than posting a new one on every push.
const Marker = "<!-- docket:evidence -->"

// MarkdownOptions configures the review comment.
type MarkdownOptions struct {
	// Title overrides the heading.
	Title string
	// MaxDetailed caps how many hunks get their own section.
	MaxDetailed int
	// Repo and Commit, when set, turn file references into links.
	Repo   string
	Commit string
	// Threshold, when above zero, states the policy the record was checked
	// against, so the comment explains why a check failed.
	Threshold float64
}

// Markdown renders a record as a pull request comment. It reorders the diff by
// evidence: the hunks nothing can vouch for come first, and everything with a
// test behind it is collapsed underneath.
func Markdown(rec *cer.Record, o MarkdownOptions) string {
	if o.MaxDetailed <= 0 {
		o.MaxDetailed = 15
	}
	title := o.Title
	if title == "" {
		title = "Docket — where the evidence is thin"
	}

	var b strings.Builder
	b.WriteString(Marker + "\n")
	fmt.Fprintf(&b, "## %s\n\n", title)

	t := rec.Totals
	if t.Hunks == 0 {
		b.WriteString("No attributable hunks in this change.\n")
		return b.String()
	}

	byTier := map[Tier][]cer.Hunk{}
	for _, h := range rec.Hunks {
		byTier[TierOf(h)] = append(byTier[TierOf(h)], h)
	}
	noEvidenceLines := lines(byTier[NeedsAttention])

	if noEvidenceLines > 0 {
		fmt.Fprintf(&b, "**%d of %d added lines (%.0f%%) have no verification evidence.**",
			noEvidenceLines, t.AddedLines, 100*float64(noEvidenceLines)/float64(max(t.AddedLines, 1)))
		fmt.Fprintf(&b, " They are listed first.\n\n")
	} else {
		fmt.Fprintf(&b, "Every hunk in this change has some verification evidence behind it.\n\n")
	}

	b.WriteString("| | hunks | added lines |\n|---|--:|--:|\n")
	for _, tier := range []Tier{NeedsAttention, Thin, Covered} {
		hs := byTier[tier]
		if len(hs) == 0 {
			continue
		}
		fmt.Fprintf(&b, "| %s %s | %d | %d |\n", icon(tier), tier, len(hs), lines(hs))
	}
	fmt.Fprintf(&b, "\n%d hunks in %d files · %.0f%% of added lines attributed to a recorded edit · mean density %.2f · trust `%s`\n\n",
		t.Hunks, t.Files, 100*float64(t.AttributedLines)/float64(max(t.AddedLines, 1)), t.MeanDensity, rec.Trust)

	if o.Threshold > 0 {
		fmt.Fprintf(&b, "Policy: every hunk must reach density %.2f.\n\n", o.Threshold)
	}

	ordered := Ordered(rec)
	detailed := 0
	var collapsed []cer.Hunk
	b.WriteString("### Look here first\n\n")
	for _, h := range ordered {
		if detailed >= o.MaxDetailed || TierOf(h) == Covered {
			collapsed = append(collapsed, h)
			continue
		}
		b.WriteString(hunkMarkdown(h, o))
		detailed++
	}
	if detailed == 0 {
		b.WriteString("Nothing stands out: every hunk has evidence behind it.\n\n")
	}

	if len(collapsed) > 0 {
		fmt.Fprintf(&b, "<details>\n<summary>%d further hunks, with evidence (%d lines)</summary>\n\n",
			len(collapsed), lines(collapsed))
		for _, h := range collapsed {
			fmt.Fprintf(&b, "- %s — density %.2f, %s\n", fileRef(h, o), h.Density, oneLineOrigin(h))
		}
		b.WriteString("\n</details>\n\n")
	}

	fmt.Fprintf(&b, "<sub>docket %s · record `%s` · %s. Density: coverage of these lines (≤0.5), a check that ran after the edit (0.3, or 0.35 if this change turned it green), type and static checks (≤0.1), recorded human contact (0.1). Nothing executed means at most 0.15; unknown authorship caps at 0.5; a locally-claimed record scores 0.9 of a CI-attested one.</sub>\n",
		rec.Generator.Version, shortDigest(rec.PayloadHash), trustNote(rec.Trust))
	return b.String()
}

func hunkMarkdown(h cer.Hunk, o MarkdownOptions) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#### %s %s — %d lines, density %.2f\n\n", icon(TierOf(h)), fileRef(h, o), h.AddedLines, h.Density)

	if h.Origin.Actor == cer.ActorUnknown {
		b.WriteString("- **origin** unknown — nothing recorded accounts for these lines")
		if h.Unknown != nil && len(h.Unknown.Reasons) > 0 {
			fmt.Fprintf(&b, " (%s)", reasons(h.Unknown.Reasons))
		}
		b.WriteString("\n")
		if h.Unknown != nil {
			for _, c := range h.Unknown.Candidates {
				label := "possible cause"
				if c.PathMentioned {
					label = "a command naming this file"
				}
				fmt.Fprintf(&b, "  - %s: `%s`\n", label, c.Command)
			}
		}
	} else {
		who := h.Origin.AgentID
		if who == "" {
			who = h.Origin.Actor
		}
		fmt.Fprintf(&b, "- **origin** %s via `%s`", who, h.Origin.Tool)
		if h.Origin.Model != "" {
			fmt.Fprintf(&b, ", %s", h.Origin.Model)
		}
		if h.Origin.Source == "observed" {
			b.WriteString(" (docket read the file before and after)")
		}
		b.WriteString("\n")
		if h.Origin.Task != "" {
			fmt.Fprintf(&b, "- **asked for** %s\n", h.Origin.Task)
		}
		if h.Origin.Intent != "" {
			fmt.Fprintf(&b, "- **said it was doing** %s\n", h.Origin.Intent)
		}
	}

	for _, a := range h.Attempts {
		fmt.Fprintf(&b, "- **tried first** %s — %s", a.Summary, a.Outcome)
		if a.Reason != "" {
			fmt.Fprintf(&b, " (%s)", a.Reason)
		}
		b.WriteString("\n")
	}

	if len(h.Evidence) == 0 {
		b.WriteString("- **evidence** none: nothing ran over these lines\n")
	}
	for _, e := range h.Evidence {
		switch e.Kind {
		case "coverage":
			state := fmt.Sprintf("%d of %d lines executed", e.CoveredLines, e.TotalLines)
			if !e.Observed {
				state += " — but the report predates the code, so it is not counted"
			}
			fmt.Fprintf(&b, "- **coverage** %s\n", state)
		default:
			line := fmt.Sprintf("%s — `%s`", e.Result, e.Ref)
			if e.Transitioned {
				line += " (failed before this change)"
			}
			if strings.Contains(e.Confidence, "superseded_by_later_edit") {
				line += " (the file changed again afterwards)"
			}
			fmt.Fprintf(&b, "- **%s** %s\n", strings.ReplaceAll(e.Kind, "_", " "), line)
		}
	}
	if h.HumanContact == cer.ContactNone {
		b.WriteString("- **human contact** none recorded\n")
	} else {
		fmt.Fprintf(&b, "- **human contact** %s\n", h.HumanContact)
	}
	b.WriteString("\n")
	return b.String()
}

func fileRef(h cer.Hunk, o MarkdownOptions) string {
	label := fmt.Sprintf("`%s:%d-%d`", h.File, h.Range[0], h.Range[1])
	if o.Repo == "" || o.Commit == "" {
		return label
	}
	return fmt.Sprintf("[%s](https://github.com/%s/blob/%s/%s#L%d-L%d)",
		label, o.Repo, o.Commit, h.File, h.Range[0], h.Range[1])
}

func oneLineOrigin(h cer.Hunk) string {
	if h.Origin.Actor == cer.ActorUnknown {
		return "origin unknown"
	}
	who := h.Origin.AgentID
	if who == "" {
		who = h.Origin.Actor
	}
	kinds := map[string]bool{}
	for _, e := range h.Evidence {
		kinds[e.Kind] = true
	}
	var ks []string
	for k := range kinds {
		ks = append(ks, strings.ReplaceAll(k, "_", " "))
	}
	sort.Strings(ks)
	if len(ks) == 0 {
		return who
	}
	return who + ", " + strings.Join(ks, " + ")
}

func icon(t Tier) string {
	switch t {
	case NeedsAttention:
		return "🔴"
	case Thin:
		return "🟡"
	default:
		return "🟢"
	}
}

func trustNote(trust string) string {
	if trust == cer.TrustCIAttested {
		return "built and signed in CI"
	}
	return "built on a developer machine, so its claims are self-reported"
}

func lines(hs []cer.Hunk) int {
	n := 0
	for _, h := range hs {
		n += h.AddedLines
	}
	return n
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
