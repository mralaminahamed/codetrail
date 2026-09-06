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

codetrail indexes public Git repositories and answers questions about them, citing `file:line`. The
distinctive part is that the citation is **checkable**. Every one is the tuple
`(repo, commit, path, startLine, endLine, digest)`, rendered as an immutable forge permalink, and the
console prints the one-line command that hashes exactly those bytes out of a fresh clone:

```
git show dfd11cca1143ba03ba0fc0ff14e5dbb4d61f6f0a:log.go | sed -n '126,127p' | head -c -1 | sha256sum
961801a7ea0d9b429f8fa020bfc8423f1a8097fa69e611706f9305c3b2e13b8c  -
```

If that digest matches the one in the answer, the lines the answer points at are the lines that were
indexed. If it does not, the citation is wrong and you can see that it is wrong. A prose citation can
only be quoted back at you; a code citation either still holds what was claimed or it does not.

Answers are extractive by default — ranked spans assembled with citation markers, no API key, no
per-request vendor cost. Set a provider key and the same pipeline runs a bounded tool loop over
`search_code`, `read_span`, `definition_of` and `callers_of`, and every response names which path
produced it. When retrieval returns nothing worth answering from, codetrail **refuses**: a `200`
carrying `"refused": true` and a reason from a closed set, never an HTTP error, because filing "we
had nothing to say" inside an error-rate panel is how a dashboard starts lying.

## Contents

- [Status](#status) — what is true, and what is not
- [Quickstart](#quickstart) — clone, index a repository, ask a question, check the citation
- [API reference](#api-reference) — thirteen routes, their shapes and their failures
- [Configuration](#configuration) — every environment variable, and what is not configurable
- [Architecture](#architecture) — four apps, one datastore, and the boundary that matters
- [How it works](#how-it-works) — chunking, citations, retrieval, the symbol graph, the answering loop
- [The console](#the-console)
- [Deployment](#deployment) — what exists, and what has never been run
- [Development](#development) — build, test, and the four build tags

## Status

Everything described in this file is built, merged and green in CI. **Nothing is deployed.** The list
below is what that leaves untrue, stated once here so no section has to keep apologising.

- **There is no live instance.** No AWS account stands behind this repository, no OIDC role, no state
  bucket, no `production` environment. `terraform apply` has never run, no image has ever been
  pushed, and the deploy workflow has never been dispatched. What can be proved without an account is
  proved on every pull request; see [Deployment](#deployment).
- **The score floor is a mechanism, not a measured threshold.** `ANSWER_SCORE_FLOOR` defaults to `-1`
  — the bottom of the cosine range, which excludes nothing — and every answer carries
  `"floor": {"value": -1, "calibrated": false}`. Nobody has measured a threshold. One consequence to
  know before reading the refusal reasons: at the default, `below_floor` is unreachable, because a
  cosine similarity is never below `-1`. Setting the knob does not mark it calibrated — the flag
  means *codetrail* measured the number — and a test fails the build if any shipped surface says
  otherwise.
- **The eval ran on one corpus:** `google/uuid` at `2d3c2a9`, 74 mechanically generated cases. It
  settled two questions and left the rest open; [Retrieval](#retrieval) says exactly how narrow each
  answer is. Two of the three corpora named before the run were refused by the leakage probe.
- **No paid LLM request has ever been made from this repository.** The tool loop is proved against a
  deterministic in-process fake and against `httptest` servers on loopback. A `-tags=llm` suite
  exists and CI type-checks it; it has never been run. Every cost figure here is arithmetic over a
  published price list, not a bill.
- **`POST /api/repos` is unauthenticated and there is no rate limit on any route.** What it does is
  `git clone` a stranger's URL on your bill. Per-job caps bound one job; nothing bounds the arrival
  rate. With an LLM provider configured, any caller may also request the paid path.
- **The console screenshots in `assets/screenshots/` are stale** — they were taken before the console
  had any styling, and the shipped console does not look like them. None of them is shown in this
  file. Regenerating them needs a browser against a running stack. The terminal captures below are
  real runs against a live gateway, a live indexer and a real Postgres.
- **Roughly one call edge in eight resolves to a definition** — 945 of 7,895 on `rs/zerolog`. That is
  a measurement of what an honest label looks like on a repository a sandbox will not build, not a
  target that was missed. See [The symbol graph](#the-symbol-graph).

Two conventions this repository holds itself to, and which this file is written under: every
load-bearing test is mutation-tested — the behaviour it pins is deliberately broken and the test must
fail — and documentation understates rather than overstates. **An overclaim in a README is treated as
a defect and fixed like one.** If something has not been run, it says so in those words.

## Quickstart

You need **Docker with Compose v2**, **Go 1.27** and — for the console — **Node 22**. There is no
hosted instance to try; this is the whole path.

**1. Clone and start Postgres.**

```bash
git clone https://github.com/mralaminahamed/codetrail.git
cd codetrail
make up
```

`make up` starts one container: Postgres 17 with pgvector, on **`localhost:55432`**. Nothing else
comes up by default. Migrations are embedded in the binaries and run at boot — there is no migrate
step and no down migrations.

**2. Start the embedder and pull the model.**

```bash
docker compose -f infra/docker-compose.yml --profile ai up -d ollama
docker compose -f infra/docker-compose.yml exec ollama ollama pull nomic-embed-text
```

`nomic-embed-text` is about 274 MB and produces the 768-dimension vectors the schema is built for, on
**`localhost:11435`**. To see the mechanics without a model, skip this step and use
`EMBED_PROVIDER=fake` below: retrieval will be meaningless but everything else works, which is what
the test suite runs on.

**3. Build and run the two binaries.**

```bash
make build
export DATABASE_URL='postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable'
export EMBED_PROVIDER=ollama OLLAMA_URL=http://localhost:11435
./bin/gateway &   # :8080
./bin/indexer &   # probe and metrics on :9090
```

The gateway **embeds the question**, so it depends on the embedder at boot: with
`EMBED_PROVIDER=ollama` and Ollama down the gateway does not start, and submission and job polling go
down with retrieval. That parity is chosen over booting into a mode where retrieval `503`s and
submission works, because a degraded mode nobody can see is the silent downgrade this design exists to
avoid.

**4. Index a repository.**

```bash
JOB=$(curl -s -XPOST localhost:8080/api/repos \
  -H 'content-type: application/json' \
  -d '{"remote":"https://github.com/rs/zerolog","ref":"master"}' | jq -r .id)
```

You get a `202` and a job in `pending`. The `content-type` header is not optional on any `POST`:
without it the body binds as a form and you get a `400` naming that rule.

**5. Poll until it is done.**

```bash
curl -s localhost:8080/api/jobs/$JOB | jq .
REPO=$(curl -s localhost:8080/api/jobs/$JOB | jq -r .repo_id)
```

`status` walks `pending` → `leased` → `done`, and `repo_id` is empty until it lands. `rs/zerolog` is
99 files and takes roughly two and a half minutes, **nearly all of it embedding on CPU**. A failed
attempt goes back to `pending` and waits out a backoff of 30s, then 60s.

**6. Ask something.**

```bash
curl -s -XPOST localhost:8080/api/repos/$REPO/ask \
  -H 'content-type: application/json' \
  -d '{"q":"how does the sampler decide to drop an event"}' | jq .
```

You get a `200` with `answer`, `citations`, `answered_by: "extractive"` and a `floor` block — or a
`200` with `refused: true` and a reason. A refusal is an outcome, not an error.

**7. Check a citation.** Take any citation's `commit`, `path`, `start_line`, `end_line` and `digest`,
and against a fresh clone of that repository:

```bash
git show <commit>:<path> | sed -n '<start>,<end>p' | head -c -1 | sha256sum
```

It should print the `digest`. `head -c -1` is not decoration — a span's text ends at the last byte of
its last line, not at the newline after it, so without it you hash one byte more than the digest
covers and get a different hash. The check is only a check if it fails when it should.

**8. Walk the graph.** Three `GET`s, because a symbol name is an identifier rather than prose:

```bash
curl -s "localhost:8080/api/repos/$REPO/symbols?name=Event.Msg" | jq .
SYM=$(curl -s "localhost:8080/api/repos/$REPO/symbols?name=Event.Msg" | jq -r .symbols[0].id)
curl -s "localhost:8080/api/repos/$REPO/symbols/$SYM" | jq .
curl -s "localhost:8080/api/repos/$REPO/symbols/$SYM/callers?depth=2&limit=10" | jq .
```

The indexer needs a `go` binary on `PATH` for any edge to say `resolved`. Without one it still
indexes, every edge is `syntactic`, and it says so once at boot.

**9. Optionally, the console.**

```bash
cd apps/console && npm ci && npm run dev   # http://localhost:5173
```

Vite proxies `/api`, `/health`, `/ready` and `/metrics` to `localhost:8080` in both `dev` and
`preview`, so the browser is same-origin against the gateway. There is no build-time API URL.

## API reference

Thirteen routes, and nothing else is served.

### Conventions

**There is no authentication on any route, and no rate limit on any route.** Nothing here checks a
credential. `POST /api/repos` runs `git clone` against a URL a stranger supplied, on your bill; the
per-job caps bound one job and nothing bounds the arrival rate.

**No CORS headers are set, on purpose.** `Access-Control-Allow-Origin` on an unauthenticated API that
clones a stranger's URL would let any page on the internet drive ingestion from a visitor's browser.
The console is same-origin with the gateway by construction: its API base is the relative `/api` and
there is no build-time URL.

| | |
| --- | --- |
| Request body limit | 2 MB on every route, on `Content-Length` and on bytes read. Over it is a `413`. |
| Request id | `X-Request-Id` is honoured inbound and always set outbound. It is the field a `500` body carries. |
| Panics | Recovered, and answered `500`. |
| Request timeout | **None.** There is no request-timeout middleware. The query embed has its own 15 s budget and the LLM loop its own 60 s deadline. |
| Access logging | None, deliberately: the question is never written to any log line, and the indexer's `git` stderr is never surfaced. |
| Content type | `application/json` is required on `POST`. Without the header the body binds as a form and its fields come out empty, which surfaces as a `400` naming the rule that failed. |

**Error bodies.** A `4xx` from a handler is `{"error": "<what to change>", "rule": "form"}` — the
`rule` on a read-path `400` is always literally `form`; on `POST /api/repos` it is the admission rule
that fired, one of `form`, `scheme`, `host`. A `500` is
`{"error": "internal error", "request_id": "<id>"}` and never carries an internal message.

**Search and ask are `POST` with the question in the body on purpose.** A question in a query string
is logged by every proxy, load balancer and access log between the caller and the process, and it is
the one string here that must not be.

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/api/repos` | Submit a repository for indexing |
| `GET` | `/api/jobs/:id` | Poll a job |
| `GET` | `/api/repos` | List the corpus |
| `GET` | `/api/repos/:repo` | One repository, its counts and its staleness |
| `GET` | `/api/repos/:repo/spans/:span` | One span and its citation |
| `POST` | `/api/repos/:repo/search` | Rank spans against a query |
| `POST` | `/api/repos/:repo/ask` | Answer a question, or refuse |
| `GET` | `/api/repos/:repo/symbols` | Look a symbol up by name |
| `GET` | `/api/repos/:repo/symbols/:symbol` | One symbol and its citation |
| `GET` | `/api/repos/:repo/symbols/:symbol/callers` | Who calls it, precisely and approximately |
| `GET` | `/health` | Liveness. Always `200` while the process is listening. |
| `GET` | `/ready` | Readiness. `200` or `503`, empty body. |
| `GET` | `/metrics` | Prometheus text exposition. No auth. |

Every repository-scoped route shares three outcomes: **`404`** `{"error":"no such repository"}`,
**`410`** `{"error":"this repository was indexed and has since been evicted"}` while a tombstone
survives, and **`500`** on a store failure. The `410` is the point of tombstones: eviction is one
`DELETE` that cascades, after which nothing would distinguish an evicted repository from one nobody
ever submitted.

### `POST /api/repos`

`{"remote": <string, required>, "ref": <string, default "HEAD">}`

`remote` is admitted in order: non-empty, parseable, scheme exactly `https`, no userinfo, no port,
host on the exact allowlist, path exactly `/owner/name` with a `.git` suffix stripped, and both
segments matching `[A-Za-z0-9._-]+` and not `.` or `..`. `ref` refuses `""`, `.`, anything over 255
characters, a leading `-`, any `..`, and anything outside `[A-Za-z0-9._/-]`.

**`202`** with the job — `id`, `remote`, `ref`, `status`, and `repo_id` once it lands. Submitting a
repository that already has a `pending` or `leased` job returns that job rather than queueing a
second clone. `status` is one of `pending`, `leased`, `done`, `failed`.

**The job's `attempts` and `error` are deliberately not projected.** The indexer's stderr has carried
a server filesystem path and a credential-bearing URL, so codetrail does not report why a job failed.
That is a real gap, not an oversight.

**`400`** naming the rule that fired:
`{"error":"github.com.evil.example is not an allowed host","rule":"host"}`,
`{"error":"scheme \"http\"; only https is accepted","rule":"scheme"}`,
`{"error":"credentials in the URL","rule":"form"}`,
`{"error":"ref must be a plain git ref name","rule":"form"}`,
`{"error":"malformed request body","rule":"form"}`.

### `GET /api/jobs/:id`

**`200`** with the same job shape. **`404`** `{"error":"no such job"}`. A terminal job stays pollable
for `JOB_HISTORY_HOURS`.

### `GET /api/repos`

`?limit=` defaults to `20`, refused outside 1..50 with a `400` naming the bound rather than clamped.

**`200`** `{"repos": [...], "count": n}`, each entry `id`, `remote`, `ref`, `commit`, `indexed_at`,
`last_used_at`. `repos` is always `[]`, never `null` — as is every list in every response on every
route.

### `GET /api/repos/:repo`

**`200`** with `id`, `remote`, `ref`, `commit`, `indexed_at`, the counts `files`, `spans`,
`files_with_spans`, `symbols`, `edges`, `edges_resolved`, `edges_syntactic`, and `staleness`.

`staleness` is `{state, forge_checked, indexed_at, newer_commit?, newer_indexed_at?, note}`. `state`
is `unknown` or `superseded`. **`forge_checked` is always `false`** — it is the field that stops
`note` being read as a freshness guarantee. `note` is prose, and its unknown form is exactly:

> Correct at commit 2d3c2a9, indexed 1 day ago. codetrail has not checked whether HEAD has moved
> since.

Ages are coarse — `less than an hour`, then hours, then days — because a finer unit would imply a
freshness check that did not happen.

### `GET /api/repos/:repo/spans/:span`

**`200`** `{"span": …, "citation": …}`. The span is `id`, `path`, `kind`, `symbol`, `start_line`,
`end_line`, `text`; `kind` is one of `func`, `type`, `const`, `var`, `file`, and line numbers are
1-based and inclusive at both ends.

The citation is the full tuple — `repo_id`, `remote`, `commit`, `ref`, `path`, `start_line`,
`end_line`, `digest` — plus `permalink` and `staleness`. **`permalink` is the empty string** for a
forge whose URL shape codetrail does not know. **`404`**
`{"error":"no such span in this repository"}`.

### `POST /api/repos/:repo/search`

| Field | Type | Default | Notes |
| --- | --- | --- | --- |
| `q` | string | — | Required. 1..1000 bytes after trimming, and must contain at least one letter or digit. |
| `limit` | int | `10` | 1..50. An explicit `0` is a `400`, not the default. |
| `mode`, `k`, `w_vector`, `w_lexical`, `answerer` | — | — | **Declared only so they can be refused.** Present at all is a `400`. |

`mode` is a **response** field, not a request one: retrieval is configured once per process by
`RETRIEVAL_MODE`, so a body that named one would change nothing while looking as though it had —
including when it names the mode the process happens to run, which is not something a caller can
know. `k`, `w_vector` and `w_lexical` are refused for the same reason; `answerer` is refused here
because search ranks spans and writes no answer.

**`200`** `{repo_id, mode, top_score, count, hits}`. `top_score` is `null` in lexical-only mode and
on an empty result. Each hit carries `span_id`, `path`, `kind`, `symbol`, `start_line`, `end_line`,
`text`, `score`, `vector_score`, `vector_rank`, `lexical_rank`, `citation`. `vector_score` is the
cosine similarity, or `null` when the vector arm did not return that span; `vector_rank` and
`lexical_rank` are `0` when their arm did not. `score` is the fused rank score and **carries no
quality at all** — the top hit of any non-empty result scores `1/(k+1)`.

There is **no `floor` field on `/search`**: the floor is the answer's decision, and reporting one
here would imply a filter that did not run. An empty corpus is a `200` with `hits: []`, not an error;
a corpus indexed by a different embedder is a `500`, not a refusal.

### `POST /api/repos/:repo/ask`

The same body, with two differences: `limit` defaults to `ANSWER_MAX_SPANS` (**5**), because the span
budget *is* the retrieval depth an answer can hold; and `answerer` is honoured — `extractive` or
`llm`, defaulting to `ANSWER_DEFAULT`. Asking for `llm` when `LLM_PROVIDER=none` is a **`400`**
naming that, deliberately rather than a silent fall back to extractive.

There are three success shapes and **all three are `200`**.

**Answered.** `refused` is `false`; `answered_by` is `extractive` or `llm`; `answer` is the prose,
`citations` the resolved markers, `dropped` the count of spans that did not fit the character budget.
A span is never truncated — whole spans are dropped and counted.

```json
{ "repo_id": "9f2c1a7b3d5e4088",
  "refused": false,
  "answered_by": "extractive",
  "answer": "[1] uuid.go:71-128 (Parse)\nfunc Parse(s string) (UUID, error) {\n\t...\n}",
  "citations": [ { "marker": 1, "span_id": "a41c9e0b7d2f", "kind": "func", "symbol": "Parse",
                   "citation": { "…": "the tuple, its permalink and its staleness" } } ],
  "dropped": 2,
  "mode": "vector",
  "top_score": 0.7841,
  "floor": { "value": -1, "calibrated": false, "applicable": true } }
```

**Degraded.** `answered_by` falls back to `extractive` and a `degraded` block names what went wrong.
It is present only when the loop was actually attempted and failed — never for a deployment with no
provider, and never for a caller who asked for extractive.

```json
{ "answered_by": "extractive",
  "degraded": { "from": "llm", "reason": "step_limit" },
  "llm": { "model": "…", "steps": 6, "tool_calls": 9, "stop": "step_limit",
           "tools": [ {"name":"search_code","ms":142} ],
           "usage": { "input_tokens": 11200, "output_tokens": 900, "estimated": false },
           "citations_dropped": 0 } }
```

The `llm` block is present whenever the loop ran, degradation included. Its `tools` list is ordered
and carries names and durations — **never arguments**, because an argument is derived from the
question.

**Refused.** `refused` is `true`, there is no `answer` and no `citations`, and `reason` is one of
`no_spans`, `below_floor`, `unscored` with a `detail` sentence for a reader.

```json
{ "repo_id": "9f2c1a7b3d5e4088",
  "refused": true,
  "answered_by": "extractive",
  "reason": "no_spans",
  "detail": "Nothing in this repository's index matched the question.",
  "mode": "vector",
  "top_score": null,
  "floor": { "value": -1, "calibrated": false, "applicable": true } }
```

`floor.applicable` is `false` in `lexical` mode, where there is no cosine similarity to compare
against. `floor.calibrated` is `false` in the shipped build and stays `false` even when an operator
sets a value. **At the default floor of `-1`, `below_floor` cannot be reached**; the reachable
refusals are `no_spans` and `unscored`. The `detail` for a below-floor refusal names the floor and
says which kind it is, derived from `calibrated` rather than written down: at an unmeasured floor it
reads *"The best match scored under the configured floor of 0.4. That floor is a mechanism, not a
measured threshold: no evaluation has chosen this number, so it has filtered nothing."*

### `GET /api/repos/:repo/symbols`

`name` (required, non-empty after trimming), `suffix` (bool, default `false`, match a method by its
last segment), `pkg` (default any), `limit` (default `20`, 1..50).

**`200`** `{repo_id, count, matched, truncated, symbols, staleness}`. `matched` is `exact` or
`suffix` and is derived from the flag that reached the store, so a widening is never silent. Each
symbol is `id`, `name`, `pkg`, `kind`, `path`, `start_line`, `end_line`, `span_id` — and **`span_id`
keeps its key when empty**, so a client can tell "this declaration has no span" from "the field is
gone".

`pkg` is what makes this route usable on a repository where the same method name appears in several
packages. The console's API client serialises it; no console view passes one yet, so today it is an
API affordance rather than a shipped feature.

### `GET /api/repos/:repo/symbols/:symbol`

**`200`** `{symbol, citation, staleness}`. `citation` is **`null`** when the symbol has no span, and
`staleness` sits beside it rather than inside it so the claim survives a null citation. **`404`**
`{"error":"no such symbol in this repository"}`.

### `GET /api/repos/:repo/symbols/:symbol/callers`

`?depth=` defaults to `1` and is refused outside **1..3**; `?limit=` defaults to `20`, 1..50. Both
are refused rather than clamped, because a caller asking for depth 40 has misunderstood the endpoint
and quietly serving 3 hides that.

The ceiling of 3 was lowered from 5 and measured: fan-out in a call graph is multiplicative and the
depth bound is the only thing that caps it. `EXPLAIN (ANALYZE)` over 20 mutually-calling symbols
walks 1,494,559 rows at depth 5 before `LIMIT` sees any of them, and 30 symbols at depth 4 exceeds
the statement timeout; at depth 3 the same graphs answer in 79 ms and 1.5 s. Three is also what the
model's `callers_of` tool is given, so the endpoint and the tool bound the same walk the same way.

**`200`** `{repo_id, symbol, depth, truncated, callers, approximate}`. **The two lists are never
merged.**

- `callers` are the **resolved** ones: the calling symbol, its `depth`, `provenance` (always
  `resolved` here), the `call` site as `{path, line}`, and a citation.
- `approximate` is `{matched_on, count, truncated, failed, callers}`. Its entries carry `to_name` and
  `provenance: "syntactic"` and **no `depth`**, because a name match is depth-1 by construction.
- `approximate.failed` is `true` when only that query failed. The precise answer still stands and the
  request is still a `200`.

A symbol nothing calls is a `200` with two empty lists. A symbol that does not exist is a `404`.

### The closed sets

Every one of these is a metric label as well as a response field, which is why none of them can take
an arbitrary value.

| Set | Values |
| --- | --- |
| job `status` | `pending`, `leased`, `done`, `failed` |
| admission `rule` | `form`, `scheme`, `host` |
| span `kind` | `func`, `type`, `const`, `var`, `file` |
| edge `provenance` | `resolved`, `syntactic` |
| retrieval `mode` | `vector`, `lexical`, `hybrid` |
| `answered_by` | `extractive`, `llm` |
| refusal `reason` | `no_spans`, `below_floor`, `unscored` |
| `staleness.state` | `unknown`, `superseded` |
| symbol `matched` | `exact`, `suffix` |
| `degraded.reason` | the fourteen listed under [the answering loop](#the-answering-loop) |

## Configuration

Every environment variable the code reads, with the default it reads when the variable is unset.
There is no configuration file; a knob not on this list does not exist. The only command-line flags
are `-probe` on both service binaries — the container healthcheck, which GETs the process's own
`/health` and exits `0` or `1` — and the eval runner's own flags.

Almost every knob below is **validated at boot**, and a value that does not parse or falls outside
its range is a refusal to start naming the setting and the value it was given, not a silent fall back
to the default. Three exceptions are worth knowing:

- **`PORT` is not validated.** A bad value boots the gateway, logs `gateway up`, and *then* dies on
  the listener.
- **`PROBE_PORT` is not validated and its bind failure is not fatal.** The indexer goes on indexing
  with no `/health`, `/ready` or `/metrics`.
- **The `LLM_*` knobs are read only when a provider is configured**, and `EMBED_MODEL` and
  `OLLAMA_URL` only under `EMBED_PROVIDER=ollama`. At the defaults, a malformed value in one of those
  is ignored rather than refused.

### Storage and embedding

Read by the gateway and the indexer. The eval runner reads the four embedding knobs too, but takes
its databases from `-ast-dsn` and `-window-dsn` rather than `DATABASE_URL`.

| Knob | Default | What it does |
| --- | --- | --- |
| `DATABASE_URL` | `postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable` | Postgres. Migrations run at boot on both binaries. |
| `ALLOWED_HOSTS` | `github.com,codeberg.org` | Exact-host clone allowlist, **replacing** the default rather than extending it. Set to the empty string it admits nothing, which is a usable way to freeze ingestion. The entries themselves are never validated. |
| `EMBED_PROVIDER` | `ollama` | `ollama` or `fake`. The gateway needs it to embed the question; the live test suite refuses anything but `fake`. |
| `EMBED_MODEL` | `nomic-embed-text` | Written into `spans.embed_model`. A corpus indexed by another model is an error, not a low score. |
| `EMBED_DIM` | `768` | Checked against the schema's `vector(768)` before anything else is built. Two vector spaces in one column rank nonsense confidently and no query would look wrong. |
| `OLLAMA_URL` | `http://localhost:11435` | Probed once at boot with a real embed call: an address that is not one, a server that is not there and a model that was never pulled are each a refusal to boot naming the setting, rather than a failure on the first leased job. |

### Gateway

The eval runner reads the retrieval and answer knobs from this table too, with the same names,
defaults and refusals, because it runs the same retriever the gateway serves from.

| Knob | Default | What it does |
| --- | --- | --- |
| `PORT` | `8080` | The API listener. Not validated. |
| `RETRIEVAL_MODE` | `vector` | `vector`, `lexical` or `hybrid`. Not a request field. |
| `RETRIEVAL_RRF_K` | `60` | Fusion's rank discount. From the paper the method comes from, not measured here. |
| `RETRIEVAL_CANDIDATES` | `40` | Per-arm search depth. 40 because that is pgvector 0.8.6's own `hnsw.ef_search` default on the pinned image, read from `pg_settings.boot_val` rather than assumed: asking the ANN index for more rows than `ef_search` degrades recall with no error. |
| `LEXICAL_SPLIT_IDENTIFIERS` | `true` | Adds a query's camel-case parts to its lexical terms. |
| `ANSWER_SCORE_FLOOR` | `-1` | The refusal threshold. Refused outside `[-1, 1]` or as NaN. Setting it never marks it calibrated: the flag means *codetrail* measured the number. |
| `ANSWER_MAX_SPANS` | `5` | Spans one answer may hold. Also `/ask`'s default `limit`. |
| `ANSWER_MAX_CHARS` | `8000` | Characters one answer may hold. Whole spans are dropped to fit, never truncated. |
| `ANSWER_DEFAULT` | `extractive` | `extractive` or `llm`. What a request naming no `answerer` gets, **even when a provider is configured**. `llm` with `LLM_PROVIDER=none` is a refusal to boot. |
| `LLM_PROVIDER` | `none` | `none`, `fake` or `anthropic`. At `none` no client is built and no key is read. |
| `LLM_MODEL` | `claude-opus-5` | Checked non-empty only. There is no boot probe, so a wrong model surfaces on the first paid request. |
| `LLM_BASE_URL` | `https://api.anthropic.com` | `https` only, against a compiled-in one-entry host allowlist. |
| `LLM_API_KEY_FILE` / `LLM_API_KEY` | unset | The file wins and is read once at boot. Never a `.env`. |
| `LLM_MAX_CONCURRENT` | `2` | A semaphore that degrades with `busy` rather than queueing. |
| `LLM_TOKENS_PER_HOUR` | `200000` | A rolling in-memory budget, **per process**. |
| `LLM_BELOW_FLOOR` | `false` | Whether a below-floor retrieval may still spend a model call. At the default floor this guards an unreachable branch. |

### Indexer

The eval runner reads the chunking knobs from this table, except `CHUNK_STRATEGY`, which it sets per
arm rather than from the environment.

| Knob | Default | What it does |
| --- | --- | --- |
| `PROBE_PORT` | `9090` | `/health`, `/ready` and `/metrics`. Deliberately not `PORT`: a copy-pasted task definition would otherwise serve the wrong process on the right port. Not validated, and a bind failure is logged rather than fatal. |
| `SCRATCH_DIR` | `$TMPDIR/codetrail` | Where clones land, and the only writable path in the image. Two workers must not share one. |
| `MAX_REPO_BYTES` | `268435456` (256 MB) | Cap on the clone. The `go` command's caches are outside it. |
| `MAX_REPO_FILES` | `20000` | Cap on the walk. |
| `MAX_FILE_BYTES` | `1048576` (1 MB) | Cap on one read. |
| `JOB_DEADLINE_SECONDS` | `600` | One wall-clock budget for the **whole** job, type-check included. |
| `MAX_ATTEMPTS` | `3` | Attempts before a job is terminal. |
| `POLL_SECONDS` | `2` | Lease poll interval. |
| `KEEP_REPOS` | `50` | Corpus size, enforced by LRU eviction on last-queried time. |
| `KEEP_TOMBSTONES` | `500` | How many evicted repositories still answer `410` instead of `404`. |
| `JOB_HISTORY_HOURS` | `168` | How long a terminal job stays pollable. |
| `JOB_SWEEP_MINUTES` | `60` | How often that window is enforced. |
| `EMBED_BATCH` | `32` | Span texts per `Embed` call. |
| `TYPECHECK` | `true` | The graph stage's kill switch. Off means every edge is `syntactic`. |
| `TYPECHECK_GOPROXY` | `off` | `off`, or `https://` proxy elements an operator trusts. `direct` and any `,direct` fallback are refused at boot. This is the whole network posture of the type-check. |
| `REINDEX_SKIP_CLONE` | `true` | Resolve the ref first and complete without cloning when that commit is already indexed. |
| `REINDEX_REUSE` | `true` | Reuse an existing embedding keyed on `(digest, embed_model, embed_dim)`. |
| `CHUNK_STRATEGY` | `ast` | `ast` or `window`. A property of the run, never of a row. |
| `CHUNK_WINDOW_LINES` | `40` | Window size for the baseline strategy and for the AST strategy's fallbacks. |
| `CHUNK_WINDOW_OVERLAP` | `10` | Window overlap. Must be less than `CHUNK_WINDOW_LINES`. |
| `CHUNK_MAX_DECL_LINES` | `200` | Above this a declaration is sub-windowed instead of becoming one useless span. |
| `STRIP_DOC_COMMENTS` | `false` | Builds the eval corpus. Blanks comment bytes and keeps their newlines, so no line number moves. |

Both re-index knobs default to `true`, so a deployment that has never heard of them is running both.
`REINDEX_SKIP_CLONE=false` disarms the fast path outright, or runs the counterfactual that shows what
it saved.

### What is not configurable

Named here because an operator will look for a knob and there is not one.

| | |
| --- | --- |
| The LLM host allowlist | exactly one entry, `api.anthropic.com`. A widening knob would be the hole the control exists to close. |
| The loop's bounds | `MaxSteps` 6, `MaxToolCalls` 12, `MaxInputTokens` 12,000, `MaxOutputTokens` 1,500, `Deadline` 60 s, `MaxRepeats` 2, `MaxToolErrors` 2. |
| The tools' limits | `MaxHits` 5, `MaxSpanChars` 32,000 cumulative, `MaxDefinitions` 10, `MaxCallers` 20, `MaxDepth` 3. |
| The per-arm fusion weights | both `1`. Only `RETRIEVAL_RRF_K` and `RETRIEVAL_CANDIDATES` are settable. |
| The embedding dimension | `768`, fixed by the schema. `EMBED_DIM` is checked *against* it and never sets it. |
| Timeouts | provider client 90 s, query embed 15 s, embedder boot probe 30 s, gateway shutdown 10 s. |

**Tooling, not deployment knobs:** `CODETRAIL_TFPLAN_JSON` points the policy tests at a
`terraform show -json` file; `UPDATE_CONSOLE_FIXTURES` lets the live suite rewrite the console's
committed fixtures; `CI` turns a skipped live suite into a hard failure when `DATABASE_URL` is unset.

## Architecture

Four apps and one datastore.

| App | Owns | Trust |
| --- | --- | --- |
| `apps/gateway` | The HTTP API: submit a repository, poll a job, search, read spans, walk the graph, answer a question. | Public |
| `apps/indexer` | Leases jobs, clones, parses, embeds, writes, deletes the clone. | **Untrusted input** |
| `apps/console` | React 19 + TypeScript + Tailwind. Submit, watch indexing, ask, jump to source. | Browser |
| `apps/evalrunner` | The retrieval and chunking experiments. Runs the *same* retriever the gateway serves from. | CLI |

### Why the indexer is a separate process

The indexer is the only component that touches **a stranger's URL**, forks `git` and needs disk.
Splitting it out makes the sandbox a **deployment boundary** rather than a comment asking people to
be careful: in the Terraform it is separate ECS services under `awsvpc`, so separate ENIs and
separate security groups, with the indexer behind no load balancer, accepting no connection but a
metrics scrape, and holding no IAM task role. Reviewing that boundary is reviewing a set of
resources. The two also have opposite resource profiles — spiky CPU and disk against steady memory —
so they scale apart.

The boundary is not *who makes network calls*; both do. The gateway has to embed the **question**, so
it calls `OLLAMA_URL` at boot and on every search and every ask, and with a provider configured it
calls the model as well. The boundary is **who chooses the destination**:

- The indexer's clone destination comes from a stranger's submission. That is why host-allowlisting,
  scheme checking and the neutered `git` environment are where they are.
- Every other destination is **operator-chosen at boot** — the embedder's `OLLAMA_URL`, and the model
  provider's base URL, which is additionally checked against a compiled-in exact-host allowlist and
  refuses to start otherwise. Nothing derived from a request body, a question, a repository's
  contents or a tool result reaches a URL, a host, a header or a proxy setting. That is a test rather
  than a promise: a question and a span both containing `https://evil.example/` are driven through
  the client and the recorded request URL is asserted byte-identical to the boot-time constant.

One inherited gap, recorded rather than fixed: the transport controls listed under
[the answering loop](#the-answering-loop) cover the **provider** endpoint only. `search_code` reaches
the embedder through the shared embedding client, which has none of them — it honours `HTTPS_PROXY`
and follows redirects. Worth knowing before sizing an Ollama deployment: an `/ask` that runs the tool
loop makes up to 12 embedder calls rather than one, because every `search_code` embeds the model's
rewriting of the question.

### One datastore

Postgres with pgvector, and no separate vector database. The questions this product answers are
partly relational — "who calls this" is a recursive CTE — so keeping the embeddings in the same
database means a similarity search and a graph hop are one query against one consistent snapshot,
rather than two systems that can disagree about what is indexed. The job queue is a Postgres table
too: one producer, durable, retryable, inspectable from `psql`. Migrations live in
`packages/shared/store/migrations/` and run at boot on both binaries.

### Indexing a stranger's repository

codetrail accepts any public Git URL through its API and runs `git clone` on it. Several decisions
look paranoid because of that.

**Admission runs before anything is fetched**, and the rules are listed under
[`POST /api/repos`](#post-apirepos). The reasons behind them: `https` only, because `file://` alone
would turn "index a repo" into "read the indexer's disk"; an **exact** host allowlist rather than a
suffix match, so `github.com.evil.example` and `pages.github.com` are both refused; and a rejection
that names **which rule** fired, because a generic `400` tells an operator nothing about what to
change.

`ALLOWED_HOSTS` replaces the default rather than extending it. One limitation to know before setting
it: an accepted path is exactly `/owner/name`, so a forge that nests namespaces deeper is only half
served — adding `gitlab.com` accepts `group/repo` and refuses `group/subgroup/repo`. Host-allowlisting
is the SSRF control; it does **not** defend against a hostile allowlisted forge, and codetrail claims
no DNS-rebinding protection, because `git` is a subprocess and cannot be handed a validating dialer.

**One repository is one queued job, including while it waits to retry.** Submitting a repository that
is already `pending` or `leased` returns the existing job rather than queueing a second clone, matched
case-insensitively. A job that fails an attempt goes back to `pending` and waits out a backoff, and a
waiting job is still `pending` — so re-submitting during the wait returns the waiting job and does not
start it any sooner. The API answers `202` either way, so it is written here rather than left to be
inferred from a job that does not move.

**The clone is shallow, single-branch and blob-filtered**, runs in its own process group under a
wall-clock deadline so the timeout kills `git`'s children too, and has `GIT_TERMINAL_PROMPT=0` with a
neutered `GIT_ASKPASS` so a private URL fails immediately instead of blocking forever on a credential
prompt nobody is there to type. Size and file-count caps are **enforced**: a checkout over the cap is
deleted, because a cap that leaves the oversized tree on disk has not enforced anything.

**The walker never follows a symlink.** A repository can contain `link -> /etc/passwd`, and a naive
walk reads and indexes it, so anything that is not a regular file is skipped outright. The guards are
mutation-checked, and every symlink, fifo and device fixture is built at runtime — checking this
repository out requires nothing symlink-shaped on disk.

**The corpus does not grow without bound.** Hard admission caps plus LRU eviction on last-queried
time: anyone may submit, a popular repository stays warm, and the least recently queried one goes when
`KEEP_REPOS` is reached, in a single `DELETE` that cascades. Every successful read winds the clock, so
a repository is not evicted while it is being queried. Job history is bounded separately and by age —
a terminal job is swept `JOB_HISTORY_HOURS` after it finishes and a running job is never swept, so a
job id its holder is still polling is never `404`ed at a moment chosen by an unrelated stranger. What
none of this bounds is a flood inside the window; the control for that would be a rate limit on
`POST /api/repos`, which does not exist.

## How it works

### Chunking

**One span per top-level declaration**, carrying its doc comment, with a kind (`func`, `type`,
`const`, `var`) and a symbol — a method's is receiver-qualified, so `Logger.Info` rather than `Info`.
A declaration longer than `CHUNK_MAX_DECL_LINES` (200) is cut into fixed windows instead of becoming
one useless span, and a file that is not Go — or Go that will not parse — is windowed whole. Import
declarations are dropped: an import block is not a retrievable unit, and it is the one declaration
whose text repeats across thousands of files.

`kind=file` does **not** say which strategy produced a row. Sub-windowed declarations and unparseable
files carry it under the AST strategy too. The strategy is a property of the run, never of a row.

**Fixed windows are a first-class strategy, not only a fallback.** `CHUNK_STRATEGY=window` chunks the
whole corpus into 40-line windows with 10 lines of overlap and never parses anything. It exists so
the eval has a baseline that was not retrofitted after the fact.

**Two limits, measured rather than guessed:**

- **Package documentation is unretrievable under the AST strategy.** `f.Doc` is not a declaration, so
  a file holding only a package comment produces no spans at all, and neither does an import-only
  `tools.go`. The two strategies therefore do not cover the same set of files, and a question about
  that prose cannot be answered from it — under `hybrid` it does not even refuse, because the vector
  arm returns its top candidates whatever they score, so the honest outcome is an answer citing
  *other* files.
- **Stripping doc comments moves AST span boundaries.** It moves no *line* — blanking the comment
  bytes and keeping their newlines is what buys that — but a declaration's span starts at its doc
  comment, and a blanked comment is no longer one. 506 of `rs/zerolog`'s 1,303 AST spans start later
  in the stripped corpus, which is why gold spans have to be harvested from the corpus that was
  actually indexed.

**The production index and the eval index differ deliberately.** Production **keeps** doc comments,
because there they are the best retrieval signal a span has. The eval indexes the same repositories
with doc comments **stripped**, because the golden set's questions *are* that prose: leaving it in
would make every question a literal substring of its own answer and score both strategies on string
overlap.

**Measured on one real repository.** `rs/zerolog` at `dfd11cca` — 99 files, about 1.0 MB, picked
because it is not all Go and not one file:

| | AST strategy | window strategy |
| --- | --- | --- |
| spans, doc comments kept | 1,303 (1,044 `func`, 100 `type`, 73 `file`, 67 `var`, 19 `const`) | 771, all `kind=file` |
| spans, doc comments stripped | 1,303 | 768 |
| files with at least one span | 87 of 99 | 88 of 99 |

The window strategy loses three spans to stripping, because blanking `log.go`'s ~100-line package
comment leaves three 40-line windows with no word left in them. They are dropped rather than embedded
and the job reports how many: a zero vector would be worse, because
`'[0,0,0]'::vector <=> '[1,2,3]'::vector` is `NaN` in pgvector and would rank unpredictably instead of
failing loudly.

**Embedding dominates everything else.** On one machine, with temporary instrumentation since
reverted: ~147 s with `nomic-embed-text` on CPU, against 18 ms to read and chunk the whole checkout
and 1.4 s to write all 1,303 spans. Most of that write is the HNSW index — the same 1,303 vectors
insert in 33 ms without it and 1,031 ms with — which is not worth acting on at this size and would be
at a hundred times it. Those figures are a one-time hand measurement, not something the shipped
binaries log. Indexing the same commit twice produces the same 1,303 span ids, which is what makes a
retry safe.

### Citations

A citation is `(repo, commit, path, startLine, endLine, digest)`. The tuple is the claim; the
permalink is the convenience.

**The permalink is pinned to the commit, never the ref**, so it means the same thing in a year. It is
rendered only for a forge whose URL shape is in a two-entry table (`github.com` and `codeberg.org`,
whose blob URLs genuinely differ); for anything else the field is the **empty string** rather than a
guess, because a guessed URL that `404`s reads as "the code is gone" instead of "we guessed". The
clone allowlist is configured separately and on purpose: "whose code may we clone" is a different
question from "whose URL shape do we know", so an operator who allowlists a third forge gets a corpus
whose citations have no links and full tuples.

**Staleness says exactly what is known and what is not.** Every citation carries the commit it is
correct at, how long ago that was indexed, and — in those words — that **codetrail has not checked
whether the ref has moved since**. Asking the forge would put a network call to a stranger's host on
the public read path, which is the egress this design confines to the indexer. The one positive claim
the corpus can support it does make: when the same ref is *also* indexed here at a later commit,
`state` is `superseded` and the note names that commit.

**The verification command is exact, and `head -c -1` is load-bearing.** A span's text ends at the
last byte of its last line, not at the newline after it, so `sed -n '126,127p' | sha256sum` alone
hashes one byte more than the digest covers and produces a different hash. The check is only a check
if it fails when it should. The console renders the command with a copy button beside every citation.

A real run: a question over `rs/zerolog`, its extractive answer, and the top citation's digest
matching what `git show` prints for exactly those lines at that commit.

<img src="assets/screenshots/ask.png" alt="An answer over rs/zerolog, its citation's digest matching git show piped through sed and sha256sum, and an uncalibrated floor of -1" width="880">

### Retrieval

Two arms over one repository. All three modes ship and stay switchable — and **the default runs the
vector arm alone**, because fusion was measured and lost.

**The vector arm** is pgvector cosine similarity, filtered by `repo_id`, ordered on the distance
operator so it can use the HNSW index; sorting on the computed similarity gives the same order and no
index path at all. What leaves the store is the similarity, `1 - distance`, because a floor on a
quantity where lower is better is an inverted filter whose tests still pass.

**The lexical arm** is Postgres full-text search over a generated `tsvector` column: the span's symbol
at weight `A` and its whole text at weight `B`, with a GIN index. A question never reaches
`to_tsquery` — `&`, `|`, `!`, `:` and `(` are operators there and most questions about code contain
one — so the query is tokenised into letter-and-digit runs and `OR`-ed. `OR`, not `AND`: this arm
exists to widen what fusion has to work with, and an `AND` over a tokenised question finds nothing as
soon as one word is missing.

**Fusion is by rank, never by score.** A cosine similarity and a `ts_rank_cd` have no common unit, and
normalising them would invent an exchange rate nobody measured. A span scores `Σ 1/(k + rank)` over
the arms that returned it, with `k = 60` — the constant from the paper the method comes from, not a
value measured against this corpus.

**Two limitations of the lexical arm, both measured.** Under `ts_rank_cd` **repetition outranks
coverage**: a span mentioning one query term six times outranks a span mentioning each of two terms
once (2.4 against 0.8), and under `ts_rank` the order reverses (0.1813 against 0.2432). And the
`simple` text-search configuration **has no stopword list**, so `"where is parseConfig defined"`
becomes the `OR` of `where | is | parseconfig | parse | config | defined` — every word of it, plus the
camel-case parts `LEXICAL_SPLIT_IDENTIFIERS` adds — and every span containing the word "defined" is a
candidate. Neither is fixed: a code-appropriate stopword list and a ranking-function sweep are both
choices that want a corpus to measure against, and guessing at them now is what the `-1` floor exists
to avoid.

#### What the eval said

`RETRIEVAL_MODE` defaults to `vector`. It used to default to `hybrid`, on the argument that the design
*defines* retrieval as hybrid — and that argument stopped being the best one available the moment
there was a measurement.

Measured on **one corpus**: `google/uuid` at `2d3c2a9`, 74 mechanically generated cases (every
exported symbol with a doc comment; the question is the prose, the answer is that symbol's span),
live `nomic-embed-text`, `k = 60`, 40 candidates per arm, limit 10, identifier splitting on. Only
`RETRIEVAL_MODE` differed between the three runs. On the AST arm:

| mode | MRR | hit@1 | hit@5 | hit@10 | gold never retrieved |
| --- | --- | --- | --- | --- | --- |
| `vector` | **0.7492** | 0.6216 | 0.9054 | 0.9459 | 4 / 74 |
| `hybrid` | 0.4023 | 0.2432 | 0.6351 | 0.7568 | 18 / 74 |
| `lexical` | 0.1637 | 0.0676 | 0.2703 | 0.3784 | 46 / 74 |

The decision rule was written down before the data. MRR is the primary endpoint; the threshold is
`max(2σ, δ)` with `δ = 0.02` a pre-registered minimum effect size and σ the bootstrap standard error
of the difference, resampling the 74 questions with replacement `B = 1000` times. σ = 0.052, so the
threshold is 0.104. `MRR(hybrid) − max(MRR(vector), MRR(lexical)) = −0.347`, which is **3.3× the
threshold in the losing direction**. σ is stable: over the full grid of five seeds and `B` from 200 to
20,000 it stays between 0.052 and 0.058 on the AST arm, and the decision is the same in every one of
those combinations. The window arm agrees in direction by a narrower margin (−0.122 against a
threshold of 0.098). The script is [`docs/eval/bootstrap.py`](docs/eval/bootstrap.py); the runs are
under [`docs/eval/runs/`](docs/eval/runs).

hit@k and the refusal behaviour were reported and did not decide. They agree with MRR in sign at every
`k`, which is worth saying because a disagreement would have been the more interesting finding. The
`gold never retrieved` column is the mechanism: both arms are searched to a depth of 40 and the fused
list is trimmed to 10, so a lexical arm that ranks the gold span poorly pushes it out of the answer
entirely.

**What this result does not say.** It is one corpus — one small, single-package Go library — and the
golden set is *doc-comment prose → the symbol it documents*, which is close to the best case for
embeddings and close to the worst case for a lexical arm. It contains
**no identifier-lookup question at all**, and "where is `parseConfig` defined" is the question the
lexical arm exists for. So the honest claim is
**fusion does not help on doc-comment prose queries, on this corpus, at these settings** — not
"fusion does not help". The two lexical limitations above compound it and are not controlled for.
Judging the lexical arm properly needs a stopword list, a ranking-function sweep and a second golden
set of identifier lookups; none of that has been done.

**Nothing was deleted.** Fusion, the per-arm weights and the per-arm ranks all still ship. Every hit
carries its `vector_rank` and `lexical_rank`, so an arm's contribution is readable from the data
rather than inferred. `k` and the per-arm candidate depth are settable without a code change; **the
per-arm fusion weights are not** — they are both `1` in the gateway's wiring and no environment
variable sets them.

**Why there is only one corpus.** Three were named before the run, and a leakage probe scans every
indexed span for every normalised question, refusing the whole corpus on the first hit. `google/uuid`
passed with zero leaking cases out of 74. `rs/zerolog` was refused for **two genuine leaks** out of
514 cases — a doc comment repeated verbatim inside a function body, and an `Example`'s doc comment
repeated in a `README.md`, which the doc-stripper leaves alone because it is not Go. `sirupsen/logrus`
was refused for **one false positive** out of 191, where a two-word question appears inside an
unrelated window; that is a defect in the probe rather than in the corpus, and it was recorded rather
than patched, because the threshold that would clear it sits one word away from the genuine zerolog
leaks. A test reads the ledger and fails the build if a corpus marked published has no committed run,
or a corpus marked refused has one. [`docs/eval/corpora.md`](docs/eval/corpora.md) accounts for all
three.

**Chunking was compared on the same corpus, and that comparison is weaker.** In the shipped `vector`
mode, MRR is **0.749** for the AST strategy under both gold rules, against **0.606** lenient and
**0.510** strict for the window strategy. No decision rule was pre-registered for this endpoint and no
default moved. The two are **not comparable on that number alone**: the window strategy's mean lenient
gold set is 1.5 spans against the AST strategy's 1.0, so the lenient rule hands it more chances while
the strict rule penalises it for tiling, and its haystack is smaller (110 spans against 189). Both
asymmetries are real and neither is corrected for. The claim this file makes is that **AST chunking
led on one corpus at the shipped settings** — not that AST chunking retrieves better. Every figure,
both gold rules and every qualifier are in [`docs/eval/README.md`](docs/eval/README.md).

#### Grounded, or refused

codetrail refuses when retrieval returns nothing, when the top cosine similarity falls under
`ANSWER_SCORE_FLOOR`, and when that similarity is not a number at all. A refusal is a `200` carrying
`"refused": true` and a reason from a closed set; it is never an HTTP error, and the two are separate
counters asserted in both directions.

**The floor is compared against the vector arm's cosine similarity**, not the fused score. After
reciprocal-rank fusion there is no quality number left — the top hit of any non-empty result scores
`1/(k+1)` whether it is perfect or the best of a worthless set — so a floor there would be "did
anything come back" with extra arithmetic. In `RETRIEVAL_MODE=lexical` there is no such number at all,
and the response says `"applicable": false` rather than comparing an unbounded, corpus-dependent
`ts_rank_cd` against a cosine threshold.

**The floor's value is not calibrated**, and everything a reader sees about it derives from
`floor.calibrated` rather than restating one of the two states. What ships is the mechanism and the
instrument: the refusal path, the knob, two gauges, and a `codetrail_retrieval_top_score` histogram
that a calibration would read. Picking a threshold before measuring one is a guess wearing a
measurement's clothes. When somebody measures one, `calibrated` flips to `true` and every sentence
about the floor changes with it.

The distributions that exist: on `rs/zerolog`, seven real questions produced top cosine similarities
between 0.664 and 0.744; on `google/uuid`'s 74 generated cases the AST arm's range is wider on both
sides, 0.6239 to 0.8507. Those are costs and ranges, not a quality result, and reading a threshold off
a single library's documentation style is exactly what calibration on one corpus would be.

**Retrieval latency**, mean over 21 asks per mode on the 1,303-span `rs/zerolog` corpus: `lexical`
5.3 ms, `vector` 24.0 ms, `hybrid` 33.7 ms. Most of the vector modes' time is the query embed — one
`nomic-embed-text` call on CPU measured at ~16 ms on its own — so the pgvector query is ~8 ms and the
second arm costs ~10 ms. The cost of the second arm is a number; its benefit is not.

### The symbol graph

A precise Go call graph needs type information, which needs the repository to actually compile — right
Go version, dependencies downloadable. Plenty of public repositories will not, in a sandbox.

So codetrail takes **every** call site from the AST first, then lets `go/packages` upgrade the rows it
can name, and **labels every edge with which one it got**. A **`resolved`** edge means *this exact
symbol*, and `to_symbol_id` points at its row. A **`syntactic`** edge means *something named `Close`*,
and its target is **null rather than a guess**. Nothing binds a syntactic edge afterwards: "who calls
this" traverses `to_symbol_id` and never a name, and name-matched callers come back in a separate,
labelled, depth-1 set with its own count, so a client that flattens the two does it knowingly.

**The label is per-edge, not per-repository.** The edge *set* comes from the AST and type information
only ever upgrades individual rows, so the same repository indexed with and without the type-checker
produces the **same edge ids** and differs only in `provenance` and `to_symbol_id`. A repository where
nine packages type-check and four do not gets precise edges for the nine and honest approximations for
the four, rather than being downgraded wholesale.

Only `calls` edges are written. `imports` and `references` are in the schema's `kind` enum and nothing
writes them: `imports` cannot be written under this schema at all, because `from_symbol_id` is
`NOT NULL` and an import belongs to a *file* rather than to a definition.

#### What one real repository looks like

`rs/zerolog` at `dfd11cca`, on the default policy. These are what indexing **cost and produced**. No
golden set exists for the graph, so nothing here says the graph is good.

| | | | |
| --- | --- | --- | --- |
| symbols (definitions) | 1,234 | `resolved` edges | 945 |
| edges (call sites) | 7,895 | `syntactic` edges | 6,950 |
| resolved outside the repository, counted `external` | 498 | callees neither identifier nor selector, counted `unnameable` | 435 |
| packages attempted / loaded / failed | 13 / 9 / 4 | the job's type-check reason | `load_error`, and the job still finished `done` |

**Just under one edge in eight resolves** — 945 of 7,895, or 11.97%. That is a measurement of what an
honest label looks like on a repository a sandbox will not build, not a target. It has two causes,
both measurable. **Four of the thirteen packages do not load**, because imports they need are in
modules not in the job's cache and `GOPROXY=off`: `zerolog` (`github.com/mattn/go-colorable`), `hlog`
(`github.com/rs/xid`), `journald` (`github.com/coreos/go-systemd/v22/journal`) and `pkgerrors`
(`github.com/pkg/errors`). And **external test packages are never loaded** — the loader runs with
`Tests: false` — so every call in `zerolog_test`, `log_test`, `diode_test` and `hlog_test` is
syntactic, which is 997 of the 6,950 on its own.

The per-package split is where the per-edge claim stops being an argument and becomes data:

| package clause | `zerolog` | `cbor` | `json` | `zerolog_test` | `hlog` | `log` | other ten |
| --- | --- | --- | --- | --- | --- | --- | --- |
| resolved | 613 | 172 | 39 | 0 | 54 | 26 | 41 |
| syntactic | 2,823 | 1,427 | 1,018 | 785 | 335 | 1 | 561 |

The first column is worth reading twice: `zerolog` is one of the four packages that **failed to
load**, and it still carries 613 resolved edges. A stage that had stamped the package's outcome on its
rows would have written 3,436 syntactic edges there and looked entirely healthy.

The same check one layer up — `Event.Msg`'s resolved callers with a `file:line` per hop and a
checkable citation each, its name-matched callers kept in a list of their own, and the cited line
holding the call it claims:

<img src="assets/screenshots/graph.png" alt="Event.Msg's three resolved callers with their call sites, its three name-matched approximate callers, and the cited line and digest checked against git show" width="880">

#### Three limits of the graph

- **A call into the standard library resolves and is still recorded as `syntactic`.** `fmt.Sprintf`
  names a real object and has no row here to point at, and the invariant that a null target is what
  makes the label mean anything cannot bend. The fact is not lost — those 498 calls are counted
  `external`, separately from the ones nothing could name at all — but a third enum value would be
  more informative and the schema has two.
- **The cycle guard is not answer-neutral.** "Who calls this" is a recursive CTE whose guard stops a
  walk re-entering a node it has passed, and the walk starts at the queried symbol — so on a real
  cycle it also stops that symbol appearing in its own caller list. `rs/zerolog`'s CBOR decoder is
  genuinely mutually recursive, and at depth 5 the guarded traversal explores 7 rows against the
  unguarded one's 43 — the cost the guard exists for — but the unguarded *answer* additionally
  contains `cbor2JsonOneObject` at depth 2. A **direct** self-call does still appear, at depth 1. So
  "who calls this" reads as "who **else** calls this" once a cycle is involved.
- **The `go` command's caches are outside every cap.** `MAX_REPO_BYTES` is measured on the clone;
  `GOCACHE` and `GOMODCACHE` are written afterwards, beside the checkout, and removed with the job —
  measured at ~0.7 MB per job on the default policy.

#### What the type-checker is allowed to do

Nothing by default except read the checkout. `go` is forked inside a stranger's source tree and it is
a program whose purpose is to fetch things and compile them, so its environment is an **allowlist
built from nothing** rather than the parent's with a few overrides: `GOPROXY=off`, `GOVCS=*:off`,
`GOTOOLCHAIN=local`, `GOWORK=off`, `GOENV=off`, `GOPACKAGESDRIVER=off`, `CGO_ENABLED=0`, and
scratch-local caches that go with the job. Everything else — `GOFLAGS`, `GOPRIVATE`, `GOROOT`,
`GODEBUG`, `LD_PRELOAD`, `HTTPS_PROXY` — is closed **by omission**, which survives a mutation that
appends to `os.Environ` where a safe value would not. A recording proxy receives zero requests, with a
control run beside it that reaches one, so the zero is evidence rather than an absence. The residual
risk is stated rather than implied: `go/packages` resolves the name `go` against the **parent's**
`PATH`, and while the code refuses to run when the configured binary is not what `PATH` resolves,
nothing can help a worker whose `PATH` was hostile at boot. The same has always been true of `git`.

**`TYPECHECK_GOPROXY` turns module fetching on**, buying resolution for packages with third-party
imports and costing outbound requests to hosts chosen by a stranger's `go.mod` — the SSRF the
admission allowlist exists to refuse, reached by a road that allowlist cannot see. `direct`, and any
`,direct` fallback, is **refused at boot**: measured with the entry removed from the allowlist
entirely, the go command falls back to `https://proxy.golang.org,direct` and a fixture requiring
`golang.org/x/mod` type-checks. That is the SSRF, executed.

**The indexer needs a `go` binary on `PATH`** to resolve anything. Without one it still indexes —
every edge is syntactic, a `no_toolchain` reason is counted, and one line at boot says so — because an
indexer that cannot type-check is a better product than one that will not start. Both container images
ship a Go toolchain, and the smoke test asserts a non-zero resolved-edge count for that reason.

### The answering loop

**Extractive is the default and it stays the default.** `LLM_PROVIDER` defaults to `none`: no client
is constructed, no key is read, nothing is dialled, `answered_by` is always `extractive`, and
`degraded` is **absent** — an unconfigured deployment is not a degraded one, and reporting it as one
would make every offline deploy look permanently broken. `ANSWER_DEFAULT` also defaults to
`extractive` **even when a provider is configured**, so a public demo runs the free path for a visitor
who does not ask for more.

With a provider configured and `answerer: "llm"` on the request, the loop runs over four read-only
tools — `search_code`, `read_span`, `definition_of` and `callers_of` — under seven bounds, none of
which is configurable:

| bound | value | what it bounds |
| --- | --- | --- |
| `MaxSteps` | 6 | model calls |
| `MaxToolCalls` | 12 | tool dispatches, across all steps |
| `MaxInputTokens` | 12,000 | input tokens, **cumulative for the whole loop** |
| `MaxOutputTokens` | 1,500 | output tokens, **cumulative for the whole loop** |
| `Deadline` | 60 s | wall clock, one budget derived once from the request |
| `MaxRepeats` | 2 | identical tool calls before the loop gives up |
| `MaxToolErrors` | 2 | bad tool arguments before the loop gives up |

None of those numbers is measured. They are chosen to be obviously finite, the way the score floor's
`-1` is chosen to be obviously not a threshold. Each is observable three ways — the fake model's
request recorder, the `llm` block on the wire, and the delta vector of `codetrail_llm_stop_total` —
and each is driven at N−1, N and N+1, with the just-under case required to *succeed*, so a test can
tell "the bound fired" from "the loop never worked".

**Fourteen ways it degrades, each with its own branch.** The stop vocabulary has fifteen members and
`final` is the only one that is not a degradation. The other fourteen — `rate_limited`,
`unauthorized`, `provider_unavailable`, `deadline`, `malformed_response`, `malformed_tool_call`,
`tool_error`, `repeated_tool_call`, `step_limit`, `tool_call_limit`, `token_budget`, `uncited`,
`busy`, `budget_exhausted` — each get their own branch, their own test and their own counter label,
and every one of them serves the **cited extractive answer** instead, with
`"degraded": {"from": "llm", "reason": …}` naming which fired. Three of them never reach the provider
at all, which is why one shared "it degrades" test would not be evidence. A silent downgrade from a
model to a fallback is the kind of failure that costs a week before anyone notices.

**A not-found from a tool is not a degradation.** `read_span` on a span that is not there answers
`{"error":"not_found"}` *to the model*, because that is something a model can route around. A store
error is not: a broken database is not something a model can plan against, and letting it retry burns
spend against a system that is down.

**An answer that resolves no citation is discarded.** Citations are constructed by codetrail from the
spans the tools actually returned, never parsed out of the model's text: the model writes `[3]`, and
marker 3 resolves to the third span it opened with `read_span`. A marker naming a span it never opened
is stripped and counted in `citations_dropped`; an answer with nothing left is `uncited` and the
extractive answer is served instead.

**What the model cannot do**, each property enforced by a test. It cannot name a repository — no tool
schema has a repo field, the id comes from the URL path and is closed over before the first model
call, and a call carrying one is a `malformed_tool_call` rather than a field silently dropped. It
cannot read outside that repository, cause a write (the tool set has no writer), manufacture a
citation, reach the network directly, extend its own budget, or alter the system prompt — repository
text arrives only inside a framed tool result, escaped against its own delimiter so a span containing
`</tool_result>` cannot close its frame. The injection tests are driven by a fake that **obeys the
injection completely**, because that tests our boundary rather than a model's compliance.

**And the honest limit.** None of this claims the model cannot be made to write something false. A
comment saying "this function is safe" will influence the prose, as it would influence a human reader.
What is bounded is **reach, not persuasion**. Nor does the citation gate bound correctness: a model
that read spans A and B can write a claim true only of A and cite `[2]`, which is B. That citation is
well-formed, resolvable and digest-checkable, and attached to the wrong span. The gate guarantees
every marker points at a span the loop actually opened, whose digest you can check. It guarantees
nothing about the sentence beside it.

#### The provider client

Hand-rolled over `net/http`, no SDK, because every control here is a property of the transport and an
SDK owns the transport. `LLM_BASE_URL` is `https` only against a one-entry compiled-in host allowlist
and refuses to start otherwise; `Transport.Proxy` is `nil`, written out rather than omitted, so an
operator's `HTTPS_PROXY` cannot redirect a credential-bearing request; there is no retry, because the
loop owns the one deadline; and the key comes from `LLM_API_KEY_FILE` or `LLM_API_KEY`, **never a
`.env` file**, redacted in two layers because a secret held in an unexported field is walked by `fmt`
through reflection, where the Stringer is unreachable and `%+v` on the client prints the key.

**Every redirect is refused.** Go strips exactly six headers on a cross-domain redirect —
`Authorization`, `Www-Authenticate`, `Cookie`, `Cookie2`, `Proxy-Authorization`, `Proxy-Authenticate`
— and the Messages API authenticates with **`x-api-key`**, which is on none of them. Go would forward
this client's credential to whatever host a redirect named, so a control that reasoned about
`Authorization` would protect a header this client never sends.

#### What bounds spend, in four layers, and only the last is a ceiling

1. **The default answers nothing paid.** `ANSWER_DEFAULT=extractive` means a public demo's default
   costs zero. It does **not** mean a visitor costs zero.
2. **Per request:** the bounds above. The worst case is `MaxInputTokens + MaxOutputTokens` =
   **13,500 tokens**, because both are cumulative totals for the whole loop rather than per-call
   limits. Add up to 12 embedder calls — free of vendor cost, not of latency.
3. **Per process:** `LLM_MAX_CONCURRENT` (2) as a semaphore that **degrades with `busy` rather than
   queueing**, because the gateway has no request-timeout middleware and a queue would have no bound;
   and `LLM_TOKENS_PER_HOUR` (200,000) as a rolling in-memory budget — roughly fifteen worst-case
   requests an hour, and **per process**, so with *n* replicas the real ceiling is *n* times it. That
   number is not measured either.
4. **Per account:** your provider's own spend cap, which is outside this codebase and is the only one
   of the four that is actually a ceiling.

At Anthropic's published list price for the default `claude-opus-5` — $5 per million input tokens and
$25 per million output, published 2026-06 — the 13,500-token worst case is
**$0.060 + $0.038 ≈ $0.10 per request**. That is arithmetic over a price list; no paid request has ever
been made from this repository, so nobody has checked it against a bill.

> **Read this before you set a key.** With a provider configured, **any caller may request the paid
> path** — `answerer` is a request field, and codetrail has no authentication and no rate limit on any
> route. What bounds a stranger is `LLM_TOKENS_PER_HOUR` per process and your provider's account spend
> cap. Gating `answerer: "llm"` behind a token is the right fix and it is an auth system this project
> does not have.

### Re-indexing the same repository

**Nothing is reused that would change a row.** Every id in the corpus is a hash whose first component
is the repo id, and the repo id is `hash(remote, commit)`, so a new commit changes every id there is.
Two things are reused.

**The clone is skipped when the commit is already indexed.** `git ls-remote` resolves the ref before
anything is fetched, which makes the repo id computable with no bytes on disk; if that row exists the
job completes `done` without cloning, and writes nothing. The ref match is **byte-exact and refuses
zero-or-many**, which is the whole security content of it: `git ls-remote --heads <url> main` is a
*tail* match and the submitter owns the branch layout being matched against, so a repository holding
both `refs/heads/a/main` and `refs/heads/main` returns the wrong commit on line one — and with `main`
absent entirely it returns `a/main` and exits 0, so there is no error to notice. A hit also needs the
row's span count to be non-zero **and its spans to carry this worker's `embed_model` and
`embed_dim`**: the repo id contains neither, so without that third condition an operator who changes
`EMBED_MODEL` and re-submits everything gets `done` in seconds with the old vectors still in place,
and the repair is the re-index that just did nothing.

**The embedding is reused, keyed on `(digest, embed_model, embed_dim)`** — not the span row and not
the file row. The safety argument is one equality: the indexer computes the digest over the same
string it hands the embedder. Reuse is **not scoped to a repository**, on purpose, so that a fork, a
vendored copy and a moved file all hit; one repository's indexing therefore depends on another's rows,
and the mitigation is that an embedding is a *ranking* input rather than an authorisation one.

**Its sharpest limit.** `embed_model` is a **tag, not a build**. Two Ollama servers running different
builds of `nomic-embed-text` write the identical string into `spans.embed_model` while producing
vectors from different embedding spaces, and reuse then imports a vector that ranks nonsense
confidently against the rest of the corpus. The digest re-check cannot see it, because the text is
identical. codetrail does not solve this; it bounds the blast radius by **policy** — one embedder
build behind one `embed_model` name — which is an operational obligation rather than an enforced
invariant.

Everything else is recomputed: spans, files, symbols and edges, because their ids contain the commit;
the parse and the chunk, because skipping them needs a chunker-configuration fingerprint on every span
row and `go/parser` over bytes already in memory is not the expensive half of a job; and the
type-check and the graph, always and in full, because resolution runs over a *package*, so a call in
an **unchanged** file can resolve differently when a **different** file changes and a reused edge
would preserve a `resolved` label whose target no longer exists.

**Measured**, on `rs/zerolog` at `dfd11cca` and then a second branch of it, on the fake embedder with
type-checking off:

| pass | commit | files | spans | reused | files changed | wall clock |
| --- | --- | --- | --- | --- | --- | --- |
| 1 — `master` | `dfd11cca` | 99 | 1,303 | 0 | *(no earlier commit)* | 7.3 s |
| 2 — a second branch, reuse **on** | `eb64a6dd` | 68 | 712 | **343** | 63 | 6.3 s |
| 2′ — the same, reuse **off** | `eb64a6dd` | 68 | 712 | 0 | 63 | 5.2 s |
| 3 — re-submitted at the same commit | `eb64a6dd` | — | — | — | — | 2.1 s, **no clone** |

**Reuse saved 343 embedder calls out of 712 — and no measurable wall clock, which is the honest
headline.** The fake embedder is an in-process hash, so embedding is nearly free and the six seconds
are the clone and the walk. The saving is real only against a real embedder and **that was not
measured**: no Ollama was running for this. What *is* measured is the call count. Those 343 spans came
from 336 distinct shared digests, so seven were repeated helpers sharing one digest inside a commit —
the case a `map[digest]` assuming one row per digest would silently drop.

## The console

React 19, TypeScript and Tailwind v4 in `apps/console`, built by Vite into a static bundle. It submits
a repository, watches indexing, asks, jumps to source, and browses the symbol graph. It has a visual
design, a dark mode driven by `prefers-color-scheme`, and citation cards with a copy button for the
verification command; the design is argued in [`apps/console/README.md`](apps/console/README.md).

> **The console screenshots that used to illustrate this section are stale**, and have been removed
> rather than left presented as current. They were taken before the console had any styling at all —
> the whole stylesheet was Tailwind's preflight — and the shipped console does not look like them.
> Regenerating them needs a browser against a running stack. Every *behavioural* statement below was
> re-read against the code and holds.

**A refusal is not an error, and the console renders them differently.** A refusal is `role="status"`
headed *"No answer — and no error."*, carrying the server's own sentence for the reason plus a note
about the floor; an error is `role="alert"`, carrying a request id an operator can grep for. Neither
can render the other's evidence, and that distinction is a **type** in the client rather than a
convention: the outcome union has six members and none of them is a refusal.

**The console draws no scale from the score floor** — no bar, no meter, no percentage — while
`floor.calibrated` is false. A scale drawn from a number nobody measured renders a guess as a
measurement. The floor is a sentence, and the sentence says what it is. **The staleness sentence is
the server's, verbatim**: the console does not soften "codetrail has not checked whether the ref has
moved" into "may be out of date", and a test asserts the exact sentence *and* that no paraphrase of it
appears anywhere in the tree.

**No console test hits a live gateway.** All 183, across 21 files, run against MSW over fixtures that a
`-tags=live` Go test emits from the shipped handlers over a real Postgres, and CI fails when a
committed fixture stops matching what those handlers produce. That pins the shape and the content of
what the console renders; it proves nothing about a browser reaching a running process, which was done
once by hand. None of the tests asserts a class name, which is what makes the design safe to change.

**What it does not do.** It does not say why a job failed, because the API does not. It does not check
whether a ref has moved, because nothing does. It cannot link to a forge whose URL shape codetrail
does not know. And the accessibility sweep in
[`apps/console/docs/a11y-sweep-2026-09-03.md`](apps/console/docs/a11y-sweep-2026-09-03.md) has one
pass — a screen reader — that was **not run**, which that file says rather than leaving it to be
inferred.

## Deployment

**Nothing is deployed.** There is no AWS account behind this repository, no OIDC role, no state bucket
and no `production` environment. `terraform apply` has never run, no image has ever been pushed to a
registry, and `deploy.yml` — which is `workflow_dispatch` only — has never been dispatched once. What
follows is what exists and what it has been proved to do.

| | Verified with no cloud account, on every pull request | Needs a live account |
| --- | --- | --- |
| **Images** | Both application images build, and `infra/image_test.sh` asserts what is inside them: uid 65532 under a read-only root filesystem answering `/health`, `/ready` and `/metrics`; exactly one `go` on `PATH`, at `/usr/local/go/bin/go`; no `GO*`, `GIT_*` or `*_PROXY` in either image's environment; no shell at all in the gateway image; the RDS trust store readable by the runtime user; and a real `git clone https://…` succeeding from inside the indexer. | Every push to a registry. **The Ollama image has never been built at all.** |
| **Terraform** | `fmt`, `validate`, an offline `plan` under mock credentials on empty state, and **29 assertions** over the plan JSON. Two plans of one configuration are byte-identical. | `apply`. Every resource identifier. The destroy → apply → empty-plan cycle. |
| **Alerts** | `promtool check rules`, `check config` on both scrape files, and `test rules` — **19 tests covering all twelve rules**, every conjunct proved to decide its outcome, and four of the five state alerts proved to fire with no traffic at all. | That Prometheus in the deployed VPC can discover either service. |
| **Smoke test** | All nine assertions, against the built images over `docker compose`, including an end-to-end index of `rs/zerolog`. | The same script against a deployed URL. |
| **Workflows** | `actionlint` with shellcheck; every third-party action pinned to a resolved commit SHA. | One run. Of anything. |

**The shape.** Separate ECS Fargate services under `awsvpc` for the gateway, the indexer and the
embedder, so separate ENIs and **separate security groups**: the claim that the sandbox is a
deployment boundary rather than a code convention is a set of resources. **45 resources** plan in
total. The indexer accepts no connection but a metrics scrape, sits behind no load balancer, has **no
IAM task role** — so `169.254.170.2` is not a live credential endpoint inside the process that reads
untrusted input — and can write exactly two paths. Its task-definition environment is asserted as a
closed set of twelve keys, so a future `HTTPS_PROXY`, which would reach `git` even though it cannot
reach the compiler, is a red build.

The plan policy suite is what makes an unapplied stack reviewable: the plan is create-only and exactly
45 resources; **no data source calls the AWS API**, which is the single decision that keeps the suite
account-free; no ingress is open to `0.0.0.0/0`; the database is not publicly accessible, is
encrypted, sits in private subnets, and is reachable only from the two application security groups;
the DSN verifies the server certificate; no secret value appears in any plaintext container
environment; and `EMBED_PROVIDER` is never `fake` in the cloud.

**Egress is as wide on the gateway as on the indexer, and that is a limit rather than an oversight.**
A Fargate task pulls its image, reads its secrets and ships its logs through its own ENI, and there is
no AWS-managed prefix list for ECR, Secrets Manager or CloudWatch Logs to scope 443 with. Both are
enumerated to 443, 5432, 11434 and DNS rather than `protocol = "-1"` — a control on both rather than a
difference between them. Interface VPC endpoints would earn the stronger claim at roughly $29/month
and are not built. **The network layer does not enforce the host allowlist and does not pretend to**:
ports are enumerated, hosts never are, because `github.com` resolves into a large changing CDN range
and a TLS-terminating egress proxy is a second trust boundary in a project whose point is having few.
The host decision stays in the admission policy, and `ALLOWED_HOSTS` is written explicitly into the
task definition so widening it is a reviewed diff.

**`alb_allowed_cidrs` has no default and the plan fails without it.** `POST /api/repos` has no auth
and no rate limit and what it does is `git clone` a stranger's URL on your bill, so `["0.0.0.0/0"]` is
a legitimate answer for a public demo — it just has to be an answer somebody wrote down.

### What it would cost

List prices, us-east-1, on-demand, 730 hours — **arithmetic over a published rate, never checked
against a bill:**

| Line | Shape | $/month |
| --- | --- | --- |
| Fargate — gateway | 0.25 vCPU / 0.5 GiB | 9.01 |
| Fargate — indexer | 1 vCPU / 2 GiB | 36.04 |
| Fargate — ollama | 2 vCPU / 4 GiB | 72.08 |
| Fargate ephemeral storage — indexer | 40 GiB | 1.62 |
| ALB + its two public IPv4 | 1 LCU | 29.57 |
| Public IPv4 — tasks | 3 × $0.005/hr | 10.95 |
| RDS `db.t4g.micro` single-AZ + 20 GiB gp3 | PostgreSQL 17 | 13.98 |
| Secrets Manager, CloudWatch Logs, ECR | | 2.76 |
| **Total, always on, no console** | | **≈ $176** |

**The embedder is 41% of that**, and it is there because there are exactly two embedding providers and
one of them is a fake. A hosted embedder would cost cents; it needs 768 dimensions natively, which the
schema fixes. Scaling the services to zero leaves the ALB and the database at about $44/month and
keeps the URL and the corpus; `terraform destroy` leaves about $0.60/month for the state bucket and
the ECR repositories, and recreation takes twelve to fifteen minutes. That is defensible here because
**the corpus is disposable by design**: eviction is LRU, the repo id is `hash(remote, commit)` so
re-indexing converges, and a resubmitted URL rebuilds everything.

Everything that needs an account is listed, one row per unproven claim with the command that would
close it, in [`infra/terraform/README.md`](infra/terraform/README.md#the-account-required-ledger).

### Alerts and observability

Twelve rules in `infra/prometheus/alerts.yml`: five state alerts carrying no minimum-traffic conjunct,
one count rule, five ratios each carrying a conjunct proved to decide its outcome, and one quantile.
**No Alertmanager is deployed and nothing pages anyone** — the rules evaluate and are visible in
Prometheus, and that is the whole claim. `ScoreFloorUncalibrated` **fires by design today**, because
the floor is the `-1` placeholder; its own description says to silence it rather than chase it.
`promtool check rules` cannot tell a real metric from a typo, so `infra/prometheus/metric_names.sh`
carries that separately: every `codetrail_` name in the rules must exist as an instrument in the
metrics package.

Both binaries serve `/health`, `/ready` and `/metrics` — the gateway on `PORT`, the indexer on
`PROBE_PORT`. **`/health` checks nothing**: it is an unconditional `200` proving only that the process
is listening. **`/ready` runs the dependency checks** in order, stopping at the first failure, each
with its own 2 s timeout, and caches the result for 10 s — under the 30 s probe interval an
orchestrator uses, so a struggling database is not probed hardest exactly when it can least answer.
The gateway checks Postgres and then the embedder, using the same probe that boot used; the indexer
checks Postgres and nothing else. The container health check is `-probe`, which hits `/health` and
never `/ready` deliberately: a health check on readiness turns one shared-datastore outage into a
rolling restart of every task, killing the very process that could still serve `/metrics` and say why.

Twenty-three instruments, all `codetrail_`-prefixed, and **every label is a closed set** — never a
repository path, a file path, a symbol name or a question. Every label combination is seeded to zero
at start, so a counter that has not moved reads `0` rather than as an absent series, which is what the
ratio alerts need before the first request arrives. Because both binaries register on the same default
registry, the indexer also publishes the gateway's instruments at zero; that is why alerts over a
gateway-only *gauge* select on `service=` while counters are safe unscoped.

## Development

Go 1.27, Node 22, Docker with Compose v2. `terraform` and `promtool` are needed only for the
infrastructure targets.

```bash
make build   # bin/gateway, bin/indexer, bin/evalrunner
make lint    # gofmt + go vet, then the console's eslint and tsc
make test    # the hermetic Go suites and the console's
make psql    # a shell against the running database
make down    # stop the compose stack
```

Both Go scopes are built from **git-tracked files** rather than `./...`, because `go list ./...` does
not skip `apps/console/node_modules` and picks up a vendored Go package from inside it.

### The build tags, and what each one needs

`_live_test.go` is a naming convention, not a build tag, and it maps to **three** of the four. So
`go test -tags=live ./...` silently skips two files whose names say "live" — nothing errors, they are
simply not in the build.

| Tag | Gates | Needs |
| --- | --- | --- |
| *(none)* | 638 tests. The whole product's logic, hermetically. | Nothing. |
| `live` | +199 tests: the store, the queue, both binaries end to end, and the console's fixtures. | **A live Postgres with pgvector.** Runs on `EMBED_PROVIDER=fake` — no model. |
| `ollama` | +3 tests: the one check a fake cannot make, that the real model returns the width the schema is built for. | **A running Ollama with `nomic-embed-text` pulled.** |
| `llm` | +3 tests: the Anthropic request shape. | **A paid API key. This suite costs money and has never been run.** |
| `tfplan` | 29 assertions over the Terraform plan JSON. | `terraform` on `PATH` and a generated plan. **No AWS account.** |

```bash
# The tests that matter most: the ones against a real database.
DATABASE_URL='postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable' \
  go test -tags=live ./...

# The width check, against a real model.
OLLAMA_URL=http://localhost:11435 go test -tags=ollama ./packages/shared/embed/

# The one suite that COSTS MONEY. Read the spend section before you run it.
LLM_PROVIDER=anthropic LLM_API_KEY=sk-… go test -tags=llm ./packages/shared/llm/
```

CI runs the default and `live` suites and the plan policy suite on every pull request. It `go vet`s
the `ollama` and `llm` suites without running them, so an opt-in suite cannot rot unnoticed between
the runs nobody makes.

**Each live suite creates a throwaway database of its own** beside the one `DATABASE_URL` names,
migrates it, and drops it when the suite ends. That is not tidiness: these tests clear whole tables —
eviction is a whole-corpus operation and the queue's leases are global — and doing that to the
database you pointed them at is how a corpus disappears while the suite still prints `ok`. The DSN
above is therefore only ever connected to in order to `CREATE DATABASE` and `DROP DATABASE`, which the
role it names has to be allowed to do.

### The other checks

`make test` runs **none** of these, and CI runs each as its own step.

```bash
make policy          # terraform plan under mock credentials, then the assertions over its JSON
make smoke           # the nine-assertion end-to-end script against the built images over compose
make alerts-test     # promtool check rules + check config on both scrape files + test rules
make images          # build both application images
make image-test      # assert what is inside them: uid, filesystem, PATH, trust store, egress
make migrations-lint # refuse a destructive migration that is not expand-only
make eval-corpus     # the two indexer passes the chunking comparison needs, as one command
```

`make policy` runs `make tf-plan` first, so it needs `terraform`; `make alerts-test` needs `promtool`;
`make smoke`, `make images` and `make image-test` need Docker.

Console fixtures are not written by hand — they are emitted from the shipped handlers over a real
Postgres, and CI fails when a committed fixture stops matching what those handlers produce:

```bash
DATABASE_URL='…' UPDATE_CONSOLE_FIXTURES=1 \
  go test -tags=live -run TestConsoleFixtures ./apps/gateway/internal/handler/
```

The eval runner is a separate binary with its own flags rather than a `DATABASE_URL`: it takes
`-ast-dsn` and `-window-dsn`, because comparing two chunking strategies means indexing the same commit
twice into two databases. [`docs/eval/README.md`](docs/eval/README.md) has the recipe and reads the
artefacts under [`docs/eval/runs/`](docs/eval/runs).

## Portfolio sibling

Alongside [sitemon](https://github.com/mralaminahamed/sitemon) (event-driven monitoring, Go + NATS +
React) and [flagcast](https://github.com/mralaminahamed/flagcast) (feature flags, Go + gRPC + React).
codetrail adds static analysis, a relational graph over code, and a retrieval experiment with a
measurable answer.

## License

[MIT](LICENSE)
