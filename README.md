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

```sh
go install github.com/Dillonsmart/docket/cmd/docket@latest
```

Then, in a repository:

```sh
docket init
```

That installs a `prepare-commit-msg` hook (which writes the trailer — the audited agent never writes its own audit record), a `post-commit` hook (which stores the record), the `refs/docket/*` refspec, a local signing key, and the Claude Code hooks that let docket observe edits made through the shell.

From then on, every commit gets a trailer:

```
Docket: sha256:8f3a2b…
```

and a signed record on `refs/docket/records`, addressed by that digest.

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

For pull requests, [the GitHub Action](action/) reads the records the commits brought with them and posts one comment, reordering the diff so the hunks nothing can vouch for come first and the well-covered boilerplate is collapsed underneath.

---

## How it works

**Attribution.** Docket parses the agent transcript into an ordered event stream, replays every recorded edit per file, and carries a provenance vector — one origin per line — forward through subsequent edits. At commit time it aligns the committed file against that replay and resolves each hunk.

A line is attributed only when its text is found at the aligned position *and* in the recorded output of the edit being credited. Timestamps only order events; they never justify an attribution.

When an edit's recorded pre-image disagrees with the replay — because a shell command, an editor or a person changed the file in between — the disagreement is detected, the affected lines become `unknown`, and the replay re-seeds from the recorded truth. Carrying a reconstruction past a disagreement is how tools produce confident, wrong answers. **An attribution engine that is confidently wrong is worse than no product**, so `unknown` is always an available answer, and it always comes with a reason.

**Observation.** Reading the transcript recovers `Edit` and `Write` calls. An agent that writes files through the shell — a heredoc, `sed -i`, a generator, a formatter — leaves nothing there to recover. So docket watches the working tree itself: a `PreToolUse` hook snapshots content, a `PostToolUse` hook diffs it, and both images are stored as git blobs. Those edits are marked `observed` rather than `transcript`, because docket read them itself.

**Evidence.** Test runs, type checks and static analysis are correlated with the edits they followed — a check that ran *before* the code was written is not evidence about it. Coverage reports (istanbul `coverage-final.json`, lcov) are matched line by line against the hunk, and ignored when the report is older than the code it would otherwise appear to cover.

**Density.** One number per hunk, between 0 and 1, [published in full](spec/CER.md#6-evidence-density) rather than tuned in private: coverage of these lines is worth at most 0.5, a check that passed after the edit 0.3 (0.35 if this change turned it green), type and static checks 0.1 together, recorded human contact 0.1. If nothing executed the code, the score is capped at 0.15. If nobody can say who wrote it, at 0.5. A locally-claimed record scores 0.9 of what a CI-attested one would.

Those caps are the point. This number will be turned into a target, exactly as coverage was, and a metric you can raise without running anything is worse than no metric.

---

## How well does attribution actually work

Docket measures itself. `docket gate` replays real sessions against real commits and reports what it could and could not explain:

| Repository | Hunks in files the session edited | Added lines | Content-verified |
|---|---|---|---|
| A Laravel engine built with the Edit/Write tools (4 commits, 98 edits) | **96.2%** | 99.6% | 100% |
| A PHP framework built largely through the shell (12 commits, 79 edits) | **59.2%** | 82.1% | 100% |

Two things to take from this. Every attribution docket made was verified against the crediting edit's own recorded output — it did not credit a single line to an edit that did not write it. And the second row is why the collector exists: those sessions wrote files with heredocs and `sed`, which the transcript does not record, and that work happened before docket could observe it. With `docket init` in place, those edits are observed directly.

Across whole diffs the headline rate is lower — 40% and 37% — because real commits contain `composer.lock`, scaffolded models and generated code that no agent edit ever touched. Docket reports those as `unknown` with the commands that ran nearby listed as *candidates*, never as attributions. That is the honest answer, and it is deliberately not smoothed into the number.

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

Not built yet: collectors for other agents (Codex, Gemini, anything ACP-native), coverage correlation beyond istanbul/lcov, GitLab, cross-repository aggregation, policy gates on paths, and the hosted team tier.

## Licence

Apache 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
