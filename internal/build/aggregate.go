package build

import (
	"fmt"
	"strings"
	"time"

	"github.com/Dillonsmart/docket/internal/cer"
	"github.com/Dillonsmart/docket/internal/gitx"
	"github.com/Dillonsmart/docket/internal/store"
)

// Aggregate merges the stored records of a commit range into one view.
//
// A pull request is reviewed as a whole, but the evidence was recorded one
// commit at a time, and it was recorded on the machine where the work happened —
// CI has no transcripts to rebuild from. So the review surface reads what was
// stored rather than trying to reconstruct it, and says plainly which commits
// brought no record with them.
type Aggregate struct {
	Record *cer.Record
	// Missing lists commits in the range with no stored record. A reviewer needs
	// this: absent evidence and absent record look identical otherwise.
	Missing []string
	Covered int
}

// AggregateRange builds the view for base..head.
func AggregateRange(repo *gitx.Repo, base, head string) (*Aggregate, error) {
	out, err := repo.Git("rev-list", "--reverse", "--no-merges", base+".."+head)
	if err != nil {
		return nil, err
	}
	revs := strings.Fields(out)
	if len(revs) == 0 {
		return nil, fmt.Errorf("no commits in %s..%s", base, head)
	}

	agg := &Aggregate{Record: &cer.Record{
		DocketVersion: cer.Version, Spec: cer.SpecID, Trust: cer.TrustCIAttested,
		Generator:   cer.Generator{Name: "docket", Version: Version},
		Redaction:   cer.Redaction{Version: "aggregate", Mode: "aggregate of stored records"},
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}}
	if sha, err := repo.RevParse(base); err == nil {
		agg.Record.Base = sha
	}

	seenSessions := map[string]bool{}
	density := 0.0
	for _, rev := range revs {
		rec, _, err := store.ReadByCommit(repo, rev)
		if err != nil {
			agg.Missing = append(agg.Missing, rev)
			continue
		}
		agg.Covered++
		// The aggregate can claim no more than its weakest member.
		if rec.Trust != cer.TrustCIAttested {
			agg.Record.Trust = rec.Trust
		}
		for _, s := range rec.Sessions {
			if !seenSessions[s.ID] {
				seenSessions[s.ID] = true
				agg.Record.Sessions = append(agg.Record.Sessions, s)
			}
		}
		for _, h := range rec.Hunks {
			h.Commit = rec.Commit
			agg.Record.Hunks = append(agg.Record.Hunks, h)
			density += h.Density
			agg.Record.Totals.AddedLines += h.AddedLines
			agg.Record.Totals.AttributedLines += h.Attributed
			agg.Record.Totals.VerifiedLines += h.Verified
			if h.Origin.Actor == cer.ActorUnknown {
				agg.Record.Totals.UnknownHunks++
			}
			if h.Density == 0 {
				agg.Record.Totals.ZeroEvidence++
				agg.Record.Totals.ZeroEvidenceAdd += h.AddedLines
			}
		}
	}
	agg.Record.Totals.Hunks = len(agg.Record.Hunks)
	agg.Record.Totals.Files = countFiles(agg.Record.Hunks)
	if len(agg.Record.Hunks) > 0 {
		agg.Record.Totals.MeanDensity = round2(density / float64(len(agg.Record.Hunks)))
	}
	digest, err := agg.Record.Digest()
	if err != nil {
		return nil, err
	}
	agg.Record.PayloadHash = digest
	return agg, nil
}
