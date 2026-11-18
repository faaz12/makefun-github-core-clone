# makefun-github-core-clone

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/faaz12/makefun-github-core-clone/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/faaz12/makefun-github-core-clone/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge, built for the [make.fun bounty](https://make.fun/bounty/github-core-clone-01)
"Build a GitHub-compatible git forge" ($500).

One process, Node 22 stdlib only, no dependencies, no network, no git binary.
It serves, all in memory:

- **git smart HTTP** at `/{owner}/{repo}.git` — `info/refs` (protocol v2 and v0),
  `git-upload-pack` (v2 `ls-refs` + `fetch` with shallow/deepen, v0 fetch) and
  `git-receive-pack` (v0, report-status, side-band-64k, fast-forward checks).
  Pure-TypeScript object model: SHA-1 objects, pack encode/decode (incl.
  OFS/REF deltas and thin packs), pkt-line framing.
- **REST v3** under `/api/v3` — repositories, refs, branches, commits,
  contents, labels, issues, comments, pull requests, squash merges with
  conflict detection.
- **GraphQL v4** at `/api/graphql` — the documents `gh` and
  `github-mcp-server` send: viewer, repository, issues (states/orderBy/cursor
  pagination), pullRequests, node(id:), __type introspection, and the
  createIssue / closeIssue / createPullRequest / mergePullRequest / addComment
  mutations.
- **MCP** at `/mcp` — JSON-RPC 2.0 over Streamable HTTP, era 2026-07-28
  (`server/discover`, `tools/list`, `tools/call`): create_repo, get_repo,
  create_issue, list_issues, create_pull_request, merge_pull_request.

Auth is a single bearer token (`MAKEFUN_SHOP_TOKEN`); the viewer is the
fixed scenario account. All responses are deterministic (no wall clock in
any payload: timestamps come from `MAKEFUN_TRIAL_NOW`).

## Go core (2026-11-18)

The entry now runs a statically linked Go binary instead of the TypeScript
server. `main.ts` is a thin shim that execs the vendored `./forge` binary
(built from `./go` with `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"`,
linux/amd64, 3.3 MB — under the 8 MiB per-file cap). The Go code is a
behavior-identical port of the TypeScript sources in this repo (same routes,
same wire bytes), with the top-10 pattern applied: raw-TCP HTTP/1.1 server
(no `net/http`), zero-alloc hot paths, single-inflate pack ingest, and a
pre-serialized MCP `tools/list` payload. `go vet` clean, `go test ./...`
passes (pack round-trip incl. ref-deltas). The TypeScript sources stay in
the repo as the port source.

Local grade (kit `grade-local`, `--driver process`, 2026-11-18, 1 instance):

| trial | feature | agent | perf | p50 (µs) | score | replay |
|------:|--------:|------:|-----:|---------:|------:|:------:|
| 0 | 600/600 | 200/200 | 169 | 302,582 | **969** | ok |

Same-day TypeScript baseline on the same machine: 959 (perf 159,
p50 359,521 µs). A 3-instance Go run averaged 938 (90% interval 898–979)
under heavy VM contention (load ~4.6 on 1.5 shared CPUs while other agents
built in parallel), so treat local numbers as noisy — the server's
production cluster grades the commit. The entry is built and verified
only; it has NOT been submitted.

## Local grade (kit `grade-local`, `--driver process`, 2026-11-14)

| trial | feature | agent | perf | score | replay |
|------:|--------:|------:|-----:|------:|:------:|
| 0 | 600/600 | 200/200 | 176 | **976** | ok |
| 1 | 600/600 | 200/200 | 165 | **965** | ok |
| 2 | 600/600 | 200/200 | 169 | **969** | ok |

120/120 conformance checks pass on every practice trial; replay is
byte-identical (deterministic). For comparison, the dev-board leader at the
time scored 951 (600/200/151) — this entry's edge is perf (165–176 vs 151),
i.e. it answers the four timed workloads faster than the house reference.

## Run

```sh
node --experimental-strip-types main.ts
```

`main.ts` is a shim that execs the vendored Go binary `./forge`. The binary
owns the harness lifecycle (bind an ephemeral port, register with
`$MAKEFUN_ENV_URL`, poll `/done`, exit 0). To rebuild it:

```sh
cd go && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ../forge .
```

## Layout

- `main.ts` — lifecycle (starter, unchanged)
- `app.ts` — HTTP server, routing, auth, request ids
- `git.ts` — git object model, pack codec, pkt-line, reachability
- `githttp.ts` — smart-HTTP handlers
- `store.ts` — in-memory repositories, issues, PRs, labels, git objects
- `rest.ts` — REST v3 handlers
- `graphql.ts` — GraphQL parser + executor
- `mcp.ts` — MCP handlers
