// Package agents discovers every agent session that touched a repository.
//
// Docket's whole model — an ordered stream of edits and commands, replayed per
// file — is deliberately agent-agnostic. What differs between harnesses is only
// where the session is written and in what shape, so supporting another agent
// is a reader, not a redesign. This package holds the readers and the roster.
package agents

import (
	"errors"
	"os"

	"github.com/Dillonsmart/docket/internal/agents/codex"
	"github.com/Dillonsmart/docket/internal/agents/opencode"
	"github.com/Dillonsmart/docket/internal/transcript"
)

// Found is one discovered session and what reading it cost.
type Found struct {
	Session *transcript.Session
	Stats   transcript.ParseStats
}

// Problem is a source docket could see but not read. These are reported rather
// than swallowed: "no evidence" and "docket could not look" are different
// answers, and only one of them is about the code.
type Problem struct {
	Agent  string
	Detail string
}

// Discover returns every session from every supported agent that ran inside a
// repository, along with anything that could not be read.
func Discover(repoRoot string) ([]Found, []Problem) {
	var found []Found
	var problems []Problem

	// Claude Code: one JSONL transcript per session.
	if paths, err := transcript.Find(repoRoot); err == nil {
		for _, p := range paths {
			s, stats, err := transcript.Parse(p)
			if err != nil {
				problems = append(problems, Problem{Agent: transcript.AgentClaudeCode, Detail: p + ": " + err.Error()})
				continue
			}
			found = append(found, Found{Session: s, Stats: stats})
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		problems = append(problems, Problem{Agent: transcript.AgentClaudeCode, Detail: err.Error()})
	}

	// Codex: a rollout file per session, under a date-partitioned directory.
	if sessions, stats, err := codex.Sessions(repoRoot); err == nil {
		for i, s := range sessions {
			f := Found{Session: s}
			if i < len(stats) {
				f.Stats = stats[i]
			}
			found = append(found, f)
		}
	} else {
		problems = append(problems, Problem{Agent: transcript.AgentCodex, Detail: err.Error()})
	}

	// opencode: everything in one SQLite database.
	sessions, err := opencode.Sessions(repoRoot)
	switch {
	case err == nil:
		for _, s := range sessions {
			found = append(found, Found{Session: s})
		}
	case errors.Is(err, opencode.ErrNoSQLite):
		problems = append(problems, Problem{Agent: transcript.AgentOpencode,
			Detail: "sessions found but sqlite3 is not installed, so they cannot be read"})
	case errors.Is(err, os.ErrNotExist):
		// opencode is simply not installed here.
	default:
		problems = append(problems, Problem{Agent: transcript.AgentOpencode, Detail: err.Error()})
	}

	return found, problems
}

// Installed reports which agents docket can see data for at all, which is what
// `docket doctor` prints.
func Installed(repoRoot string) map[string]int {
	counts := map[string]int{}
	found, _ := Discover(repoRoot)
	for _, f := range found {
		counts[f.Session.Agent] += len(f.Session.Edits)
	}
	return counts
}
