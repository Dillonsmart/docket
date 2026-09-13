<p align="center">
  <img src="Docket.png" alt="Docket" width="420">
</p>

<p align="center"><em>A per-commit evidence record for agent-written code.</em></p>

---

Coding agents produce code faster than anyone can verify it. The reviewer receives a finished diff with no implementation journey: no record of what the agent tried first, what it verified, or which lines nobody ever looked at. The harness already emits that journey and then throws it away at commit time.

Docket captures it, folds it against the diff, and produces a per-hunk evidence record — so review attention lands where there is no evidence.

```
$ docket show HEAD

docket 7a9da95a2a748c2fb99706f48f2ddf64760ffab2
  record     sha256:046c5278e93cb516e3ba72400fd1a69648c85b93833d31d57648e9ed8e535ebf
  trust      local_claimed (signed on local)
  verified   digest and signature check out (ed25519:f889a26cbc34c95e)

  1 hunks in 1 files, 4 added lines
  100% of added lines attributed to a recorded edit
  mean evidence density 0.77

src/auth.js:3-6 (4 lines)  covered  density 0.77
  origin      claude-code/main via Edit (reported by the harness)
              claude-opus-5
  task        Fix session fixation on login
  intent      The raw UUID is reused across logins, so minting a prefixed id instead.
  attempt     Stamping a creation time on the session so expiry can be checked. [superseded]
              vitest failed after this change: npx vitest run
  evidence    coverage: 4 of 4 lines executed (coverage/coverage-final.json)
  evidence    test_execution: pass (npx vitest run --coverage) — failed before this change
  human       no recorded human contact with these lines
```

No account. No network. The records live in the repository, on an orphan ref.

---

## Install

A single static binary. No Go toolchain, no runtime, nothing to compile.

```sh
curl -fsSL https://raw.githubusercontent.com/Dillonsmart/docket/main/install.sh | sh
```

Or without installing anything:

```sh
npx @dillonsmart/docket init
```

Or take the archive for your platform from [the releases page](https://github.com/Dillonsmart/docket/releases) — macOS and Linux on arm64 and x86-64, Windows on both — unpack it, and put `docket` on your PATH. Every release publishes `SHA256SUMS`; the installer checks them for you.

Building from source stays available for anyone who wants it:

```sh
go install github.com/Dillonsmart/docket/cmd/docket@latest
```

Then, in a repository:

```sh
docket init
```

That installs a `prepare-commit-msg` hook (which writes the trailer — the audited agent never writes its own audit record), a `post-commit` hook (which stores the record), the `refs/docket/*` refspec, a local signing key, and the Claude Code hooks that let docket observe edits made through the shell.

From then on, every commit gets one extra line, in the trailer block where `Signed-off-by` and `Reviewed-by` live:

```
    Add the session helper

    The API needs a stable id per session, and the obvious place is here
    rather than in the middleware.

    Reviewed-by: Someone Else <someone@example.com>
    Docket: sha256:db77fdb4b0c1fc6cc7644d3fa2204267e25799d74a6f8f665aff3d32b12c39af
```

That is the whole footprint in your history. `git log --oneline` is unchanged, your subject line is untouched, and the digest names a signed record on `refs/docket/records` rather than a URL — so nothing in the permanent history depends on a service still existing.

Amending rebuilds the record and replaces the trailer, so a commit never points at the content it used to have. A merge gets no trailer, and neither does a commit with nothing attributable in it. Rebases and cherry-picks are the awkward case: git does not run `prepare-commit-msg` for either, so the trailer travels onto a diff it was not built from — `docket verify` reports that plainly rather than trusting the trailer.

To stop: delete the two hooks in `.git/hooks`. Existing trailers stay in the history as inert text.

## Use it

```sh
docket show HEAD                      # the record for a commit, risk-ordered
docket explain src/auth.js:51         # why does this line exist? (via git blame)
docket review --format md             # the pull request comment
docket verify HEAD                    # digest, signature and commit binding
docket push                           # send the records to the remote
docket doctor                         # what can docket see here, and what can't it
docket gate --commits 20              # measure attribution against real history
```

For pull requests, [the GitHub Action](action/) reads the records the commits brought with them and posts one comment, reordering the diff so the hunks nothing can vouch for come first and the well-covered boilerplate is collapsed underneath. It downloads the same binary, so nothing needs installing on the runner either.

### Reading someone's reasoning back

The fields that answer *why does this code look like this* are `task`, `intent` and `attempt`:

- **task** — the request this edit descends from, i.e. what the human actually asked for.
- **intent** — what the agent said it was doing in the sentence immediately before it made the edit. This is where the reasoning lands, in the agent's own words.
- **attempt** — code written into this same region and then taken out again, with the check that failed in between when there was one. The abandoned approaches are the part that is otherwise lost within hours.

```
$ docket explain database/migrations/0001_01_01_000000_create_players_table.php:54

database/migrations/…:54 was last written by be66f35c5977

  origin      claude-code/main via Edit
  task        Look at the engine plan for this project, challenge any assumptions then start implementing
  intent      Postgres `jsonb` normalises key order, so the replayed response wasn't byte-identical
              to the original. For a stored response we only ever return verbatim, `json` is the
              right column type.
  attempt     Now the schema. Replacing the default `users` table with `players` as the
              authenticatable model. [superseded]
```

`docket explain` finds the commit through `git blame`, so you can start from the code in front of you rather than having to know which commit to look in. `docket show <sha> --all --json` gives the whole record if you would rather read it as data.

Docket stores short redacted excerpts, not the conversation: enough to reconstruct the decision, never the whole transcript, because whole transcripts carry secrets and grow without bound.

---

## How it works

**Attribution.** Docket parses the agent transcript into an ordered event stream, replays every recorded edit per file, and carries a provenance vector — one origin per line — forward through subsequent edits. At commit time it aligns the committed file against that replay and resolves each hunk.

A line is attributed only when its text is found at the aligned position *and* in the recorded output of the edit being credited. Timestamps only order events; they never justify an attribution.

When an edit's recorded pre-image disagrees with the replay — because a shell command, an editor or a person changed the file in between — the disagreement is detected, the affected lines become `unknown`, and the replay re-seeds from the recorded truth. Carrying a reconstruction past a disagreement is how tools produce confident, wrong answers. **An attribution engine that is confidently wrong is worse than no product**, so `unknown` is always an available answer, and it always comes with a reason.

**Agents.** Docket reads Claude Code, Codex CLI and opencode. Everything downstream — replay, attribution, evidence, the record — is agent-agnostic, so supporting another agent is a reader, not a redesign:

| Agent | Where its session lives | What docket gets from it |
|---|---|---|
| Claude Code | `~/.claude/projects/**/*.jsonl` | Edit/Write calls with before and after images, shell commands, prompts, the agent's own narration |
| Codex CLI | `~/.codex/sessions/**/rollout-*.jsonl` | `apply_patch` calls, shell commands **with exit codes**, prompts, narration |
| opencode | `~/.local/share/opencode/opencode.db` (needs `sqlite3`) | write/edit calls with diffs, shell commands **with exit codes**, prompts, narration |
| anything else | — | wire its hooks to `docket collect pre` / `docket collect post` and the edits are observed directly |

Codex and opencode send patches rather than whole files, so their edits carry no before-image. Docket seeds the replay from the base revision and applies the patch to the content it was actually written against; when that does not fit, it re-seeds and marks what it cannot explain rather than placing the hunk by guesswork.

**Observation.** Reading the transcript recovers `Edit` and `Write` calls. An agent that writes files through the shell — a heredoc, `sed -i`, a generator, a formatter — leaves nothing there to recover. So docket watches the working tree itself: a `PreToolUse` hook snapshots content, a `PostToolUse` hook diffs it, and both images are stored as git blobs. Those edits are marked `observed` rather than `transcript`, because docket read them itself.

**Evidence.** Test runs, type checks and static analysis are correlated with the edits they followed — a check that ran *before* the code was written is not evidence about it. Coverage reports (istanbul `coverage-final.json`, lcov) are matched line by line against the hunk, and ignored when the report is older than the code it would otherwise appear to cover.

**Density.** One number per hunk, between 0 and 1, [published in full](spec/CER.md#6-evidence-density) rather than tuned in private: coverage of these lines is worth at most 0.5, a check that passed after the edit 0.3 (0.35 if this change turned it green), type and static checks 0.1 together, recorded human contact 0.1. If nothing executed the code, the score is capped at 0.15. If nobody can say who wrote it, at 0.5. A locally-claimed record scores 0.9 of what a CI-attested one would.

Those caps are the point. This number will be turned into a target, exactly as coverage was, and a metric you can raise without running anything is worse than no metric.

---

## How well does attribution actually work

Docket measures itself. `docket gate` replays real sessions against real commits and reports what it could and could not explain:

| Session | Agent | Hunks in files the session edited | Added lines | Content-verified |
|---|---|---|---|---|
| A Laravel engine, Edit/Write tools (4 commits, 98 edits) | Claude Code | **98.1%** | 99.8% | 100% |
| A Python app, `apply_patch` (4 commits, 256 edits) | Codex CLI | **94.7%** | 95.7% | 99.8% |
| A FastAPI project, 100 edits (uncommitted, measured against the working tree) | opencode | — | **92.9%** | — |
| A PHP framework built largely through the shell (12 commits, 79 edits) | Claude Code | 59.2% | 82.1% | 100% |

Two things to take from this. Every attribution was verified against the crediting edit's own recorded output — docket did not credit a line to an edit that did not write it. And the last row is why the collector exists: those sessions wrote files with heredocs and `sed`, which no transcript records, and that work happened before docket could observe it. With `docket init` in place, those edits are observed directly.

Across whole diffs the headline rate is lower — 40%, 94%, 37% — because real commits also contain `composer.lock`, scaffolded models and generated code that no agent edit ever touched. Docket reports those as `unknown` with the commands that ran nearby listed as *candidates*, never as attributions. That is the honest answer, and it is deliberately not smoothed into the number.

Run it on your own history:

```sh
docket gate --commits 20
```

---

## What it is not

- Not orchestration, task assignment or agent spawning.
- Not agent-to-agent handoff or provider routing.
- Not a window you live in. It annotates the review surface you already use.
- Not a live dashboard.
- Not something that asks for an account before it does anything.

## Privacy

Traces contain raw prompts, file contents and terminal output. So:

- Everything that reaches a record goes through an aggressive redaction pass: known credential shapes, private keys, JWTs, authorization headers, connection-string credentials, `KEY=`/`SECRET=`/`PASSWORD=` assignments — and, failing closed, any long token whose character distribution looks random.
- A record stores short redacted excerpts, never whole files or whole prompts.
- Nothing is sent anywhere. The records live in your repository, and `docket push` sends them to the git host you already trust.
- The signing key lives in `.git/docket/` and never leaves the machine.

## Trust

| Tier | Meaning |
|---|---|
| `local_claimed` | Built on a developer machine, signed with a key that machine holds. It is a claim. |
| `ci_attested` | Built by a runner, signed with a key the developer does not hold. Set `DOCKET_SIGNING_KEY` in CI. |

The distinction is in the schema from day one and is shown everywhere a record is rendered.

## The format

The record format is specified separately as the [**Commit Evidence Record**](spec/CER.md), with a [JSON Schema](spec/cer-0.1.schema.json). Docket is its reference implementation. The specification is Apache 2.0 and deliberately boring: it is meant to be implemented by other tools, including ones that compete with this one.

## Status

Built and working: attribution, the shell-edit collector, redaction, signed records on an orphan ref, coverage and test correlation, the terminal viewer, `explain`, the pull request comment, the GitHub Action, and the self-measurement gate.

Not built yet: readers for Gemini CLI and anything ACP-native, coverage correlation beyond istanbul/lcov, GitLab, cross-repository aggregation, policy gates on paths, and the hosted team tier.

## Licence

Apache 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
