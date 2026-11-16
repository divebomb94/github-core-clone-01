# github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/divebomb94/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/divebomb94/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge in a single Node process, standard library only: git smart HTTP (protocol v0 + v2 — advertisement, ls-refs, fetch with shallow/deepen, receive-pack with report-status), the REST v3 surface (repos, branches, commits, contents, issues, comments, labels, pull requests with squash merges and conflict detection), a GraphQL v4 subset (viewer, repository, issues, pull requests, mutations, node ids, introspection), and an MCP endpoint (streamable HTTP JSON-RPC with the six core tools). Everything is served from its own in-memory, content-addressed object store. Passes all 120 of the bounty conformance checks locally.

## Run it

```sh
MAKEFUN_ENV_URL=<trial env url> MAKEFUN_SHOP_TOKEN=<token> node main.ts
```

The process binds an ephemeral port, registers it with the trial environment, serves until the environment reports done, then exits.

---

<sub>Entered on [make.fun](https://make.fun/?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
