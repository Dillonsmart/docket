# Docket GitHub Action

Reorders a pull request by evidence: the hunks nothing can vouch for first, everything with a test behind it collapsed underneath. It reads the records the commits brought with them on `refs/docket/*` — it does not rebuild them, because the transcripts only exist on the machine where the work happened.

```yaml
permissions:
  contents: read
  pull-requests: write

jobs:
  evidence:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - uses: Dillonsmart/docket/action@main
        with:
          fail-under: "0.1"   # optional: block merge on hunks with no evidence
```

| Input | Default | Meaning |
|---|---|---|
| `version` | `latest` | `latest` or a tag (`v0.0.1`) downloads the prebuilt binary — no Go on the runner. `local` builds the checked-out source. Anything else is a branch or commit to build from. |
| `base` / `head` | the pull request's | Revisions to review. |
| `comment` | `true` | Post and update one comment on the pull request. |
| `fail-under` | empty | Fail the job when any hunk is below this evidence density. |
| `github-token` | `github.token` | Needs `pull-requests: write` to comment. |

If the job warns that there are no `refs/docket/*` on the remote, the commits were made without `docket init`, or nobody has run `docket push`.
