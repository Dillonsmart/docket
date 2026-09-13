// Package diffx parses unified diffs and compares line slices.
//
// Two jobs, deliberately in one package because they share the line model:
// reading what a commit changed (ParseUnified) and working out how two versions
// of a file line up (Align), which is how a line's provenance is carried
// forward through later edits.
package diffx

import (
	"strconv"
	"strings"
)

// FileDiff is one file's worth of a unified diff.
type FileDiff struct {
	Path    string // new-side path; for deletions, the old-side path
	OldPath string
	Added   bool
	Deleted bool
	Binary  bool
	Hunks   []Hunk
}

// Hunk is one @@ block.
type Hunk struct {
	OldStart, OldLines int
	NewStart, NewLines int
	// AddedLines are the new-side line numbers introduced by this hunk, paired
	// with their text.
	AddedLines []NumberedLine
	// RemovedText is the old-side text the hunk deleted, kept for attempt
	// reconstruction.
	RemovedText []string
}

// NumberedLine is a line with its 1-based number on its own side of the diff.
type NumberedLine struct {
	Num  int
	Text string
}

// NewRange returns the inclusive new-side line range a hunk covers. For a pure
// deletion there are no new-side lines, so the range collapses onto the line
// after which the deletion happened.
func (h Hunk) NewRange() [2]int {
	if h.NewLines == 0 {
		return [2]int{h.NewStart, h.NewStart}
	}
	return [2]int{h.NewStart, h.NewStart + h.NewLines - 1}
}

// ParseUnified parses `git diff` output. It is tolerant: anything it does not
// understand is skipped rather than guessed at.
func ParseUnified(diff string) []FileDiff {
	var files []FileDiff
	var cur *FileDiff
	var hunk *Hunk
	newLine := 0

	flushHunk := func() {
		if cur != nil && hunk != nil {
			cur.Hunks = append(cur.Hunks, *hunk)
			hunk = nil
		}
	}
	flushFile := func() {
		flushHunk()
		if cur != nil {
			files = append(files, *cur)
			cur = nil
		}
	}

	for _, ln := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(ln, "diff --git "):
			flushFile()
			cur = &FileDiff{}
			// diff --git a/<old> b/<new>; paths may contain spaces, so prefer
			// the ---/+++ headers below and use this only as a fallback.
			if a, b, ok := splitGitHeader(strings.TrimPrefix(ln, "diff --git ")); ok {
				cur.OldPath, cur.Path = a, b
			}
		case cur == nil:
			continue
		case strings.HasPrefix(ln, "--- "):
			p := strings.TrimPrefix(ln, "--- ")
			if p == "/dev/null" {
				cur.Added = true
			} else {
				cur.OldPath = stripPrefix(p)
			}
		case strings.HasPrefix(ln, "+++ "):
			p := strings.TrimPrefix(ln, "+++ ")
			if p == "/dev/null" {
				cur.Deleted = true
				cur.Path = cur.OldPath
			} else {
				cur.Path = stripPrefix(p)
			}
		case strings.HasPrefix(ln, "Binary files ") || strings.HasPrefix(ln, "GIT binary patch"):
			cur.Binary = true
		case strings.HasPrefix(ln, "@@"):
			flushHunk()
			h, ok := parseHunkHeader(ln)
			if !ok {
				continue
			}
			hunk = &h
			newLine = h.NewStart
		case hunk == nil:
			continue
		case strings.HasPrefix(ln, "+"):
			hunk.AddedLines = append(hunk.AddedLines, NumberedLine{Num: newLine, Text: ln[1:]})
			newLine++
		case strings.HasPrefix(ln, "-"):
			hunk.RemovedText = append(hunk.RemovedText, ln[1:])
		case strings.HasPrefix(ln, " "):
			newLine++
		case ln == `\ No newline at end of file`:
			continue
		}
	}
	flushFile()
	return files
}

func splitGitHeader(s string) (old, new string, ok bool) {
	// Only unambiguous when there is exactly one " b/" separator.
	i := strings.Index(s, " b/")
	if i < 0 || !strings.HasPrefix(s, "a/") {
		return "", "", false
	}
	return s[2:i], s[i+3:], true
}

func stripPrefix(p string) string {
	// git writes a/<path> and b/<path>; a tab-separated timestamp may follow.
	if i := strings.IndexByte(p, '\t'); i >= 0 {
		p = p[:i]
	}
	if len(p) > 2 && (strings.HasPrefix(p, "a/") || strings.HasPrefix(p, "b/")) {
		return p[2:]
	}
	return p
}

func parseHunkHeader(ln string) (Hunk, bool) {
	// @@ -oldStart,oldLines +newStart,newLines @@ optional section heading
	end := strings.Index(ln[2:], "@@")
	if end < 0 {
		return Hunk{}, false
	}
	body := strings.TrimSpace(ln[2 : 2+end])
	parts := strings.Fields(body)
	if len(parts) < 2 {
		return Hunk{}, false
	}
	oldStart, oldLines, ok1 := parseRange(strings.TrimPrefix(parts[0], "-"))
	newStart, newLines, ok2 := parseRange(strings.TrimPrefix(parts[1], "+"))
	if !ok1 || !ok2 {
		return Hunk{}, false
	}
	return Hunk{OldStart: oldStart, OldLines: oldLines, NewStart: newStart, NewLines: newLines}, true
}

func parseRange(s string) (start, count int, ok bool) {
	count = 1
	if i := strings.IndexByte(s, ','); i >= 0 {
		n, err := strconv.Atoi(s[i+1:])
		if err != nil {
			return 0, 0, false
		}
		count = n
		s = s[:i]
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, 0, false
	}
	// A zero-length range starts "after" line n, which git reports as n.
	if count == 0 {
		return n + 1, 0, true
	}
	return n, count, true
}

// ---------------------------------------------------------------- line diff

// OpKind is the kind of an alignment operation.
type OpKind int

const (
	// Equal means the line is present on both sides.
	Equal OpKind = iota
	// Insert means the line exists only on the b side.
	Insert
	// Delete means the line exists only on the a side.
	Delete
)

// Op is one alignment step. AIdx and BIdx are 0-based indexes into the
// respective slices; the index on the side the line is absent from is -1.
type Op struct {
	Kind OpKind
	AIdx int
	BIdx int
}

// maxMatrix caps the dynamic-programming table. Above it, Align degrades to a
// whole-block replace, which loses precision but never invents a match: a wrong
// alignment would silently attribute a line to the wrong edit.
const maxMatrix = 6_000_000

// Align computes an alignment of a onto b, preferring equality.
func Align(a, b []string) []Op {
	// Trim the common prefix and suffix first; edits are usually local, and this
	// takes almost every real case out of the quadratic path.
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	sa, sb := len(a), len(b)
	for sa > p && sb > p && a[sa-1] == b[sb-1] {
		sa--
		sb--
	}
	ops := make([]Op, 0, len(a)+len(b))
	for i := 0; i < p; i++ {
		ops = append(ops, Op{Equal, i, i})
	}
	mid := alignCore(a[p:sa], b[p:sb], p, p)
	ops = append(ops, mid...)
	for i := sa; i < len(a); i++ {
		ops = append(ops, Op{Equal, i, i - len(a) + len(b)})
	}
	return ops
}

func alignCore(a, b []string, offA, offB int) []Op {
	n, m := len(a), len(b)
	switch {
	case n == 0 && m == 0:
		return nil
	case n == 0:
		ops := make([]Op, 0, m)
		for j := 0; j < m; j++ {
			ops = append(ops, Op{Insert, -1, offB + j})
		}
		return ops
	case m == 0:
		ops := make([]Op, 0, n)
		for i := 0; i < n; i++ {
			ops = append(ops, Op{Delete, offA + i, -1})
		}
		return ops
	case n*m > maxMatrix:
		ops := make([]Op, 0, n+m)
		for i := 0; i < n; i++ {
			ops = append(ops, Op{Delete, offA + i, -1})
		}
		for j := 0; j < m; j++ {
			ops = append(ops, Op{Insert, -1, offB + j})
		}
		return ops
	}

	// Longest common subsequence over lines.
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	ops := make([]Op, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, Op{Equal, offA + i, offB + j})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, Op{Delete, offA + i, -1})
			i++
		default:
			ops = append(ops, Op{Insert, -1, offB + j})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, Op{Delete, offA + i, -1})
	}
	for ; j < m; j++ {
		ops = append(ops, Op{Insert, -1, offB + j})
	}
	return ops
}

// SplitLines splits file content into lines without a trailing empty element,
// so that "a\nb\n" and "a\nb" both yield two lines. Docket compares content,
// never byte offsets, so the final newline carries no information here.
func SplitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}
