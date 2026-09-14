package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/Dillonsmart/docket/internal/build"
	"github.com/Dillonsmart/docket/internal/cer"
	"github.com/Dillonsmart/docket/internal/diffx"
	"github.com/Dillonsmart/docket/internal/gate"
	"github.com/Dillonsmart/docket/internal/gitx"
	"github.com/Dillonsmart/docket/internal/render"
	"github.com/Dillonsmart/docket/internal/store"
)

func cmdBuild(env *env, args []string) error {
	fs, dir := newFlags("build", env)
	staged := fs.Bool("staged", false, "describe the staged change instead of a commit")
	base := fs.String("base", "", "revision to diff against (default: the commit's parent)")
	save := fs.Bool("store", false, "store the record on "+cer.Ref)
	unsigned := fs.Bool("unsigned", false, "do not sign the record")
	asJSON := fs.Bool("json", false, "print the record as JSON")
	noCoverage := fs.Bool("no-coverage", false, "ignore coverage reports")
	coverage := fs.String("coverage", "", "comma-separated coverage reports to read")
	rest, err := parse(fs, args)
	if err != nil {
		return exitError{code: 2}
	}
	repo, err := openRepo(*dir)
	if err != nil {
		return err
	}
	rev := ""
	if !*staged {
		rev = firstOr(rest, "HEAD")
	}
	signer, err := signerFor(repo, *unsigned)
	if err != nil {
		return err
	}
	res, err := build.Build(build.Options{
		Repo: repo, Rev: rev, Base: *base, Signer: signer,
		NoCoverage: *noCoverage, Coverage: splitList(*coverage),
	})
	if err != nil {
		return err
	}
	if *save {
		if res.Record.Commit == "" {
			return fmt.Errorf("cannot store a record for a staged change: commit first, or use the hooks")
		}
		if _, err := store.Write(repo, res.Record); err != nil {
			return err
		}
	}
	if *asJSON {
		data, err := res.Record.Canonical()
		if err != nil {
			return err
		}
		env.stdout.Write(data)
		return nil
	}
	fmt.Fprint(env.stdout, render.Show(res.Record, render.ShowOptions{}))
	if *save {
		fmt.Fprintf(env.stdout, "stored on %s as %s\n", cer.Ref, res.Record.PayloadHash)
	}
	return nil
}

func cmdShow(env *env, args []string) error {
	fs, dir := newFlags("show", env)
	all := fs.Bool("all", false, "show every hunk, including well-covered ones")
	asJSON := fs.Bool("json", false, "print the stored record as JSON")
	rebuild := fs.Bool("rebuild", false, "rebuild from the transcript instead of reading the stored record")
	limit := fs.Int("limit", 0, "cap how many hunks are shown in detail")
	rest, err := parse(fs, args)
	if err != nil {
		return exitError{code: 2}
	}
	repo, err := openRepo(*dir)
	if err != nil {
		return err
	}
	rev := firstOr(rest, "HEAD")
	rec, _, err := load(repo, rev, *rebuild)
	if err != nil {
		return err
	}
	if *asJSON {
		data, err := rec.Canonical()
		if err != nil {
			return err
		}
		env.stdout.Write(data)
		return nil
	}
	v, verr := cer.Verify(rec)
	var vp *cer.VerifyResult
	if verr == nil || verr == cer.ErrNoSignature {
		vp = &v
	}
	fmt.Fprint(env.stdout, render.Show(rec, render.ShowOptions{All: *all, Limit: *limit, Verify: vp}))
	return nil
}

// load reads a stored record, or rebuilds one when asked to — or when the commit
// predates docket, which is the common case on first use.
func load(repo *gitx.Repo, rev string, rebuild bool) (*cer.Record, bool, error) {
	if !rebuild {
		rec, _, err := store.ReadByCommit(repo, rev)
		if err == nil {
			return rec, true, nil
		}
	}
	signer, err := signerFor(repo, false)
	if err != nil {
		return nil, false, err
	}
	res, err := build.Build(build.Options{Repo: repo, Rev: rev, Signer: signer})
	if err != nil {
		return nil, false, err
	}
	return res.Record, false, nil
}

var lineRef = regexp.MustCompile(`^(.*?):(\d+)$`)

func cmdExplain(env *env, args []string) error {
	fs, dir := newFlags("explain", env)
	rest, err := parse(fs, args)
	if err != nil {
		return exitError{code: 2}
	}
	if len(rest) != 1 {
		return fail(2, "usage: docket explain <file>:<line>")
	}
	m := lineRef.FindStringSubmatch(rest[0])
	if m == nil {
		return fail(2, "usage: docket explain <file>:<line>")
	}
	path, lineStr := m[1], m[2]
	line, _ := strconv.Atoi(lineStr)
	repo, err := openRepo(*dir)
	if err != nil {
		return err
	}

	// Ask git which commit last touched the line, and which line it was there.
	// Archaeology has to start from the code as it is now, not from a commit the
	// person asking would have to know already.
	commit, origLine, err := blame(repo, path, line)
	if err != nil {
		return err
	}
	rec, stored, err := load(repo, commit, false)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.stdout, "%s:%d was last written by %s\n", path, line, short(commit))
	if !stored {
		fmt.Fprintf(env.stdout, "(no stored docket for that commit; rebuilt from the session transcript)\n")
	}
	fmt.Fprintln(env.stdout)

	for _, h := range rec.Hunks {
		if h.File != path || origLine < h.Range[0] || origLine > h.Range[1] {
			continue
		}
		fmt.Fprint(env.stdout, render.Show(&cer.Record{
			DocketVersion: rec.DocketVersion, Spec: rec.Spec, Trust: rec.Trust,
			Commit: rec.Commit, Base: rec.Base, Generator: rec.Generator,
			Sessions: rec.Sessions, Hunks: []cer.Hunk{h}, Totals: cer.Totals{
				Files: 1, Hunks: 1, AddedLines: h.AddedLines,
				AttributedLines: h.Attributed, VerifiedLines: h.Verified,
			},
			PayloadHash: rec.PayloadHash, Signature: rec.Signature,
		}, render.ShowOptions{All: true}))
		return nil
	}
	fmt.Fprintf(env.stdout, "That commit's docket has no hunk covering %s:%d.\n", path, origLine)
	return nil
}

// blame resolves a line to the commit that last changed it and its line number
// in that commit.
func blame(repo *gitx.Repo, path string, line int) (string, int, error) {
	out, err := repo.Git("blame", "--porcelain", "-L", fmt.Sprintf("%d,%d", line, line), "--", path)
	if err != nil {
		// Git already says why (no such path, line past the end of the file,
		// untracked); a typo in the path is the usual cause and the person
		// needs to see it rather than a generic failure.
		return "", 0, fmt.Errorf("git blame could not resolve %s:%d: %s", path, line, gitReason(err))
	}
	fields := strings.Fields(firstLine(out))
	if len(fields) < 3 {
		return "", 0, fmt.Errorf("unexpected git blame output for %s:%d", path, line)
	}
	orig, err := strconv.Atoi(fields[1])
	if err != nil {
		orig = line
	}
	return fields[0], orig, nil
}

// gitReason strips the "git <args>: exit status N: " prefix gitx puts on a
// failed command, leaving the line git itself printed.
func gitReason(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		return msg[i+2:]
	}
	return msg
}

func cmdReview(env *env, args []string) error {
	fs, dir := newFlags("review", env)
	format := fs.String("format", "text", "text or md")
	staged := fs.Bool("staged", false, "review the staged change")
	failUnder := fs.Float64("fail-under", 0, "exit 3 if any hunk's density is below this")
	out := fs.String("out", "", "write the output to this file as well as stdout")
	repoName := fs.String("repo", "", "owner/name, to link file references")
	rng := fs.String("range", "", "review a commit range as one change, e.g. origin/main..HEAD")
	rebuild := fs.Bool("rebuild", false, "rebuild rather than read the stored record")
	base := fs.String("base", "", "revision to diff against")
	rest, err := parse(fs, args)
	if err != nil {
		return exitError{code: 2}
	}
	repo, err := openRepo(*dir)
	if err != nil {
		return err
	}

	var rec *cer.Record
	var missing []string
	if *rng != "" {
		// A pull request is reviewed as a whole, from the records its commits
		// brought with them: CI has no transcript to rebuild from.
		base, head, ok := strings.Cut(*rng, "..")
		if !ok {
			return fail(2, "--range takes base..head")
		}
		head = strings.TrimPrefix(head, ".")
		if head == "" {
			head = "HEAD"
		}
		agg, err := build.AggregateRange(repo, base, head)
		if err != nil {
			return err
		}
		rec, missing = agg.Record, agg.Missing
	} else if *staged {
		signer, err := signerFor(repo, false)
		if err != nil {
			return err
		}
		res, err := build.Build(build.Options{Repo: repo, Base: *base, Signer: signer})
		if err != nil {
			return err
		}
		rec = res.Record
	} else {
		rev := firstOr(rest, "HEAD")
		if *base != "" {
			signer, err := signerFor(repo, false)
			if err != nil {
				return err
			}
			res, err := build.Build(build.Options{Repo: repo, Rev: rev, Base: *base, Signer: signer})
			if err != nil {
				return err
			}
			rec = res.Record
		} else {
			rec, _, err = load(repo, rev, *rebuild)
			if err != nil {
				return err
			}
		}
	}

	var text string
	switch *format {
	case "md", "markdown":
		text = render.Markdown(rec, render.MarkdownOptions{
			Repo: *repoName, Commit: rec.Commit, Threshold: *failUnder,
		})
	case "text":
		text = render.Show(rec, render.ShowOptions{})
	default:
		return fail(2, "unknown format %q: use text or md", *format)
	}
	if len(missing) > 0 {
		note := fmt.Sprintf("\n%d commits in this range carry no docket: %s\n",
			len(missing), strings.Join(shortAll(missing), " "))
		if *format == "md" || *format == "markdown" {
			note = fmt.Sprintf("\n> **%d commits in this range carry no docket** (`%s`). Their code has no recorded origin or evidence either way.\n",
				len(missing), strings.Join(shortAll(missing), "`, `"))
		}
		text += note
	}
	fmt.Fprint(env.stdout, text)
	if *out != "" {
		if err := os.WriteFile(*out, []byte(text), 0o644); err != nil {
			return err
		}
	}
	if *failUnder > 0 {
		var below []cer.Hunk
		for _, h := range rec.Hunks {
			if h.Density < *failUnder {
				below = append(below, h)
			}
		}
		if len(below) > 0 {
			return fail(3, "%d hunks are below the required evidence density of %.2f", len(below), *failUnder)
		}
	}
	return nil
}

func cmdVerify(env *env, args []string) error {
	fs, dir := newFlags("verify", env)
	rest, err := parse(fs, args)
	if err != nil {
		return exitError{code: 2}
	}
	repo, err := openRepo(*dir)
	if err != nil {
		return err
	}
	sha, err := repo.RevParse(firstOr(rest, "HEAD"))
	if err != nil {
		return err
	}
	rec, digest, err := store.ReadByCommit(repo, sha)
	if err != nil {
		return fail(4, "%v", err)
	}
	res, verr := cer.Verify(rec)

	fmt.Fprintf(env.stdout, "commit        %s\n", sha)
	fmt.Fprintf(env.stdout, "record        %s\n", digest)
	fmt.Fprintf(env.stdout, "digest        %s\n", okNo(res.DigestMatches, res.Recomputed+" matches the record", "recomputed "+res.Recomputed+" but the record says "+res.Stated))
	switch {
	case verr == cer.ErrNoSignature:
		fmt.Fprintf(env.stdout, "signature     absent\n")
	case verr != nil:
		fmt.Fprintf(env.stdout, "signature     unusable: %v\n", verr)
	case res.SignatureValid && !res.DigestMatches:
		// Saying "valid" alone here would read as reassurance, when what it means
		// is that someone signed a digest the record no longer matches.
		fmt.Fprintf(env.stdout, "signature     valid over the stated digest, which no longer matches this record (key %s)\n", res.KeyID)
	default:
		fmt.Fprintf(env.stdout, "signature     %s\n", okNo(res.SignatureValid, "valid, key "+res.KeyID, "does NOT verify against key "+res.KeyID))
	}
	fmt.Fprintf(env.stdout, "trust         %s\n", rec.Trust)

	// A digest that checks out only proves the record is intact. Whether it
	// describes *this* commit is a separate question, and the answer is no
	// whenever a commit has been rebased, cherry-picked or squashed: git does
	// not run prepare-commit-msg for any of those, so the trailer survives onto
	// content it was never built from.
	switch fit, err := describes(repo, sha, rec); {
	case err != nil:
		fmt.Fprintf(env.stdout, "describes     could not check: %v\n", err)
	case fit == "":
		fmt.Fprintf(env.stdout, "describes     ok, the record matches this commit's diff\n")
	default:
		fmt.Fprintf(env.stdout, "describes     NO: %s\n", fit)
	}

	trailer, hasTrailer := repo.Trailer(sha, cer.Trailer)
	switch {
	case !hasTrailer:
		fmt.Fprintf(env.stdout, "binding       no %s trailer on the commit; found through the by-commit index\n", cer.Trailer)
	case trailer == digest:
		fmt.Fprintf(env.stdout, "binding       the commit's trailer names this record\n")
	default:
		fmt.Fprintf(env.stdout, "binding       MISMATCH: the trailer names %s\n", trailer)
	}
	if rec.Commit != "" && rec.Commit != sha {
		fmt.Fprintf(env.stdout, "commit field  MISMATCH: the record names %s\n", rec.Commit)
		return fail(4, "the record does not belong to this commit")
	}
	if !res.DigestMatches || (res.SignaturePresent && !res.SignatureValid) {
		return fail(4, "verification failed")
	}
	return nil
}

// describes reports why a record does not match a commit, or "" when it does.
//
// The check is structural — base, file count, added lines — so it works on a
// machine that has never seen the session the record was built from.
func describes(repo *gitx.Repo, sha string, rec *cer.Record) (string, error) {
	parent, err := repo.BaseOf(sha)
	if err != nil {
		return "", err
	}
	if rec.Base != "" && rec.Base != parent {
		return fmt.Sprintf("the record was built against %s, but this commit's parent is %s",
			short(rec.Base), short(parent)), nil
	}
	diff, err := repo.Diff(gitx.DiffOpts{Base: parent, Head: sha})
	if err != nil {
		return "", err
	}
	files, added := 0, 0
	for _, fd := range diffx.ParseUnified(diff) {
		if fd.Binary || fd.Deleted {
			continue
		}
		counted := false
		for _, h := range fd.Hunks {
			if len(h.AddedLines) == 0 {
				continue // deletion-only hunks carry no provenance either side
			}
			added += len(h.AddedLines)
			counted = true
		}
		if counted {
			files++
		}
	}
	if files != rec.Totals.Files || added != rec.Totals.AddedLines {
		return fmt.Sprintf("this commit adds %d lines across %d files; the record describes %d across %d",
			added, files, rec.Totals.AddedLines, rec.Totals.Files), nil
	}
	return "", nil
}

func cmdGate(env *env, args []string) error {
	fs, dir := newFlags("gate", env)
	commits := fs.Int("commits", 20, "how many commits back to measure")
	since := fs.String("since", "", "measure commits in this range instead, e.g. v1.0..HEAD")
	threshold := fs.Float64("threshold", 0.85, "the attribution rate that counts as passing")
	asJSON := fs.Bool("json", false, "print the measurement as JSON")
	if _, err := parse(fs, args); err != nil {
		return exitError{code: 2}
	}
	repo, err := openRepo(*dir)
	if err != nil {
		return err
	}
	var revs []string
	if *since != "" {
		out, err := repo.Git("log", "--format=%H", "--reverse", "--no-merges", *since)
		if err != nil {
			return err
		}
		revs = splitLines(out)
	} else {
		out, err := repo.Git("log", "--format=%H", "--reverse", "--no-merges", "-n", strconv.Itoa(*commits))
		if err != nil {
			return err
		}
		revs = splitLines(out)
	}
	if len(revs) == 0 {
		return fmt.Errorf("no commits to measure")
	}
	rep, err := gate.Run(repo, gate.Options{Revisions: revs, Threshold: *threshold, SkipEmpty: true})
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(env.stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	fmt.Fprint(env.stdout, rep.Text())
	if !rep.Passed {
		return fail(3, "")
	}
	return nil
}

func cmdPush(env *env, args []string) error {
	fs, dir := newFlags("push", env)
	rest, err := parse(fs, args)
	if err != nil {
		return exitError{code: 2}
	}
	repo, err := openRepo(*dir)
	if err != nil {
		return err
	}
	remote := firstOr(rest, "origin")
	out, err := repo.Git("push", remote, cer.Ref+":"+cer.Ref)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.stdout, "pushed %s to %s\n%s\n", cer.Ref, remote, strings.TrimSpace(out))
	return nil
}

func cmdFetch(env *env, args []string) error {
	fs, dir := newFlags("fetch", env)
	rest, err := parse(fs, args)
	if err != nil {
		return exitError{code: 2}
	}
	repo, err := openRepo(*dir)
	if err != nil {
		return err
	}
	remote := firstOr(rest, "origin")
	out, err := repo.Git("fetch", remote, "+refs/docket/*:refs/docket/*")
	if err != nil {
		return err
	}
	fmt.Fprintf(env.stdout, "fetched refs/docket/* from %s\n%s\n", remote, strings.TrimSpace(out))
	return nil
}

// firstOr returns the first positional argument, or a default.
func firstOr(args []string, fallback string) string {
	if len(args) > 0 && args[0] != "" {
		return args[0]
	}
	return fallback
}

func shortAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, short(s))
	}
	return out
}

func okNo(ok bool, yes, no string) string {
	if ok {
		return "ok, " + yes
	}
	return "FAILED, " + no
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitLines(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Split(strings.TrimSpace(s), "\n")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func short(s string) string {
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
