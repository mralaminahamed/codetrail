<div align="center">

<img src="assets/icon-256.png" alt="codetrail icon" width="96" height="96">

# codetrail

**Ask a codebase a question. Get an answer that cites `file:line` — and a citation you can check.**

[![CI](https://github.com/mralaminahamed/codetrail/actions/workflows/ci.yml/badge.svg)](https://github.com/mralaminahamed/codetrail/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.27-00ADD8.svg?logo=go&logoColor=white)](https://go.dev/)
[![React](https://img.shields.io/badge/React-19-61DAFB.svg?logo=react&logoColor=black)](https://react.dev/)
[![Postgres](https://img.shields.io/badge/Postgres-17%20%2B%20pgvector-4169E1.svg?logo=postgresql&logoColor=white)](https://github.com/pgvector/pgvector)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

</div>

> **Status: in development. Nothing here is deployed, and most of it is not built yet.**
> The [design spec](docs/superpowers/specs/2026-08-31-codetrail-design.md) is written and approved.
> [P1](docs/superpowers/plans/2026-08-31-p1-ingestion.md) — ingestion — and
> [P2](docs/superpowers/plans/2026-09-01-p2-chunking.md) — chunking, embeddings and spans — are
> merged and green in CI. A repository now goes in and citable spans come out; **nothing retrieves
> them yet**, and the chunking experiment has not been run. This README says plainly which parts
> exist. See [Status](#status).

## What it is

Retrieval over prose is a solved-enough problem. Retrieval over **code** is not, for three reasons
this project exists to attack.

**Code does not chunk like prose.** A paragraph window cuts a function in half and staples its
second half to the top of the next one. codetrail chunks on the AST — one span per declaration —
and then *measures* that against a fixed-window baseline instead of asserting it is better. Both
arms exist and both index a real repository end to end as of P2 — in the production corpus **and**
in the doc-stripped corpus the eval indexes, which is the one §9 needs and the one the baseline arm
could not build at all until this phase's last fix. The measurement itself is
[§9 of the spec](docs/superpowers/specs/2026-08-31-codetrail-design.md#9-the-eval), it is designed
so the questions cannot be authored to flatter the chunker under test, and **it has not been run
yet**.

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

**One repository is one queued job — including while it waits to retry.** Submitting a repository
that is already `pending` or `leased` returns the existing job instead of queueing a second clone of
it, matched case-insensitively so `octocat/Spoon-Knife` and `OctoCat/spoon-knife` are one job. A job
that fails an attempt goes back to `pending` and waits out a backoff, and a waiting job is still
`pending`, so it still holds the slot: **re-submitting during the wait returns the waiting job and
does not start it any sooner**, and there is no way to force an earlier retry. With the default
three attempts the waits are 30s and then 60s. That is intended — hammering a forge that is down is
worse than waiting — but the API answers `202` with the existing job either way, so it is written
here rather than left to be inferred from a job that does not move.

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

### What it does today

All three captures below are real terminal output from the merged phases, not mockups. There is
no console yet — that is P5 — so there is no UI to screenshot, and inventing one would break the
rule at the bottom of this file.

Admission runs before anything is fetched, and a rejection names **which rule** fired:

<img src="assets/screenshots/admission.png" alt="Seven rejected URLs, each with the rule that refused it" width="880">

A submitted repository is cloned in the sandbox, recorded, and evicted when the quota is reached:

<img src="assets/screenshots/ingest.png" alt="A repository submitted, indexed, deduplicated and evicted" width="880">

And it is chunked into spans, each carrying the columns a citation is made of — path, line range,
digest, model — so the range can be checked against the file at that commit:

<img src="assets/screenshots/spans.png" alt="rs/zerolog indexed into 1303 spans, one of them checked against git show and sed" width="880">


## Chunking, and the two corpora

**One span per top-level declaration**, carrying its doc comment, with a kind (`func`, `type`,
`const`, `var`) and a symbol — a method's is receiver-qualified, so `Logger.Info` rather than
`Info`. A declaration longer than a threshold (200 lines, configurable) is cut into fixed windows
instead of becoming one useless span, and a file that is not Go — or Go that will not parse — is
windowed whole. Import declarations are dropped: an import block is not a retrievable unit, and it
is the one declaration whose text repeats across thousands of files.

**`kind=file` does not say which arm produced a row.** Sub-windowed declarations and unparseable
files carry it under the AST strategy too, so a repository of generated code produces rows
indistinguishable from the baseline arm's. The arm is a property of the run, not of the row.

**Fixed windows are a first-class strategy, not only a fallback.** `CHUNK_STRATEGY=window` chunks
the whole corpus into 40-line windows with 10 lines of overlap and never parses anything. It exists
so P6 has a baseline to compare against that was not retrofitted after the fact. **No comparison
has been run, and nothing here claims AST chunking retrieves better** — that is exactly the
question the eval exists to settle, and asserting it in a README is the failure this design is
built to avoid.

**The two corpora differ deliberately** (spec §5). The production index **keeps** doc comments,
because there they are the best retrieval signal a span has. The eval indexes the same repositories
with doc comments **stripped**, because the golden set's questions *are* that prose: leaving it in
would make every question a literal substring of its own answer and score both arms on string
overlap. Stripping blanks the comment bytes and keeps their newlines, so no line number moves and
both arms still chunk byte-identical input.

**Measured on one real repository**, `rs/zerolog` at `dfd11cca` — 99 files, 1.0 MB, picked because
it is not all Go and not one file:

| | AST arm | window arm |
| --- | --- | --- |
| spans | 1,303 (1,044 `func`, 100 `type`, 73 `file`, 67 `var`, 19 `const`) | 771, all `kind=file` |
| files with at least one span | 87 of 99 | 88 of 99 |

Embedding dominated everything else: ~147 s with `nomic-embed-text` on CPU, against 18 ms to read
and chunk the whole checkout and 1.4 s to write all 1,303 spans. Most of that write is the HNSW
index — 1,303 vectors insert in 33 ms without it and 1,031 ms with — which is not worth acting on
at this size and would be at a hundred times it. Indexing the same commit twice produced the same
1,303 span ids, which is what makes a retry safe.

**The eval's corpus is measured, not only production's.** The same commit with doc comments
stripped: the AST arm produces the same 1,303 spans, and the window arm produces **768** — three
fewer than the 771 above, because blanking `log.go`'s ~100-line package comment leaves three 40-line
windows with no word left in them. Those are dropped rather than embedded, and the job says how many
it dropped. A blank window has nothing retrievable in it; `embed.Fake` refuses a text it can hash no
token from, which is what CI runs on; and a zero vector would be worse than either, because
`'[0,0,0]'::vector <=> '[1,2,3]'::vector` is `NaN` in pgvector and would rank unpredictably instead
of failing loudly.

Two limitations this phase **measured** rather than guessed:

- **Package documentation is unretrievable under the AST strategy.** `f.Doc` is not a declaration,
  so a file holding only a package comment produces no spans at all —
  `hlog/internal/mutil/mutil.go` above is one, and the window arm covers it while the AST arm does
  not. An import-only `tools.go` has the same shape. The arms therefore do **not** cover the same
  set of files, and P6 has to account for that rather than assume it away.
- **Stripping doc comments moves AST span boundaries.** It moves no *line* — that is what blanking
  buys — but a declaration's span starts at its doc comment, and a blanked comment is no longer
  one, so 506 of those 1,303 spans start later in the stripped corpus than in the production one.
  Gold spans harvested from unstripped source would not name the stripped corpus's rows. A separate
  consequence: Go that will not parse cannot be stripped, so those files are absent from the eval
  corpus entirely rather than silently carrying their prose into it.

The knobs, all validated at boot rather than per job: `CHUNK_STRATEGY`, `CHUNK_WINDOW_LINES`,
`CHUNK_WINDOW_OVERLAP`, `CHUNK_MAX_DECL_LINES`, `STRIP_DOC_COMMENTS`, `EMBED_PROVIDER`,
`EMBED_MODEL`, `EMBED_DIM`, `EMBED_BATCH`, `OLLAMA_URL`. `EMBED_DIM` is checked against the
schema's `vector(768)` before the first job runs, because two vector spaces in one column rank
nonsense confidently and no query would look wrong. For `EMBED_PROVIDER=ollama`, "validated" costs
one embed call at startup: an address that is not one, a server that is not there and a model that
was never pulled are each a refusal to boot naming the setting, rather than a failure on the first
leased job that spends an attempt against its cap for something no retry can fix.

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
| **P2** | AST chunking, embeddings, spans, window fallback | done |
| **P3** | Retrieval, citations, extractive ask, measured floor | not started |
| **P4** | Symbol graph, per-edge provenance, graph endpoints | not started |
| **P5** | React console | not started |
| **P6** | Eval harness: generated golden set, AST versus window | not started |
| **P7** | LLM tool loop, hybrid retrieval fusion, incremental re-index | not started |
| **P8** | Terraform, CD, deploy | not started |

Nothing above is deployed. There is no live instance, no cloud account behind this repository, and
no benchmark result to quote yet — when there is one, it will come with the numbers that produced
it. The figures in [Chunking](#chunking-and-the-two-corpora) are what indexing one repository
**cost**; nothing yet says which chunking retrieves better, because nothing retrieves at all.

## Running what exists

```bash
make up      # Postgres 17 + pgvector on :55432, migrations apply on first connect
make test    # hermetic tests
make lint    # gofmt + go vet

# The tests that matter most: the ones that run against a real database. A repository
# is indexed end to end into spans here, on the deterministic fake embedder — spec §9
# requires the mechanics to be provable with no model anywhere, so the live suite
# defaults to it and refuses any other provider.
DATABASE_URL='postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable' \
  go test -tags=live ./...

# The one check a fake cannot make: that the real model returns the width the schema
# is built for. Behind its own tag, so CI never needs a model.
docker compose -f infra/docker-compose.yml --profile ai up -d ollama
docker compose -f infra/docker-compose.yml exec ollama ollama pull nomic-embed-text
OLLAMA_URL=http://localhost:11435 go test -tags=ollama ./packages/shared/embed/
```

Each live suite **creates a throwaway database of its own** beside the one `DATABASE_URL` names,
migrates it, and drops it when the suite ends. That is not tidiness: these tests clear whole tables
— eviction is a whole-corpus operation and the queue's leases are global — and doing that to the
database you pointed them at is how a corpus disappears while the suite still prints `ok`. The DSN
above is therefore only ever connected to in order to `CREATE DATABASE` and `DROP DATABASE`, which
the role it names has to be allowed to do.

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
