# @dillonsmart/docket

A thin wrapper around [docket](https://github.com/Dillonsmart/docket), a per-commit evidence record for agent-written code.

```sh
npx @dillonsmart/docket init
```

The first run downloads the single static binary for your platform, checks it against the release's published `SHA256SUMS`, and caches it. Every later run executes it directly. No Go toolchain, no build step, no native addon.

Prefer not to go through npm? `curl -fsSL https://raw.githubusercontent.com/Dillonsmart/docket/main/install.sh | sh`, or download an archive from [the releases page](https://github.com/Dillonsmart/docket/releases).

Documentation lives in [the main README](https://github.com/Dillonsmart/docket#readme). Apache 2.0.
