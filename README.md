<div align="center">

# codetrail

**Ask a codebase a question. Get an answer that cites `file:line` — and a citation you can check.**

[![CI](https://github.com/mralaminahamed/codetrail/actions/workflows/ci.yml/badge.svg)](https://github.com/mralaminahamed/codetrail/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.27-00ADD8.svg?logo=go&logoColor=white)](https://go.dev/)
[![React](https://img.shields.io/badge/React-19-61DAFB.svg?logo=react&logoColor=black)](https://react.dev/)
[![Postgres](https://img.shields.io/badge/Postgres-17%20%2B%20pgvector-4169E1.svg?logo=postgresql&logoColor=white)](https://github.com/pgvector/pgvector)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

</div>

> **Status: in development. Nothing here is deployed and most of it is not built yet.**
> The [design spec](docs/superpowers/specs/2026-08-31-codetrail-design.md) is written and approved;
> [P1](docs/superpowers/plans/2026-08-31-p1-ingestion.md) is the plan currently being implemented.
> This README describes what is being built, and says plainly which parts exist. See
> [Status](#status).

## What it is

Retrieval over prose is a solved-enough problem. Retrieval over **code** is not, for three reasons
this project exists to attack.

**Code does not chunk like prose.** A paragraph window cuts a function in half and staples its
second half to the top of the next one. codetrail chunks on the AST — one span per declaration —
and then *measures* that against a fixed-window baseline instead of asserting it is better. The
measurement is [§9 of the spec](docs/superpowers/specs/2026-08-31-codetrail-design.md#9-the-eval),
and it is designed so the questions cannot be authored to flatter the chunker under test.

**A code citation can be checked.** A prose citation can only be quoted back at you. A code
citation is `(commit, path, line range, digest)` — it either still holds what was claimed or it
does not. Every one renders as an immutable forge permalink, carries a content digest, and says so
explicitly when the ref has moved on since indexing. "Correct at commit `abc123`, which is three
months behind `main`" is a different claim from "correct", and codetrail makes the difference
visible rather than hoping you assume the generous reading.

**Retrieval is hybrid, not just vectors.** "Who calls this?" is a graph question that embeddings
answer badly. "Where is `parseConfig` defined?" is a lexical question they answer worse. So there
is a symbol graph and a lexical arm alongside the vector search, and which combination actually
helps is a question the eval settles rather than the README asserting.

### Grounded, or refused

When retrieval cannot support an answer, codetrail declines instead of writing something plausible.
The score floor that decides this is **measured** — calibrated from the eval's distribution and
recorded with the numbers that produced it — not picked because it sounded about right.

### Offline by default

The default answer is extractive: ranked spans, assembled with citation markers, no model call, no
API key, no marginal cost. Set a provider key and the same pipeline runs a bounded agentic tool loop
over `search_code`, `read_span`, `definition_of` and `callers_of` — and **the response names which
one answered it**, because a silent downgrade from a model to a fallback is the kind of failure that
costs a week before anyone notices.

## Architecture

Four apps. The split that matters is `gateway` from `indexer`.

| App | Owns | Trust |
| --- | --- | --- |
| `apps/gateway` | The HTTP API: submit a repo, poll a job, search, read spans, walk the graph, answer a question. | Public |
| `apps/indexer` | Leases jobs, clones, parses, embeds, writes, deletes the clone. | **Untrusted input** |
| `apps/console` | React 19 + TypeScript + Tailwind. Submit, watch indexing, ask, jump to source. | Browser |
| `apps/evalrunner` | The chunking experiment. Runs the *same* retriever the gateway serves from. | CLI |

The indexer is the only component that touches a stranger's URL, forks `git`, needs disk and
reaches the network. Making it a separate process means the sandbox is a **deployment boundary**
rather than a comment asking people to be careful. They also have opposite resource profiles —
spiky CPU and disk against steady memory — so they scale apart.

### One datastore

Postgres with pgvector, and no separate vector database. The questions this product answers are
partly relational — "who calls this" is a recursive CTE — and keeping the embeddings in the same
database means a similarity search and a graph hop are one query against one consistent snapshot,
rather than two systems that can disagree about what is indexed.

The job queue is a Postgres table too. One producer, durable, retryable, inspectable from `psql`.
Adding a broker to carry that would be a second thing to run and a paragraph to justify.

## Indexing a stranger's repository

codetrail accepts any public git URL through its API and runs `git clone` on it. That is the
sharpest edge in the project, and several decisions look paranoid because of it.

**Admission runs before anything is fetched.** The scheme must be `https` — `file://` alone would
turn "index a repo" into "read the indexer's disk". The host must be on an **exact** allowlist, not
a suffix match, so `github.com.evil.example` and `pages.github.com` are both refused. Credentials
and ports in the URL are refused outright. A rejection names **which rule** fired, because a generic
`400` tells an operator nothing about what to change.

**The clone is shallow, single-branch, blob-filtered**, runs in its own process group under a
wall-clock deadline so the timeout kills `git`'s children too, and has `GIT_TERMINAL_PROMPT=0` with
a neutered `GIT_ASKPASS` so a private URL fails immediately instead of blocking forever on a
credential prompt nobody is there to type. Size and file-count caps are **enforced** — a checkout
over the cap is deleted, because a cap that leaves the oversized tree on disk has not enforced
anything.

**The walker never follows a symlink.** A repository can contain `link -> /etc/passwd`, and a naive
walk reads and indexes it. Anything that is not a regular file is skipped outright. This is a
vulnerability, not a hardening nicety, and the guards are mutation-checked rather than assumed:
removing `O_NOFOLLOW` fails two tests, and removing all three of the link, type and fstat guards
fails ten. Every symlink, fifo and device fixture is built at runtime — nothing symlink-shaped is
committed, because checking this repository out should not require any.

**What the allowlist does not buy.** Host-allowlisting is the SSRF control. It does **not** defend
against a hostile allowlisted forge, and codetrail claims no DNS-rebinding protection: `git` is a
subprocess and cannot be handed a validating dialer. That is a real limitation and it is written
here rather than left for someone to discover.

**Configuring the allowlist.** `ALLOWED_HOSTS` is a comma-separated list of exact hosts, and it
replaces the default (`github.com`, `codeberg.org`) rather than extending it. One limitation to
know before setting it: an accepted path is exactly `/owner/name`, so a forge that nests namespaces
deeper is only half served — adding `gitlab.com` accepts `group/repo` and refuses
`group/subgroup/repo`.

**The corpus does not grow without bound.** Hard admission caps plus LRU eviction on last-queried
time. Anyone may submit; a popular repository stays warm; the least recently queried one goes when
the quota is reached, in a single `DELETE` that cascades. The `jobs` table is the exception and is
deliberately not bounded yet: re-submitting a repository that has finished appends a row, and how
much of that history is worth keeping is a decision that belongs with the P3 read endpoints.

## The symbol graph, and its honesty

A precise Go call graph needs type information, which needs the repository to actually compile —
right Go version, dependencies downloadable. Plenty of public repositories will not, in a sandbox.

So codetrail tries `go/packages` with full type information and falls back to syntactic, name-based
edges when that fails — and **labels every edge with which one it got**. A `resolved` edge means
*this exact symbol*. A `syntactic` edge means *something named `Close`*, and its target is recorded
as null rather than as a guess.

The label is **per-edge, not per-repository**: a repository where three packages type-check and two
do not gets precise edges for the three and honest approximations for the two, rather than being
downgraded wholesale.

## Status

| Phase | Delivers | State |
| --- | --- | --- |
| **P0** | Skeleton, schema, migrations, compose, CI with live Postgres | done; CI landed with P1 |
| **P1** | Ingestion: admission, sandbox, job queue, caps, LRU eviction | done |
| **P2** | AST chunking, embeddings, spans, window fallback | not started |
| **P3** | Retrieval, citations, extractive ask, measured floor | not started |
| **P4** | Symbol graph, per-edge provenance, graph endpoints | not started |
| **P5** | React console | not started |
| **P6** | Eval harness: generated golden set, AST versus window | not started |
| **P7** | LLM tool loop, hybrid retrieval fusion, incremental re-index | not started |
| **P8** | Terraform, CD, deploy | not started |

Nothing above is deployed. There is no live instance, no cloud account behind this repository, and
no benchmark result to quote yet — when there is one, it will come with the numbers that produced
it.

## Running what exists

```bash
make up      # Postgres 17 + pgvector on :55432, migrations apply on first connect
make test    # hermetic tests
make lint    # gofmt + go vet

# The tests that matter most: the ones that run against a real database.
DATABASE_URL='postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable' \
  go test -tags=live ./packages/shared/...
```

`make psql` opens a shell against the running database.

## How this is built

Design first, then a plan, then code — in that order, which was not true of the first commit and is
said here rather than quietly fixed.

- [`docs/superpowers/specs/`](docs/superpowers/specs/) — the design, with the reasoning behind each
  decision and an explicit list of what was deferred and why.
- [`docs/superpowers/plans/`](docs/superpowers/plans/) — per-phase implementation plans, task by
  task, each with its tests written before its implementation.

Two conventions worth knowing if you read the commits:

**Every load-bearing test is mutation-tested.** The behaviour it pins is deliberately broken, and
the test must fail. A test that cannot be broken on purpose is not evidence, and the plans list the
specific mutations for each task.

**Documentation understates rather than overstates.** If something has not been run, it says so in
those words. An overclaim in a README is treated as a defect and fixed like one.

## Portfolio sibling

Alongside [sitemon](https://github.com/mralaminahamed/sitemon) (event-driven monitoring, Go + NATS +
React) and [flagcast](https://github.com/mralaminahamed/flagcast) (feature flags, Go + gRPC +
React). codetrail adds static analysis, a relational graph over code, and a retrieval experiment
with a measurable answer.

## License

[MIT](LICENSE)
