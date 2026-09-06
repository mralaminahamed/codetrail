# Where these fixtures come from

Every file here is written by `apps/gateway/internal/handler/fixtures_live_test.go`
(`//go:build live`) from the **shipped response structs** over a live Postgres.
`UPDATE_CONSOLE_FIXTURES=1` writes; without it the test compares and fails
naming the file, so CI's existing live step fails when a handler's shape
changes and the console has not been updated for it.

A hand-written fixture is a guess about the API that typechecks against a
hand-written type: two guesses agreeing with each other. So the class of every
file is recorded, because **a fixture the shipped stack cannot produce is a
fixture the guard cannot guard**, and calling it "emitted" would be this
directory lying about its own evidence.

| Class | What carried it |
| --- | --- |
| **a** | The real `*rag.Retriever` and the real `*store.Store`, end to end. |
| **b** | The shipped `handler.Handler` with **one interface** stubbed — `Retriever`, `Reader` or `Enqueuer`. A stub supplies a `rag.Result` or a `store` row; **never a JSON body**, so the response still comes out of the shipped structs, `detail()`, `floorView`, `citer` and `rag.NewCitation`. |
| **c** | A hand-written literal. Exactly one, and it says why below. |
| **d** | The shipped `Handler` **and** the shipped `Loop`, with the **provider** scripted (`llm.Fake`) and the loop's `agent.Corpus` in memory over real spans. Two seams, not one, which is why it is not class (b); it says below why neither can be closed. |

## Class (a) — the shipped path, end to end

`job-pending` `job-leased` `job-done` `job-failed` `repos` `repo` `repo-stale`
`span` `search-hybrid` `search-lexical` `ask-answered` `ask-refused-no-spans`
`ask-refused-no-spans-lexical` `ask-refused-below-floor` `symbols-one` `symbol`
`symbol-nospan` `callers-approx-empty` `citation-codeberg` `citation-nolink`
`error-400-scheme` `error-400-host` `error-400-form-path` `error-400-form-ref`
`error-400-depth` `error-404-repo` `error-404-job` `error-410-repo`

Three of those need a **configuration**, not a code path, and the emitter says
which:

- `ask-refused-below-floor` needs `ANSWER_SCORE_FLOOR` set. At the shipped
  default of `-1` this refusal is **unreachable**: `Decide` refuses when
  `top < f.Value` (`decide.go:86`) and a cosine similarity is never below `-1`.
  The emitter uses `0.99`. At a default deployment the only reachable refusals
  are `no_spans` and `unscored`.
- `search-hybrid` needs a small `RETRIEVAL_CANDIDATES` (the emitter uses `2`).
  At the default of 40 the vector arm returns every span in this corpus, so no
  hit has `vector_rank: 0` and the null-vs-real `vector_score` contrast the
  fixture exists for does not exist in the payload.
- `citation-nolink` needs a repository on a host `ALLOWED_HOSTS` admits and the
  `forges` table does not know. `Permalink` re-admits through `forgePolicy`,
  whose host set is the two-entry `forges` table (`cite.go:149-152`), and
  returns `""` on any failure — so an operator who allowlists a third forge
  gets a corpus every one of whose citations has `permalink: ""`.

## Class (b) — one interface stubbed

- **`ask-answered-empty`** — the `citations: null`, `answer: ""` shape.
  `Assemble`'s "the span did not travel" branch (`answer.go:83-89`) is **dead
  code under the shipped retriever**: `spansOf` builds its `want` set from the
  very hits it is handed and fills the map from the same arms
  (`retrieve.go:190-204`, called at `retrieve.go:124`), so `spans[h.SpanID]` is
  present for every hit, always. A stub returning `Result{Hits: […], Spans: {}}`
  is the only way to reach it. **This is itself a finding:** the branch is
  unreachable today, and a P7 tool loop that assembles from hits it did not
  retrieve alongside is exactly how it stops being.
- **`ask-refused-unscored`** — needs `math.IsNaN(top)` with `VectorRan` true
  (`decide.go:83-84`). The real retriever sets `TopScore = math.NaN()` only
  *before* the vector arm runs, and `VectorRan` is `r.Mode != ModeLexical`
  (`retrieve.go:92`), so the pair is unproducible from any query.
- **`symbols`** (two definitions of one name) and **`callers`** (a caller row
  whose `citation` is `null`, and one symbol name in both lists) — see the
  corpus limitation below.
- **`callers-approx-failed`** — `approximate.failed: true`. The block reports
  its own query's failure rather than returning it, because the precise callers
  are what was asked for (`graph.go:274-287`). Reaching it needs
  `ApproximateCallersOf` to fail, which a healthy Postgres does not do.

- **`error-500`** — a stubbed `Enqueuer` is the thing that fails. Its RESPONSE
  path is entirely shipped code (`h.fail` writes the body) and this file used to
  call it class (a) for that reason, in three places, while also saying at the
  bottom of the page that it needs a stub. Both cannot be true: the table's own
  definition of class (a) is "the real `*store.Store`, end to end", and a
  handler holding a deliberately broken `Enqueuer` is not that. It is class
  (b), and the earlier label was wrong rather than a matter of emphasis.

  The `request_id` is pinned through an inbound `X-Request-Id`, which echo's
  `RequestID` middleware honours; that is the only seam that makes a generated
  id reproducible.

  *The plan classified `callers`, `callers-approx-failed`, `symbols` and
  `error-500` as class (a). They are not; each needs a stub. Recorded rather
  than relabelled quietly.*

### The corpus limitation behind `symbols` and `callers`

The only corpus these suites have is the committed indexer fixture tree
(`apps/indexer/cmd/testdata/repo`). Measured against it:

- **no identifier is declared twice**, so a multi-definition list — fixture
  rule 5 — is not producible by any query;
- it holds **one** resolved call edge (`Total` → `Machine.Push` at
  `calc/use.go:7`);
- it holds **no unresolved call to any name it also defines**, so an
  `approximate` block with rows in it is not producible either — the only real
  syntactic edges point at `fmt.Sprintf`, which this repository does not define
  and therefore cannot be the subject of a `callers` query.

So `callers-approx-empty` is what the corpus really produces, and `callers` and
`symbols` supply their rows through a stubbed `Reader`. Every symbol in them is
a **real declaration** at its real path and line range, and every resolved
edge sits on a line that really holds that call. The rows that are not in the
corpus are: `Table` as a caller of `Machine.Push` at `big.go:6`, the two
`approximate` rows, and the second `Total`. Adding them to the indexer's
testdata would break `apps/indexer/cmd/index_live_test.go`, which pins that
tree span by span.

`Table` itself is **not** invented: `big.go`'s `var Table` is a declaration
longer than `CHUNK_MAX_DECL_LINES`, and it is **measured** that no span covers
its first line, so the shipped containment rule
(`apps/indexer/cmd/graph.go:125`) leaves it with `span_id: ""`. That is the
real "this declaration has no span" state `graph.go:37-39` keeps a key for, and
it is what `symbol-nospan.json` — class (a) — is emitted from.

## Class (d) — the answering loop, with the provider scripted

`ask-answered-llm` `ask-answered-degraded` `ask-refused-degraded`

**These three exist because the console had been ignoring two fields since P7.**
`read.go` puts `degraded` (`{from, reason}`) and `llm` (a trace) on **both**
`answerResponse` and `refusalResponse`. Nothing in the console declared, parsed
or rendered either — `git log -S "degraded" -- apps/console/` was empty — and
the reason nothing caught it is this directory: **no fixture existed for any of
the three shapes**, so the emitter produced only extractive answers and
refusals and the compare-and-fail guard had nothing to compare.

The consequence that guard was blind to is the one the root README calls "the
kind that costs a week before anyone notices": `read.go` stamps
`answered_by: "extractive"` on a **degraded** answer, which is the same value a
deployment with no model at all reports. A degraded model call and a plain
extractive answer were **byte-identical** to a client reading only that field,
and `Answer.tsx` read only that field.

### The two seams, and why neither can be closed

1. **The provider is scripted** (`llm.Fake`). A real provider call needs a
   vendor key and a network hop out of CI, and there is no configuration of the
   shipped stack that reaches `read.go`'s answering branch without one. `llm.Fake`
   is the shipped double for the `llm.Model` interface and nothing else in the
   path is faked: the `Loop`, its spend controls, `agent.Run`, the four tools,
   the citation gate, `cited()`, `rag.NewCitation` and the response structs are
   all production code.
2. **The loop's `agent.Corpus` is in memory**, holding **real spans read out of
   the live store** immediately before the loop runs. Not for convenience:
   `agent.Trace` records a per-tool duration as
   `time.Since(start).Milliseconds()`, and that is the **only value in an /ask
   response that is not a function of its inputs**. A Postgres round trip lands
   on either side of 1 ms depending on the machine, which would make the
   committed `"ms"` drift and fail this guard for a reason that is not a handler
   change. A map lookup is microseconds, so the field is `0` — and the emitter
   **asserts** `"ms":0` rather than hoping for it, so the day that stops being
   true the emitter says so instead of the guard flapping.

   The span in `ask-answered-llm.json` is otherwise entirely real: its id, path,
   line range, text and digest all come out of the shipped store, so the
   citation the model "wrote" is one a reader can check byte for byte.

### What each of the three is for

- **`ask-answered-llm`** — `answered_by: "llm"`, a trace whose `stop` is
  `final`, and **no `degraded` key at all**. `degraded` is present IFF the loop
  was attempted and did not write the answer (`read.go:67-71`), so its absence
  here is a property of the fixture and the emitter asserts it with a
  `mustNotContain`.
- **`ask-answered-degraded`** — the pair that makes the defect visible. Same
  `answered_by: "extractive"` as `ask-answered.json`, same extractive prose,
  same three citations; the only difference is the `degraded` block and the
  trace. A console that renders the two identically is rendering a silent
  downgrade, and now a test can say so. Driven by a `rate_limited` failure on
  turn 1, so the trace has `tools: null` — a shape the parser must survive,
  since every other list in this API is allocated with `make`.
- **`ask-refused-degraded`** — a refusal carrying `degraded` and `llm`. It needs
  **both** a floor that refuses and `LLM_BELOW_FLOOR=true`, so no query can
  produce it. `read.go:443-448` is what it pins: "the degraded loop does not
  inherit the floor's permission to be ignored", without which `LLM_BELOW_FLOOR`
  would turn every degradation into an answer to a question the floor refused.

## Class (c) — the one hand-written literal

- **`citation-badscheme`** — a citation whose `permalink` is not `https:`.
  **The shipped code cannot produce one.** `Permalink` re-admits the remote
  through `admit.Check` (`cite.go:180-183`), which refuses any non-`https`
  scheme (`admit/admit.go:77-78`), so no configuration reaches it. The file
  exists to prove the console's `href` guard is wired, and the guard is defence
  against a future `forges` entry or a future relaxation of `admit` — **not**
  against anything reachable today. It is `span.json` with one field replaced,
  so every other value in it is still emitted.

## What pins the timestamps, and why there are four seams and not one

1. `Handler.Now` — the clock a citation and a staleness note are dated
   *against*. A field for exactly this (`handler.go:36-38`).
2. A SQL `UPDATE` of `repos.indexed_at` and `repos.last_queried_at` — the row
   the note is computed *from*. `PutRepo` and `TouchRepo` write `now()` in SQL
   (`packages/shared/store/repos.go`), where no Go field reaches.
3. A SQL `UPDATE` of `jobs.id` — `Enqueue` mixes `time.Now().UnixNano()` into
   the id (`jobs.go:92`).
4. `time.Local`, pinned to UTC for the duration of the emitter. **Measured:**
   pgx decodes a `timestamptz` into `time.Local` and `encoding/json` renders a
   `time.Time` with its location's offset, so the same instant from the same
   handler emitted `"2026-08-31T18:00:00+06:00"` on a `+06` machine and
   `"2026-08-31T12:00:00Z"` on a UTC one. The instant is untouched; only the
   location it renders in is pinned.

None of these **normalises a timestamp out of** a fixture. That distinction is
the point: the staleness sentence embeds an age computed from `indexed_at`, the
console pins that sentence verbatim, and a normalised timestamp would blind
this guard to the one drift it exists to catch.
