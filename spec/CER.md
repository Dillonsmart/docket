# Commit Evidence Record (CER), version 0.1

Status: draft. Licence: Apache 2.0. Reference implementation: [docket](https://github.com/Dillonsmart/docket).

A Commit Evidence Record answers three questions about every hunk in a commit:

1. **Where did it come from?** Which agent, model, session and request, or a human, or nothing that can be established.
2. **What was tried first?** Approaches written into this region and then taken out again, and why.
3. **What verified it?** Which checks ran after the code was written, and whether they executed these exact lines.

The format is deliberately unglamorous. It is meant to be produced by several tools, consumed by review surfaces that have never heard of any of them, and still readable when the tool that wrote it no longer exists.

---

## 1. Design rules

These are the constraints every part of the format follows. An implementation that breaks one of them is not producing CER.

**R1 — Unknown is a valid answer, and the only honest one when nothing is established.** A record must never attribute a line by proximity, timing or likelihood. Attribution requires recorded content that matches the committed line.

**R2 — Evidence is graded by what produced it.** A record states whether an edit was *observed* (the producer read the file before and after) or *reported* (an agent harness said so), and whether the record was built on a developer machine or by CI. It also names the agent that wrote the code, which is a different question from which model did.

**R3 — The record is content-addressed, not location-addressed.** A commit refers to its record by digest. No URL, no vendor, no service that has to still exist.

**R4 — The digest excludes the envelope.** The commit id cannot be inside the hashed payload, because the digest is written into the commit message, which is fixed before the commit exists. See §4.

**R5 — Free text is redacted before it is stored.** Prompts, intents, commands and test output can carry credentials. An implementation must redact aggressively and record that it did.

---

## 2. Document

A record is a single JSON object. Unknown members must be preserved by consumers that re-serialise it.

| Member | Type | Required | Meaning |
|---|---|---|---|
| `docket_version` | string | yes | Schema version, `"0.1"`. |
| `spec` | string | yes | `"CER/0.1"`. |
| `trust` | string | yes | `local_claimed` or `ci_attested`. See §5. |
| `base` | string | yes | Revision the diff was taken against. |
| `generator` | object | yes | `{name, version}` of the producing tool. |
| `redaction` | object | yes | `{version, mode, rules_fired[]}`. |
| `sessions` | array | yes | The agent sessions that contributed. May be empty. |
| `hunks` | array | yes | One entry per diff hunk that adds lines. |
| `totals` | object | yes | Commit-level roll-up. |
| `commit` | string | envelope | The commit the record describes. Empty while staged. |
| `generated_at` | string | envelope | RFC 3339. |
| `payload_digest` | string | envelope | `sha256:<hex>` over the canonical payload. |
| `signature` | object | envelope | Detached signature over `payload_digest`. |

### 2.1 Hunk

| Member | Type | Meaning |
|---|---|---|
| `file` | string | Repository-relative path, forward slashes. |
| `range` | [int, int] | Inclusive new-side line range. |
| `commit` | string | Only in an aggregate covering several commits. |
| `origin` | object | See §2.2. |
| `contributors` | array | Other edits that wrote lines in this hunk. |
| `attempts` | array | See §2.3. |
| `evidence` | array | See §2.4. |
| `human_contact` | string | `none`, `edited`, `approved`, `viewed`. |
| `added_lines` | int | Lines the hunk adds. |
| `attributed_lines` | int | Of those, how many resolve to a recorded edit. |
| `verified_lines` | int | Of those, how many appear verbatim in that edit's recorded output. |
| `attribution_confidence` | string | `high`, `medium`, `none`. See §3. |
| `evidence_density` | number | 0–1. See §6. |
| `unknown` | object | Present when `origin.actor` is `unknown`: `{reasons{}, candidates[]}`. |

### 2.2 Origin

`actor` is `agent`, `human` or `unknown`. For an agent: `agent_id`, `model`, `session`, `task` (the request this descends from), `tool`, `at`, `intent` (what the agent said it was doing), and `command` for shell-driven edits.

`agent_id` is `<harness>/<role>` — `claude-code/main`, `claude-code/subagent`, `codex/main`, `opencode/main` — and the session it belongs to carries the harness name in `sessions[].agent`. An implementation must not invent a role it cannot establish: a harness that does not record which subagent definition ran says `subagent`, not a name.

`source` is `observed` or `transcript` (R2).

A record must not populate `agent_id` or `model` when `actor` is `unknown`.

### 2.3 Attempt

`{summary, outcome, reason, lines, at, sample}` where `outcome` is `abandoned` or `superseded`. `reason` should name the check that failed between the attempt and its removal, when one did. An attempt without evidence that it existed must not be recorded.

### 2.4 Evidence

`{kind, ref, result, transitioned, observed_after_edit, covered_lines, total_lines, confidence, at}`.

`kind` is `test_execution`, `coverage`, `typecheck` or `static_check`. `observed_after_edit` states whether the check ran after the code was written; a check that ran before it is not evidence about it. `transitioned` means the check failed before this change and passes after — a much stronger signal than a suite that was always green.

An implementation that reads a harness recording only patches (no pre-image) must resolve them against the content the edit ran against — normally the base revision — and must not place a hunk whose context cannot be found.

### 2.5 Unknown

`reasons` counts lines by why they could not be attributed:

| Reason | Meaning |
|---|---|
| `pre_existing` | The line was already there when the session first saw the file. |
| `untracked_mutation` | The file changed outside any recorded edit. |
| `human_edit` | The harness reported the human changing the file. |
| `lossy_edit` | An edit was recorded but could not be replayed. |
| `not_in_timeline` | The committed line appears nowhere in the replay. |

`candidates` may list commands that could account for the change. They are candidates, never attributions: `path_mentioned` marks the circumstantial case where the command line names the file.

---

## 3. Attribution

An implementation resolves each added line to at most one recorded edit. To claim `high` confidence, the line's text must be present in the crediting edit's recorded post-image. `medium` means the line aligned to a replay of recorded edits but was not found verbatim. `none` means nothing is claimed.

Replay must detect divergence: when an edit's recorded pre-image disagrees with the replayed content, the lines that differ become unknown with `untracked_mutation` (or `human_edit`), and replay re-seeds from the recorded pre-image. Carrying a reconstruction forward past a disagreement is how wrong attributions are produced, and R1 forbids it.

Events after the commit's timestamp must be excluded. Because git records commit times to the second, an implementation may widen the cutoff by up to one second.

---

## 4. Canonical form and digest

The canonical form is JSON with:

- object members sorted by Unicode code point,
- no insignificant whitespace,
- no HTML escaping,
- numbers rendered as ECMAScript `Number::toString`.

`payload_digest` is `"sha256:" + hex(sha256(canonical(record − envelope)))`, where the envelope is `commit`, `generated_at`, `payload_digest` and `signature`.

The commit refers to its record through a git trailer:

```
Docket: sha256:8f3a2b…
```

Because the trailer is written before the commit object exists, the digest cannot cover the commit id. A verifier checks the binding in the other direction: the commit's trailer names a record, and the record's `commit` member names that commit.

---

## 5. Trust

| Tier | Meaning |
|---|---|
| `local_claimed` | Built on a developer machine and signed with a key that machine holds. It is a claim by that machine. |
| `ci_attested` | Built by a runner and signed with a key the developer does not hold. |

Signatures are ed25519 over the ASCII `payload_digest`. `signature` carries `{alg, key_id, sig, public_key, signer}`; `key_id` is `"ed25519:" + hex(sha256(public_key))[0:16]`.

An aggregate over several records takes the lowest tier of its members.

---

## 6. Evidence density

`evidence_density` is a number in [0, 1] summarising how well a hunk is backed. The formula must be published by any implementation that emits it. The reference implementation uses:

| Component | Value |
|---|---|
| Coverage of the hunk's lines, from a report newer than the code | up to 0.5, proportional |
| A check that passed after the edit | 0.3, or 0.35 if it failed before the change |
| Type and static checks that passed after the edit | 0.05 each, capped at 0.1 |
| Recorded human contact | 0.1 (`edited`/`approved`), 0.05 (`viewed`) |

Then, in order: if nothing executed the code, the score is capped at 0.15; if the origin is unknown, at 0.5; if `trust` is not `ci_attested`, the score is multiplied by 0.9.

The caps exist because this number will be turned into a target, and a metric that can be raised without running anything is worse than no metric.

---

## 7. Storage

The reference storage is an orphan ref in the repository itself:

```
refs/docket/records
  records/<first two hex of digest>/<rest>.json
  by-commit/<first two hex of commit>/<rest>          → the digest
```

Orphan refs are not fetched by default; a producer should configure `+refs/docket/*:refs/docket/*` on the remote. The by-commit index exists because a squash merge or rebase rewrites the message and drops the trailer.

Nothing in the format depends on this storage. A record is a JSON document and may be kept anywhere.

---

## 8. Conformance

A producer conforms if it:

1. emits documents that validate against `cer-0.1.schema.json`,
2. computes `payload_digest` per §4,
3. never attributes a line without matching recorded content (R1),
4. records `source` and `trust` truthfully (R2),
5. redacts free text before storing it (R5).

A consumer conforms if it preserves unknown members and does not treat `local_claimed` as attested.

---

## 9. Relationship to other work

CER describes what happened to a **commit**. It is not an attempt to re-do:

- **SLSA / in-toto / Sigstore** describe how an artifact was *built* and by whom. CER describes how the source was *written* and what checked it. A CER record is a reasonable input to a build attestation, not a replacement for one.
- **OpenTelemetry GenAI semantic conventions** describe agent spans in flight. CER is the durable residue of those spans, folded against a diff. The obvious alignment is for a subagent span to carry the task it was handed, which today it does not.
- **ACP** carries sessions, permission requests and tool execution between editors and agents. It is one field short of carrying provenance; CER's `origin` is the shape that field would take.
