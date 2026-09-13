// Package evidence correlates verification signals with attributed hunks: which
// checks ran after the code was written, which of them executed these exact
// lines, and what that adds up to.
//
// The plan warns that any evidence metric gets gamed the moment it becomes a
// KPI, the way coverage did. Two design choices here answer that. Coverage is
// only counted when the report is newer than the code it claims to cover, and
// execution evidence is only counted when a check actually ran after the edit —
// so a stale report or a green suite that never touched the change buys nothing.
package evidence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Coverage is line execution data for a working tree.
type Coverage struct {
	// Files maps a repository-relative path to line numbers and hit counts. A
	// line absent from the map was not reported at all, which is different from a
	// line reported with zero hits.
	Files map[string]map[int]int
	// Sources lists the reports that were loaded.
	Sources []string
	// Newest is the modification time of the newest report, used to reject
	// coverage that predates the code it would otherwise appear to cover.
	Newest time.Time
}

// Covered reports the hit count for a line and whether the line was in the
// report at all.
func (c *Coverage) Covered(path string, line int) (hits int, reported bool) {
	if c == nil {
		return 0, false
	}
	lines, ok := c.Files[path]
	if !ok {
		return 0, false
	}
	h, ok := lines[line]
	return h, ok
}

// Discover looks for coverage reports in the conventional places. It does not
// run anything: docket reports on evidence that exists, it does not manufacture
// it.
func Discover(root string) []string {
	candidates := []string{
		"coverage/coverage-final.json",
		"coverage/lcov.info",
		"coverage/coverage.lcov",
		".nyc_output/coverage-final.json",
		"coverage.lcov",
		"lcov.info",
	}
	var found []string
	for _, c := range candidates {
		p := filepath.Join(root, c)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			found = append(found, p)
		}
	}
	return found
}

// Load reads coverage reports. Unrecognised files are skipped rather than
// guessed at.
func Load(root string, paths []string) (*Coverage, error) {
	cov := &Coverage{Files: map[string]map[int]int{}}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		st, err := os.Stat(p)
		if err == nil && st.ModTime().After(cov.Newest) {
			cov.Newest = st.ModTime()
		}
		switch {
		case strings.HasSuffix(p, ".json"):
			if err := cov.loadIstanbul(root, data); err != nil {
				continue
			}
		default:
			cov.loadLCOV(root, string(data))
		}
		cov.Sources = append(cov.Sources, p)
	}
	if len(cov.Sources) == 0 {
		return nil, nil
	}
	sort.Strings(cov.Sources)
	return cov, nil
}

type istanbulFile struct {
	Path         string `json:"path"`
	StatementMap map[string]struct {
		Start struct{ Line int } `json:"start"`
		End   struct{ Line int } `json:"end"`
	} `json:"statementMap"`
	S map[string]int `json:"s"`
	// Istanbul also reports branch and function maps; statements are the right
	// granularity for line attribution and the only one every tool agrees on.
}

// loadIstanbul reads the coverage-final.json format emitted by c8, nyc, vitest
// and jest — which is what "V8 coverage" reaches a repository as.
func (c *Coverage) loadIstanbul(root string, data []byte) error {
	var files map[string]istanbulFile
	if err := json.Unmarshal(data, &files); err != nil {
		return err
	}
	for key, f := range files {
		path := f.Path
		if path == "" {
			path = key
		}
		rel := relative(root, path)
		lines := c.Files[rel]
		if lines == nil {
			lines = map[int]int{}
			c.Files[rel] = lines
		}
		for id, stmt := range f.StatementMap {
			hits := f.S[id]
			for ln := stmt.Start.Line; ln <= stmt.End.Line && ln > 0; ln++ {
				if existing, ok := lines[ln]; !ok || hits > existing {
					lines[ln] = hits
				}
			}
		}
	}
	return nil
}

// loadLCOV reads the lcov tracefile format, which is how Go, PHP, Python and
// Rust coverage most often arrives.
func (c *Coverage) loadLCOV(root, text string) {
	var cur map[int]int
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(ln, "SF:"):
			rel := relative(root, strings.TrimPrefix(ln, "SF:"))
			cur = c.Files[rel]
			if cur == nil {
				cur = map[int]int{}
				c.Files[rel] = cur
			}
		case strings.HasPrefix(ln, "DA:") && cur != nil:
			parts := strings.Split(strings.TrimPrefix(ln, "DA:"), ",")
			if len(parts) < 2 {
				continue
			}
			line, err1 := strconv.Atoi(parts[0])
			hits, err2 := strconv.Atoi(parts[1])
			if err1 != nil || err2 != nil {
				continue
			}
			if existing, ok := cur[line]; !ok || hits > existing {
				cur[line] = hits
			}
		case ln == "end_of_record":
			cur = nil
		}
	}
}

func relative(root, path string) string {
	if !filepath.IsAbs(path) {
		return filepath.ToSlash(filepath.Clean(path))
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}
