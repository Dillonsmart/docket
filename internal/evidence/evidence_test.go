package evidence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Dillonsmart/docket/internal/attribute"
	"github.com/Dillonsmart/docket/internal/cer"
)

func TestLoadIstanbulCoverage(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "src", "a.ts")
	report := map[string]any{
		abs: map[string]any{
			"path": abs,
			"statementMap": map[string]any{
				"0": map[string]any{"start": map[string]int{"line": 1}, "end": map[string]int{"line": 2}},
				"1": map[string]any{"start": map[string]int{"line": 3}, "end": map[string]int{"line": 3}},
			},
			"s": map[string]int{"0": 4, "1": 0},
		},
	}
	data, _ := json.Marshal(report)
	path := filepath.Join(dir, "coverage-final.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	cov, err := Load(dir, []string{path})
	if err != nil || cov == nil {
		t.Fatalf("load: %v", err)
	}
	if hits, ok := cov.Covered("src/a.ts", 2); !ok || hits != 4 {
		t.Errorf("line 2: hits %d reported %v", hits, ok)
	}
	if hits, ok := cov.Covered("src/a.ts", 3); !ok || hits != 0 {
		t.Errorf("line 3 should be reported as uncovered, got %d %v", hits, ok)
	}
	// A line nobody reported on is not the same as an uncovered line.
	if _, ok := cov.Covered("src/a.ts", 99); ok {
		t.Error("line 99 should not be reported at all")
	}
}

func TestLoadLCOV(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lcov.info")
	body := "SF:" + filepath.Join(dir, "pkg/a.go") + "\nDA:10,3\nDA:11,0\nend_of_record\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cov, err := Load(dir, []string{path})
	if err != nil || cov == nil {
		t.Fatalf("load: %v", err)
	}
	if hits, ok := cov.Covered("pkg/a.go", 10); !ok || hits != 3 {
		t.Errorf("line 10 = %d %v", hits, ok)
	}
}

// The density formula is published, so these are its promises rather than
// arbitrary expectations.
func TestDensityRules(t *testing.T) {
	hunk := attribute.Hunk{AddedLines: 10, Attributed: 10, Confidence: attribute.High}
	unknown := attribute.Hunk{AddedLines: 10, Confidence: attribute.None}

	cases := []struct {
		name  string
		hunk  attribute.Hunk
		ev    []cer.Evidence
		trust string
		want  float64
	}{
		{"nothing at all", hunk, nil, cer.TrustCIAttested, 0},
		{"stale coverage counts for nothing", hunk,
			[]cer.Evidence{{Kind: "coverage", CoveredLines: 10, TotalLines: 10, Observed: false}},
			cer.TrustCIAttested, 0},
		{"a test that ran before the edit counts for nothing", hunk,
			[]cer.Evidence{{Kind: "test_execution", Result: "pass", Observed: false}},
			cer.TrustCIAttested, 0},
		{"full fresh coverage alone", hunk,
			[]cer.Evidence{{Kind: "coverage", CoveredLines: 10, TotalLines: 10, Observed: true}},
			cer.TrustCIAttested, 0.5},
		{"coverage and a passing test", hunk,
			[]cer.Evidence{
				{Kind: "coverage", CoveredLines: 10, TotalLines: 10, Observed: true},
				{Kind: "test_execution", Result: "pass", Observed: true}},
			cer.TrustCIAttested, 0.8},
		{"a transitioned test is worth more", hunk,
			[]cer.Evidence{
				{Kind: "coverage", CoveredLines: 10, TotalLines: 10, Observed: true},
				{Kind: "test_execution", Result: "pass", Observed: true, Transitioned: true}},
			cer.TrustCIAttested, 0.85},
		{"type and static checks are capped", hunk,
			[]cer.Evidence{
				{Kind: "coverage", CoveredLines: 10, TotalLines: 10, Observed: true},
				{Kind: "typecheck", Result: "pass", Observed: true},
				{Kind: "static_check", Result: "pass", Observed: true},
				{Kind: "static_check", Result: "pass", Observed: true}},
			cer.TrustCIAttested, 0.6},
		{"a local claim is worth 0.9 of an attested one", hunk,
			[]cer.Evidence{{Kind: "coverage", CoveredLines: 10, TotalLines: 10, Observed: true}},
			cer.TrustLocalClaimed, 0.45},
		{"unknown authorship is capped at 0.5", unknown,
			[]cer.Evidence{
				{Kind: "coverage", CoveredLines: 10, TotalLines: 10, Observed: true},
				{Kind: "test_execution", Result: "pass", Observed: true, Transitioned: true}},
			cer.TrustCIAttested, 0.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Density(c.hunk, c.ev, cer.ContactNone, c.trust)
			if got != c.want {
				t.Errorf("density = %.2f, want %.2f", got, c.want)
			}
		})
	}
}

// Nothing executed means the score cannot climb, whatever else is recorded.
func TestDensityCapsWhenNothingRan(t *testing.T) {
	h := attribute.Hunk{AddedLines: 5, Attributed: 5, Confidence: attribute.High}
	ev := []cer.Evidence{{Kind: "typecheck", Result: "pass", Observed: true}}
	got := Density(h, ev, cer.ContactEdited, cer.TrustCIAttested)
	if got > 0.15 {
		t.Errorf("density = %.2f, want at most 0.15 when no test executed the code", got)
	}
}
