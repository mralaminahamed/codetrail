# codetrail — design

**Status:** approved 2026-08-31. Supersedes nothing; this is the project's first spec.

Ask a codebase a question, get an answer that cites `file:line`.

---

## 1. What it is, and why it is not triagepilot

codetrail indexes a public git repository and answers questions about it, citing the exact
spans it used. It shares a shape with [triagepilot](https://github.com/mralaminahamed/triagepilot)
— retrieval, grounded-or-refused, an optional agentic loop — and differs in the three places that
make it worth building:

1. **Code does not chunk like prose.** Paragraph windows cut functions in half. codetrail chunks on
   the AST — one span per declaration — and *measures* that against a fixed-window baseline rather
   than asserting it.
2. **A code citation can be checked.** Prose citations can only be quoted. A `(commit, path, line
   range, digest)` tuple either still holds what was claimed or it does not, and it renders as an
   immutable forge permalink.
3. **Retrieval is hybrid, not just vector.** A symbol graph answers "who calls this", which
   embeddings answer badly, and a lexical arm answers "where is `parseConfig` defined", which
   embeddings answer worse.

It also accepts a stranger's URL and runs `git clone` on it, which makes the ingestion path the
sharpest edge in the project and the reason several decisions below look paranoid.

**Non-goals.** Not multi-language at first (Go only — see §7). Not multi-tenant. No write access to
any repository, ever: codetrail reads.

---

## 2. Architecture

Four apps, matching the sibling repositories' layout.

| App | Owns | Trust boundary |
| --- | --- | --- |
| `apps/gateway` | The HTTP API: submit a repo, poll a job, search, read spans, walk the graph, answer a question. Writes only two things: job rows, and `repos.last_queried_at` (which is what makes LRU eviction possible). It never writes indexed content. | Public |
| `apps/indexer` | Leases jobs, clones, parses, embeds, writes, deletes the clone. The only component that touches an untrusted URL or forks `git`. | **Untrusted input** |
| `apps/console` | React 19 + TypeScript + Tailwind v4. Submit, watch indexing, ask, jump to source. | Browser |
| `apps/evalrunner` | The chunking experiment. Runs the *same* retriever the gateway serves from. | CLI |

`packages/shared` holds `config`, `logger`, `health`, `metrics`, `models`, `store`, `chunk`,
`embed`, `rag`, `symbols`.

**Why gateway and indexer are separate processes.** The indexer is the only thing that handles
untrusted input, forks a subprocess, needs disk, and reaches the network. Splitting it makes the
sandbox a *deployment boundary* rather than a code convention, and the two have opposite resource
profiles — the indexer is spiky CPU and disk, the gateway is steady memory — so they scale apart.

**Why the queue is a Postgres table.** One producer, durable, retryable, inspectable from `psql`,
and the database is already a hard dependency. Introducing NATS or Redis to carry one queue would
be ceremony, and the README would have to justify it.

---

## 3. Data model

### Tables

- **`repos`** — `id`, `remote`, `ref`, `commit_sha`, `indexed_at`, `last_queried_at`, `size_bytes`,
  `file_count`, `status`. Unique on `(remote, commit_sha)`.
- **`files`** — `id`, `repo_id`, `path`, `blob`, `lang`, `lines`. `blob` is git's own content hash,
  so an unchanged file across commits is recognised without reading it.
- **`spans`** — `id`, `repo_id`, `file_id`, `path`, `kind`, `symbol`, `start_line`, `end_line`,
  `text`, `digest`, `embed_model`, `embed_dim`, `embedding vector(768)`.
- **`symbols`** — one row per definition: `id`, `repo_id`, `file_id`, `name`, `pkg`, `kind`,
  `span_id`.
- **`edges`** — `id`, `repo_id`, `from_symbol_id`, `to_symbol_id` **nullable**, `to_name` **not
  null**, `kind` ∈ `{calls, imports, references}`, `provenance` ∈ `{resolved, syntactic}`.
- **`jobs`** — `id`, `remote`, `ref`, `status`, `attempts`, `leased_by`, `leased_until`, `error`,
  timestamps. A partial unique index on `(remote, ref)` where the status is not terminal, so
  submitting the same repository twice while it is already queued returns the existing job instead
  of cloning it twice.
- **`schema_migrations`** — `name`, `applied_at`.

### Invariants

**Line numbers are 1-based and inclusive.** Every editor and every `file:line` convention counts
that way. An off-by-one here is not cosmetic; it is a citation pointing at the wrong code.

**A syntactic edge has a null target.** It knows it calls something named `Close` and cannot say
which one. Recording a null rather than a guess is what makes the `provenance` label mean anything.

**IDs are deterministic.** `repo = hash(remote, commit)`, `span = hash(repo, path, start, end,
digest)`. Re-indexing the same commit writes the same rows, so a retried job after a crash
converges instead of duplicating. Idempotence is therefore a testable property, not a hope.

**Eviction is one `DELETE`.** Removing a `repos` row cascades to files, spans, symbols and edges.
No orphan sweep, no second system to disagree about what exists.

**The vector width is fixed at 768 and enforced at startup.** An ANN index requires a known
dimension. `store.CheckDim` refuses an embedder of another width rather than letting two vector
spaces share a table, where they would rank nonsense confidently and nothing about the query would
look wrong.

---

## 4. Ingestion and the sandbox

### Admission — before anything is cloned

1. **Scheme must be `https`.** `file://` alone would turn "index a repo" into "read the indexer's
   disk"; `ssh://` and `git://` have no legitimate use here.
2. **Exact-host allowlist**, configurable, defaulting to a small set of forges. An allowlist, not a
   denylist: the interesting SSRF targets are the ones nobody thought to deny.
3. **Caps enforced, not merely declared** — byte cap, file-count cap, and a wall-clock deadline that
   kills the whole process group.
4. `GIT_TERMINAL_PROMPT=0` and `GIT_ASKPASS` neutered, so a private URL fails immediately instead of
   blocking on a credential prompt.

### Cloning

`git clone --depth 1 --single-branch --filter=blob:none` into a scratch directory that is removed
whether the job succeeds or fails.

### Walking

**Every entry is `Lstat`-ed and non-regular files are skipped — never `Stat`, never followed.** A
repository can contain `link -> /etc/passwd`; a naive walk reads and indexes it. This is a
vulnerability, not a hardening nicety, and it gets a committed fixture containing an escaping
symlink plus a test that fails if the walker follows it.

### What the allowlist does not buy

Host-allowlisting is the SSRF control. It does **not** defend against a hostile allowlisted forge,
and codetrail claims no DNS-rebinding protection: `git` is a subprocess and cannot be handed a
validating dialer. The README states this in these terms.

### Job lifecycle

`pending → leased → running → done | failed`. Leases expire, so a dead indexer's job returns to the
queue rather than vanishing. Attempts are capped with backoff; a terminal failure carries its reason
into the API.

### Retention

Hard admission caps plus **LRU eviction** on `last_queried_at`. Anyone may submit; nothing grows
without bound; a popular repo stays warm. An evicted repo answers `410 Gone`, not `404` — it
existed, and that is a different fact.

---

## 5. Chunking

Go files parse with `go/parser`; AST only, no type information required. One span per top-level
declaration — function, method, type, const, var — carrying its doc comment.

Fallbacks, both tagged `kind=file`:

- a file that fails to parse, or is not Go, is split into fixed windows;
- a declaration longer than a threshold is sub-windowed, so one generated 3,000-line file does not
  become a single useless span.

**The chunker takes a strategy, not just a fallback.** The eval's baseline arm (§9) needs the
*entire* corpus chunked into fixed windows, not only the files the AST could not read. So chunking
is a `Strategy` — `AST` or `Window` — chosen by the caller, where `AST` falls back to windowing per
file and `Window` never parses at all. Without this the baseline arm cannot be built, and the
headline comparison has nothing to compare against.

### The doc-comment problem

The eval's golden set is "doc-comment prose → the symbol it documents" (§8). A doc comment normally
lives *inside* its span's text, so the query would be a literal substring of the correct document.
Both arms would then score near-perfectly on string overlap and the comparison would measure
nothing — it would be `grep` wearing a benchmark's clothes.

**Therefore the eval indexes a corpus with doc comments stripped from span text.** The question is
the prose; the haystack is code bodies only; both arms get identical treatment. The production index
keeps doc comments, because they are the best available signal there. The two corpora differ
deliberately and the README says so rather than letting a reader discover it.

---

## 6. Symbol graph

Definitions come from the AST. Edges are attempted through `go/packages` with type information;
where `types.Info.Uses` resolves a call to a real object the edge is `resolved` and points at a
symbol row, and where it does not the edge is `syntactic` with a name and a null target.

**Provenance is per-edge, not per-repo.** A repository where three packages type-check and two do
not gets precise edges for the three and honest approximations for the two, rather than being
downgraded wholesale. This makes the label a per-row property a test can pin.

Type-checking requires module downloads, so it runs inside the same sandbox and draws on the *same
per-job wall-clock budget* as the clone — one deadline for the whole job, not a fresh one per stage,
so a slow clone cannot buy itself extra time by failing into the type-check. Its failure is expected
rather than exceptional, and downgrades the repo's edges to `syntactic` instead of failing the job.

---

## 7. Language scope

Go only, first. `go/parser`, `go/ast`, `go/types` and `go/packages` are standard library or
`golang.org/x/tools` — no cgo, no tree-sitter build step. A second language is a later phase, and
claiming multi-language support on day one would mean claiming quality that has not been measured.

---

## 8. Retrieval, citations, answering

### Retrieval

pgvector cosine over spans, filtered by repo, fused by reciprocal rank with a lexical arm over
symbol names and identifiers. Both arms are measurable, so fusion-versus-vector-only is a second
experiment the eval settles rather than an assertion.

### Citations

`(repo, commit, path, startLine, endLine, digest)`, rendered as a forge permalink
(`…/blob/<sha>/path#L10-L20`), which is immutable by construction.

- **Digest** proves the retrieved text is the indexed text, so truncation or corruption is
  detectable rather than merely plausible.
- **Staleness is explicit.** If the ref has moved since indexing, the API says the citation is from
  an older commit. "Correct at commit X, and X is three months behind `main`" is a different claim
  from "correct".

### Answering

**Extractive by default**: ranked spans assembled with citation markers, or a refusal when the top
score falls under a floor that the eval measures rather than picks. Zero marginal cost, works
offline, and a public demo costs nothing per visitor.

**LLM opt-in**: a bounded tool loop over `search_code`, `read_span`, `definition_of`, `callers_of`,
wrapped so a rate limit or an expired key degrades to extractive — and **the response names which
one answered it**. A silent downgrade is the failure that costs a week.

---

## 9. The eval

The golden set is **derived mechanically from the corpus**: for every exported symbol carrying a doc
comment, the question is the prose and the correct answer is that symbol's span. Generated, not
hand-picked, so the questions cannot be authored to flatter the chunker under test. It yields
hundreds of cases for free and regenerates as the corpus changes.

Measured: hit@k, MRR, and the refusal behaviour, across two arms — AST spans versus fixed windows —
over the doc-comment-stripped corpus of §5. The score floor of §8 is calibrated from the resulting
distribution, the same way triagepilot's was, and recorded with the numbers that produced it.

CI runs the harness on a deterministic fake embedder to prove the mechanics still work end to end,
and **the figures that run prints are not quality** — the same banner triagepilot carries.

---

## 10. Error handling

- Admission rejection → `400` naming **which rule** failed, never a generic refusal.
- Evicted repo → `410 Gone`.
- Unknown repo or span → `404`.
- Job failure → capped attempts, backoff, terminal state carrying its reason into the API.
- Refusal and error are **distinct outcomes** in metrics, so "we had nothing to say" never hides
  inside "we broke".

---

## 11. Observability

Prometheus on `/metrics`, `/health` for liveness and `/ready` for dependency health, with
`codetrail_ready` as a gauge — `up` says the process is listening, which a gateway that has lost
Postgres still is.

Counters and histograms for: job outcomes and durations, admission rejections **by reason**,
retrieval latency and top-score distribution, answer outcomes, evictions. Every ratio alert carries
a minimum-traffic conjunct stating the sample size it is willing to speak from; state alerts (ready,
unconfigured) deliberately carry none.

---

## 12. Testing

- Hermetic unit tests throughout.
- `-tags=live` tests against real Postgres, **wired into CI from P0**. The tests that matter must not
  depend on someone remembering to export a connection string.
- Indexer tests run against committed repository fixtures, including one containing an escaping
  symlink.
- Load-bearing tests are mutation-tested: a test that cannot be broken on purpose is not evidence.
- The eval harness runs in CI on a fake embedder as a mechanics check only.

---

## 13. Phases

| Phase | Delivers |
| --- | --- |
| **P0** | Skeleton, schema, migrations, compose, CI including live Postgres, health/ready |
| **P1** | Ingestion: admission, sandbox, job queue, caps, LRU eviction |
| **P2** | AST chunking, embeddings, spans, window fallback |
| **P3** | Retrieval, citations, extractive ask, measured floor |
| **P4** | Symbol graph, per-edge provenance, graph endpoints |
| **P5** | React console |
| **P6** | Eval harness: generated golden set, AST versus window, calibration |
| **P7** | LLM tool loop, hybrid retrieval fusion, incremental re-index |
| **P8** | Terraform, CD, deploy |

Nine phases, not the eight estimated before this design existed — the ingestion subsystem and its
sandbox were not in that estimate.

---

## 14. Open questions

None blocking P0–P2. Deferred deliberately, to be decided with evidence rather than now:

- **The score floor's value.** Measured in P6; picking it earlier would be guessing.
- **Whether lexical fusion helps, and by how much.** An experiment in P6/P7, not an assumption.
- **The second language.** Not before Go is measured.

### Decided since

- **Which forges the default allowlist contains.** Settled in P1: `github.com` and
  `codeberg.org`, not `gitlab.com`. Both of the first two serve exactly `/owner/name`
  (Codeberg is Gitea), which is the shape the admission policy accepts. GitLab nests
  namespaces arbitrarily (`group/subgroup/repo`), so the policy refuses its typical URL;
  shipping it in the default allowlist would advertise a forge that half-works. Supporting
  it means changing the path check's shape and validating an arbitrary number of segments,
  which is its own task rather than a line in the allowlist. An operator can still
  configure `gitlab.com` explicitly, with that limitation.
