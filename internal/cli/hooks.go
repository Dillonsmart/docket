package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Dillonsmart/docket/internal/build"
	"github.com/Dillonsmart/docket/internal/cer"
	"github.com/Dillonsmart/docket/internal/collect"
	"github.com/Dillonsmart/docket/internal/gitx"
	"github.com/Dillonsmart/docket/internal/store"
)

// hookMain runs a git hook. It returns 0 in almost every circumstance: a tool
// that can block a commit because its own record failed to build will be
// uninstalled the first time it happens, and then there is no record of
// anything.
func hookMain(env *env, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(env.stderr, "docket hook: which hook?")
		return 2
	}
	repo, err := openRepo("")
	if err != nil {
		return 0
	}
	switch args[0] {
	case "prepare-commit-msg":
		if err := prepareCommitMsg(repo, args[1:]); err != nil {
			logf(repo, "prepare-commit-msg: %v", err)
		}
	case "post-commit":
		if err := postCommit(repo); err != nil {
			logf(repo, "post-commit: %v", err)
		}
	default:
		logf(repo, "unknown hook %q", args[0])
	}
	return 0
}

// prepareCommitMsg builds the record for the staged change and writes its digest
// into the commit message as a trailer.
//
// The record cannot contain the commit id — the commit does not exist yet — so
// the digest covers the evidence only, and post-commit stamps the id on
// afterwards. That is what the envelope carve-out in the schema is for.
func prepareCommitMsg(repo *gitx.Repo, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("no message file given")
	}
	msgFile := args[0]
	source := ""
	if len(args) > 1 {
		source = args[1]
	}
	switch source {
	case "merge", "squash":
		// A merge introduces no authored lines of its own.
		return nil
	}
	existing, err := os.ReadFile(msgFile)
	if err != nil {
		return err
	}
	if _, ok := gitx.TrailerIn(string(existing), cer.Trailer); ok {
		return nil // amending a commit that already has one
	}

	signer, err := signerFor(repo, false)
	if err != nil {
		return err
	}
	res, err := build.Build(build.Options{Repo: repo, Signer: signer})
	if err != nil {
		return err
	}
	if len(res.Record.Hunks) == 0 && res.Record.Totals.AddedLines == 0 {
		// Nothing attributable: a trailer pointing at an empty record would be
		// noise in the history for ever.
		return nil
	}
	if err := store.SavePending(repo, res.Record); err != nil {
		return err
	}
	// git interpret-trailers knows where a trailer belongs, including how to
	// handle the comment block an interactive commit adds.
	if _, err := repo.Git("interpret-trailers", "--in-place",
		"--trailer", cer.Trailer+": "+res.Record.PayloadHash, msgFile); err != nil {
		return err
	}
	logf(repo, "prepared %s for %d hunks", res.Record.PayloadHash, len(res.Record.Hunks))
	return nil
}

// postCommit stores the record now that the commit exists.
func postCommit(repo *gitx.Repo) error {
	sha, err := repo.RevParse("HEAD")
	if err != nil {
		return err
	}
	digest, ok := repo.Trailer(sha, cer.Trailer)
	if !ok || digest == "" {
		// The commit was made with --no-verify, or by something that bypassed the
		// hook. Recording nothing is the honest outcome.
		return nil
	}
	rec, err := store.LoadPending(repo, digest)
	if err != nil {
		// The parked record is gone (a rebase, a cherry-pick, a different
		// machine). Rebuild it: the build is a function of the diff and the
		// session, so the digest should come out the same — and if it does not,
		// verify will say so rather than pretend.
		signer, serr := signerFor(repo, false)
		if serr != nil {
			return serr
		}
		res, berr := build.Build(build.Options{Repo: repo, Rev: sha, Signer: signer})
		if berr != nil {
			return berr
		}
		rec = res.Record
		if rec.PayloadHash != digest {
			logf(repo, "rebuilt record %s does not match trailer %s", rec.PayloadHash, digest)
		}
	}
	rec.Commit = sha
	// The signature covers the payload digest, which excludes the commit id, so
	// stamping it on does not invalidate anything.
	if _, err := store.Write(repo, rec); err != nil {
		return err
	}
	store.ClearPending(repo, digest)
	logf(repo, "stored %s for %s", rec.PayloadHash, sha)

	// Re-baseline the collector so the next command's diff starts from the
	// committed tree.
	if st, err := collect.Open(repo); err == nil {
		_ = st.Before("")
	}
	return nil
}

// collectMain is the agent hook entry point. Like the git hooks it never fails
// loudly: a collector that can break the agent's shell is worse than a collector
// that misses an edit.
func collectMain(env *env, args []string) int {
	phase := ""
	if len(args) > 0 {
		phase = args[0]
	}
	in := collect.ReadHookInput(env.stdin)
	dir := in.CWD
	if dir == "" {
		dir, _ = os.Getwd()
	}
	repo, err := gitx.Open(dir)
	if err != nil {
		return 0 // not a repository: nothing to observe
	}
	st, err := collect.Open(repo)
	if err != nil {
		logf(repo, "collect: %v", err)
		return 0
	}
	switch phase {
	case "pre":
		if err := st.Before(in.SessionID); err != nil {
			logf(repo, "collect pre: %v", err)
		}
	case "post":
		events, err := st.After(collect.EventMeta{
			Session: in.SessionID, Tool: firstNonEmpty(in.ToolName, "Bash"),
			Command: in.Command(), Actor: "agent", ToolUseID: in.ToolUseID,
			At: time.Now().UTC(),
		})
		if err != nil {
			logf(repo, "collect post: %v", err)
		} else if len(events) > 0 {
			logf(repo, "observed %d file changes after: %s", len(events), oneLine(in.Command()))
		}
	default:
		logf(repo, "collect: unknown phase %q", phase)
	}
	return 0
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func oneLine(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ⏎ ")
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}
