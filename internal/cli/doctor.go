package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Dillonsmart/docket/internal/agents"
	"github.com/Dillonsmart/docket/internal/agents/codex"
	"github.com/Dillonsmart/docket/internal/cer"
	"github.com/Dillonsmart/docket/internal/collect"
	"github.com/Dillonsmart/docket/internal/evidence"
	"github.com/Dillonsmart/docket/internal/store"
	"github.com/Dillonsmart/docket/internal/transcript"
)

// cmdDoctor reports what docket can and cannot see. It exists because every
// failure mode of this tool is invisible by default: a missing hook, an
// unreadable transcript or a stale coverage report all look exactly like "no
// evidence", and a reviewer would draw the wrong conclusion from that.
func cmdDoctor(env *env, args []string) error {
	fs, dir := newFlags("doctor", env)
	if _, err := parse(fs, args); err != nil {
		return exitError{code: 2}
	}
	repo, err := openRepo(*dir)
	if err != nil {
		return err
	}
	out := env.stdout
	fmt.Fprintf(out, "repository   %s\n", repo.Root)
	fmt.Fprintf(out, "git dir      %s\n\n", repo.GitDir)

	// Hooks.
	hooksDir := repo.HooksDir()
	for _, name := range gitHooks {
		path := filepath.Join(hooksDir, name)
		data, err := os.ReadFile(path)
		switch {
		case err != nil:
			fmt.Fprintf(out, "hook         %-20s MISSING — run docket init\n", name)
		case !containsBytes(data, "docket hook "):
			fmt.Fprintf(out, "hook         %-20s present but not docket's\n", name)
		default:
			fmt.Fprintf(out, "hook         %-20s ok\n", name)
		}
	}

	// Agent hooks.
	agentPath := agentHooksPath(repo)
	if data, err := os.ReadFile(agentPath); err == nil && containsBytes(data, "docket collect") {
		fmt.Fprintf(out, "agent hooks  %-20s ok (shell edits are observed)\n", rel(repo.Root, agentPath))
	} else {
		fmt.Fprintf(out, "agent hooks  %-20s MISSING — edits made through the shell will be unattributable\n", rel(repo.Root, agentPath))
	}

	// Refspec.
	found := false
	for _, remote := range splitLines(mustGit(repo, "remote")) {
		for _, spec := range repo.ConfigGetAll("remote." + remote + ".fetch") {
			if spec == "+refs/docket/*:refs/docket/*" {
				fmt.Fprintf(out, "refspec      %-20s ok\n", remote)
				found = true
			}
		}
	}
	if !found {
		fmt.Fprintf(out, "refspec      %-20s not configured — records will not arrive in clones\n", "")
	}

	// Stored records.
	digests, _ := store.List(repo)
	fmt.Fprintf(out, "records      %d stored on %s\n", len(digests), cer.Ref)

	// Agent sessions.
	fmt.Fprintln(out)
	sessions, problems := agents.Discover(repo.Root)
	if len(sessions) == 0 {
		fmt.Fprintf(out, "sessions     none found for this repository\n")
		fmt.Fprintf(out, "             docket looks in %s (Claude Code), %s (Codex) and the opencode database\n",
			transcript.ProjectsDir(), filepath.Join(codex.Home(), "sessions"))
	}
	byAgent := map[string]int{}
	for _, f := range sessions {
		s := f.Session
		byAgent[s.Agent]++
		fmt.Fprintf(out, "session      %-14s %s  %d edits, %d commands, %d prompts\n",
			s.Agent, short(s.ID), len(s.Edits), len(s.Commands), len(s.Prompts))
		st := f.Stats
		if st.Unparsable > 0 || st.EditsRecovered > 0 || st.ResultsOrphaned > 0 || st.OversizeLines > 0 {
			fmt.Fprintf(out, "             lossy: %d unparsable lines, %d oversize, %d edits without images, %d results with no call\n",
				st.Unparsable, st.OversizeLines, st.EditsRecovered, st.ResultsOrphaned)
		}
	}
	for _, p := range problems {
		// A source docket cannot read is worth shouting about: it looks exactly
		// like an agent that never wrote anything.
		fmt.Fprintf(out, "session      %-14s COULD NOT READ: %s\n", p.Agent, p.Detail)
	}

	// Collector.
	fmt.Fprintln(out)
	if st, err := collect.Open(repo); err == nil {
		snap := st.Load()
		if snap == nil {
			fmt.Fprintf(out, "collector    no baseline snapshot — run docket init\n")
		} else {
			fmt.Fprintf(out, "collector    baseline %s, %d files tracked\n", snap.Taken.Format(time.RFC3339), len(snap.Files))
		}
		sessions, _ := st.Sessions()
		observed := 0
		for _, s := range sessions {
			observed += len(s.Edits)
		}
		fmt.Fprintf(out, "             %d observed edits in %d logs\n", observed, len(sessions))
	}

	// Coverage.
	reports := evidence.Discover(repo.Root)
	if len(reports) == 0 {
		fmt.Fprintf(out, "coverage     none found — hunks can only be backed by whole test runs, not by line execution\n")
	}
	for _, r := range reports {
		st, err := os.Stat(r)
		if err != nil {
			continue
		}
		fmt.Fprintf(out, "coverage     %s (%s)\n", rel(repo.Root, r), st.ModTime().Format(time.RFC3339))
	}

	// Commits without records, which is the number that matters once this is
	// installed: a docket you forgot to build is indistinguishable from code
	// with no evidence.
	fmt.Fprintln(out)
	revs := splitLines(mustGit(repo, "log", "--format=%H", "-n", "20", "--no-merges"))
	missing := 0
	for _, r := range revs {
		if _, ok := repo.Trailer(r, cer.Trailer); !ok {
			missing++
		}
	}
	if len(revs) > 0 {
		fmt.Fprintf(out, "coverage of history  %d of the last %d commits carry a %s trailer\n", len(revs)-missing, len(revs), cer.Trailer)
	}
	return nil
}

func mustGit(repo interface {
	Git(args ...string) (string, error)
}, args ...string) string {
	out, err := repo.Git(args...)
	if err != nil {
		return ""
	}
	return out
}

func containsBytes(data []byte, needle string) bool {
	return bytes.Contains(data, []byte(needle))
}
