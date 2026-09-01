# codetrail P3 — retrieval, citations, extractive ask, measured floor

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the spans P2 wrote answer a question, and make the answer checkable. Hybrid retrieval over one repository, a citation that renders as an immutable forge permalink and says what it does *not* know about staleness, an extractive answer assembled from ranked spans, and a refusal that is counted as a refusal rather than as an error.

**Architecture:** A new `packages/shared/rag` holds the parts that are pure functions — reciprocal-rank fusion, the floor decision, query tokenisation, permalink rendering, answer assembly — plus the `Retriever` that runs the two arms and fuses them. The two arms are SQL and live in `packages/shared/store`: `VectorSearch` (pgvector cosine, filtered by repo) and `LexicalSearch` (a weighted `tsvector` over `symbol` and `text`). The gateway grows read endpoints, its own embedder (it has to embed the query), and the metrics spec §11 lists for retrieval and answers. `store.Evict` starts leaving a tombstone so an evicted repo can answer `410` rather than `404`, and the `jobs` table finally gets the retention rule §14 deferred to this phase.

**Tech Stack:** Go 1.27, pgvector on Postgres 17, Postgres full-text search (`tsvector`/`to_tsquery`, `simple` configuration), echo v4, zerolog, prometheus/client_golang. **No new third-party dependencies.** The lexical arm is a generated column and a GIN index, not a search engine.

**Spec:** `docs/superpowers/specs/2026-08-31-codetrail-design.md` — read **§8 (retrieval, citations, answering)** first, then **§10 (error handling)**, then §11 (observability), §3 (data model), §13 (phases) and §14 (open questions). Spec lines 213–235 and 247–262 are the whole of this phase's mandate; read line 315 and 316 twice, because they are the two things this plan is forbidden to pretend it has done.

**Predecessor:** `docs/superpowers/plans/2026-09-01-p2-chunking.md`. P2 merged at `75ab376`. This plan matches its shape deliberately, and changes one thing about it — the mutation tables become mutation *blocks* — for the reason set out in "How to mutate".

---

## Two decisions the spec has already made, which this plan may not re-open

These are not open questions. They are settled against P3, and the failure mode for each is a plan that quietly ships the opposite.

### 1. The floor's value is measured in P6. P3 ships the mechanism and the instrument.

Spec:315 — "**The score floor's value.** Measured in P6; picking it earlier would be guessing."

So P3 builds: a configurable floor, the refusal path, the metric that separates a refusal from an error, and the **top-score histogram P6 calibrates from**. It must not ship a number dressed up as calibrated.

Three ways to get this wrong, and what this plan does instead:

- **Ship a plausible default (`0.35`, `0.4`).** Forbidden. A reader cannot tell a guess from a measurement once it is a default in a table.
- **Ship `0`.** Nearly as bad: it looks like a threshold, it is not one, and cosine similarity is defined on `[-1, 1]` so `0` does filter — it refuses a negatively-similar top hit, which is a real filter nobody measured.
- **Make the floor required at boot.** That pushes the guess onto whoever deploys it, which is worse than making it ourselves and worse than not making it.

**What this plan does:** the floor defaults to **`-1`** — the minimum of the cosine range, and therefore the one value that is visibly *not* a threshold: it cannot exclude anything, and no reader can mistake it for a tuned number. Every place the floor is visible says so:

- the answer payload carries `"floor": {"value": -1, "calibrated": false}`;
- the gateway logs one line at boot naming the value and that it is uncalibrated;
- `codetrail_score_floor` and `codetrail_score_floor_calibrated` are gauges, so a dashboard shows a guess as a guess;
- the README says the floor is a mechanism in P3 and a number in P6.

**The feature is still testable, because the refusal path is live at the default.** Two things refuse independently of the floor's value: a query that retrieves nothing at all (`no_spans`), and a top score that is not a number (`unscored` — pgvector answers `NaN` for a zero vector, measured in P2). And the floor's own arithmetic is unit-tested by *passing a floor*, not by shipping one: `Decide` is a pure function and the tests hand it `0.5` with a top score of exactly `0.5`.

**Where the floor is compared is itself a decision, and the spec's phrasing hides it.** §8 says "when the top score falls under a floor". After reciprocal-rank fusion there is no such number: an RRF score is a function of *ranks alone*, so the top hit of any non-empty result set scores `1/(k+1)` whether it is a perfect match or noise. A floor on the fused score is a floor on "did anything come back", which is the `no_spans` check with extra arithmetic. So **the floor is compared against the cosine similarity of the best-ranked span in the vector arm**, which is the only number in the pipeline that carries quality and is bounded and comparable across queries. Task 1 pins that with a test that asserts the fused score is *identical* for a perfect arm and a worthless one.

Consequence, stated rather than discovered: in `RETRIEVAL_MODE=lexical` there is no cosine similarity, so the floor **does not apply** and the response says `"applicable": false`. `ts_rank_cd` is unbounded and corpus-dependent; comparing it to a cosine floor would be a category error that happens to typecheck.

### 2. Whether lexical fusion helps is an experiment for P6/P7.

Spec:316 — "**Whether lexical fusion helps, and by how much.** An experiment in P6/P7, not an assumption."

So fusion is built to be **measured and switched**:

- `RETRIEVAL_MODE` ∈ `{vector, lexical, hybrid}`, validated at boot. The default is `hybrid` because §8 *defines* retrieval as hybrid — that is a spec-conformance choice, not a quality claim, and the README says so in those words.
- Every hit carries `vector_rank` and `lexical_rank` in the response, so an arm's contribution is readable from the data rather than inferred.
- The fusion constant `k` and the per-arm candidate depth are knobs, so P6 sweeps them without a code change.
- Nothing in the plan, the code comments, the commit messages or the README may say fusion retrieves better. **CI cannot even measure it**: CI runs on `embed.Fake`, a hashed bag of words over the same span text the lexical arm indexes, so in CI the two arms are near-duplicates of one function. That is a reason the end-to-end tests prove wiring and not quality, and it must be written down where someone reading a green build can see it.

---

## Global Constraints

- Go **1.27**; module `github.com/mralaminahamed/codetrail`.
- Default branch is **`trunk`**. Branch from it, one PR per task, **merge commits, never squash**. Never `--no-verify`.
- Commit author and committer must be `Al Amin Ahamed <alamin.ahamed.dev@gmail.com>`. The pre-push hook enforces it; do not bypass it.
- Comments are **minimal and short** — explain *why*, never restate *what*.
- Line numbers are **1-based and inclusive** (spec §3). Every citation this phase renders is a `#L<start>-L<end>` anchor over that convention; an off-by-one is now visible to a user, not only to a test.
- `gofmt -l apps packages` empty and `go vet ./...` clean before every commit.
- **No question text in a log line, ever.** Metrics labels are closed sets (`packages/shared/metrics` says so) and the same rule applies to logs here: a query is user input on a public endpoint, and it is the one string in this phase that must not end up in an operator's log aggregator. A request id is what ties a report to a line.
- Migration numbering: Tasks 3 and 5 each add one. Whichever lands first takes `0008`; the other **renumbers before merge**. Two migrations with the same number is a silent skip, because the ledger is keyed on the name.

---

## How to mutate, and what counts as a kill

P1 and P2 ran roughly 250 mutations between them. The audit of the two phases found: **~30 predicted failure messages wrong**, **6 void mutations** (hung, panicked before the assertion, broke only the build, or produced a protocol error instead of a behaviour change), **5 tests that passed whether or not the code under test ran**, and **2 mutations derived from a false rationale** — the plan's stated reason for the code was wrong, so the mutation proved nothing about anything.

Every rule below exists because of one of those. Read them before Task 1's mutation step.

1. **Commit before you mutate.** `git checkout -- <file>` restores from `HEAD`; a mutation reverted against a dirty tree restores the mutant. `git status --porcelain` must be empty before the first `sed`.
2. **The mutant must compile and vet.** `go build ./... && go vet ./...` after applying it. A mutation that only breaks the build is a test of the compiler. This bites hardest in SQL: a mutation that makes a statement syntactically invalid, or that unbinds a parameter, produces a pgx error rather than a different ranking — **that is void**, it is P1's mistake with a new spelling. A SQL mutation must produce *rows in a different order or a different set*, not an error.
3. **Predicted output is an expectation, not a fact.** Every block below says what the failure is expected to look like. The implementer runs it, **pastes the observed output into the plan**, and corrects the prediction. A row that still reads as a prediction after the task is done is an unfinished task.
4. **Name the fixture that can tell the mutant from the original, and say why it can.** A markdown file cannot test a `.go` check; unit vectors cannot tell cosine from L2. See the next section for the retrieval-specific version, which is where this phase will otherwise waste its mutation budget.
5. **State the rationale for the code you are mutating.** If the stated reason is wrong, the mutation tests nothing — that happened twice already. A reviewer must be able to read "why the code exists" and disagree with it before the mutation is run.
6. **If you cannot describe the failing output, the test is not evidence.** "It would hang" or "it would be flaky" means redesign the test until the failure is a comparison against a value.
7. **A kill is an assertion failing.** A `t.Fatal(err)` on the way to the assertion, a panic, a nil dereference or a timeout is an incident. Fix the test so its own claim is what fails, then re-run.
8. **A comment claiming safety is the highest-risk line in the file.** P2 corrected 12 false comments. Where this plan says "record why", the rule is: run the counterfactual, paste the observed output into the commit message, then write the comment. If the counterfactual passes, the claim is false — delete it.

**Format change from P1 and P2.** Those phases used a four-column table, and four columns could not carry the fixture and the rationale, which is part of why ~30 predictions shipped wrong. P3 uses a block per mutation:

```
**M<n> — <one-line description of the edit>.**
- *Why the code exists:* <the rationale a reviewer can disagree with>
- *Fixture that separates mutant from original:* <which fixture, and why it can>
- *Must fail:* <test name>
- *Expected (verify and correct):* <message>
- *Compiles and vets:* <why the mutant is a behaviour change, not a build break>
```

---

## Fixtures that can tell a retrieval bug from a passing test

Retrieval is the easiest subsystem in this project to test vacuously. Five rules, each of which invalidates a test that would otherwise look fine. They apply to **every** retrieval fixture in this phase.

1. **A single-document corpus cannot detect a ranking bug.** Every retrieval fixture holds at least three spans with a known, non-obvious intended order.
2. **The expected order must disagree with insertion order, path order, and span-id order.** A query that lost its `ORDER BY` returns rows in physical order, which is insertion order; a fixture whose best answer was inserted first passes anyway. Build the fixtures so the correct answer is neither first-inserted nor first-alphabetically.
3. **Every retrieval fixture holds two repositories.** A missing `WHERE repo_id = $1` is otherwise invisible, and the other repo's spans must be *better* matches than the target repo's, so the filter is load-bearing rather than decorative.
4. **Unit vectors cannot tell cosine from L2 — and they cannot tell cosine from inner product either.** The vector fixture uses deliberately non-unit vectors chosen so that cosine order, L2 order and inner-product order are three different orders. Task 2 gives the exact vectors.
5. **A query that lexically matches its own answer cannot tell the vector arm from the lexical one.** And under `embed.Fake` — a hashed bag of words over the same text — *no* end-to-end query can, because both arms are then computing lexical overlap. Therefore: **arm separation and fusion are tested on synthetic ranked lists with no embedder and no database** (Task 1), the arms are tested individually against hand-written rows (Tasks 2 and 3), and the end-to-end tests prove wiring only. Any test that claims to show fusion working end to end is measuring the fake.

One more, from the fake's own shape: `embed.Fake` refuses a text it can hash no token from. A query of `"???"` therefore returns an *error* from the embedder unless the handler rejects it first — which is why query validation is at the edge and returns `400`, not a `500` from three layers down.

---

## What P3 carries forward from P1 and P2

Each of these is a real constraint on the design, not context.

- **`RepoID` and `SpanID` have no room for a strategy, and the eval's two arms therefore live in separate databases.** Nothing in P3 may foreclose that: the retriever takes a store, the store takes a DSN, and no query joins across arms or assumes one database holds both. The per-repo embedder guard (Task 6) is computed from the rows, so it works in either database.
- **`kind=file` does not identify which arm produced a row.** Sub-windowed declarations and unparseable files carry it under the AST strategy too. So **retrieval never filters, boosts or explains by `kind`**, and no response field says which arm produced a span. It is a property of the run.
- **`doc.go` and `tools.go` produce zero AST spans** — `f.Doc` is not a `Decl`, and imports are dropped. Retrieval sees this directly: in the production (AST) corpus, a question about a package's documentation retrieves nothing and gets a refusal, while the same repository under `CHUNK_STRATEGY=window` answers it. This is a real corpus asymmetry, P2 measured it, and P3's job is to make it *visible* (Task 8 pins it with a test) rather than to paper over it with a `kind` heuristic.
- **`chunk.Chunks` returns `([]Chunk, int, error)`** — the int is dropped token-less regions. The consequence for P3 is that a repository's span count is not derivable from its file count, so the repo view reports `files`, `spans` and `files_with_spans` separately instead of implying coverage.
- **`jobs` retention was deferred to P3** (§4, §14). Task 5 decides it. The cheap bound — dropping terminal jobs when the same repository is re-enqueued — is refused, and the reason is written down: a caller holding a job id would get a `404` for a job that ran, at a moment chosen by an unrelated stranger's submission. Retention is by **age**, so what a caller may poll for is a stated duration rather than a race.
- **A citation's digest is what makes it checkable.** P2 verified 1,303 spans byte-for-byte against `git show`. Task 8 repeats that check *against a citation the API returned*, and Task 7 makes it structurally impossible for the assembler to break: an answer never truncates a span's text, because a truncated span under an untruncated digest is a citation that lies.

---

## Task independence

- **Tasks 1, 2, 3, 4 and 5 are independent** and can be worked in parallel from `trunk`.
  - Task 1 creates `packages/shared/rag` (pure functions only).
  - Task 2 creates `packages/shared/store/search.go`.
  - Task 3 creates `packages/shared/store/lexical.go` + a migration.
  - Task 4 adds files to `packages/shared/rag` and one query to `store`.
  - Task 5 modifies `store/evict.go`, `jobs`, the indexer + a migration.
  - Tasks 3 and 5 both add a migration: first to land takes `0008`, the other renumbers.
  - Tasks 1 and 4 share a package but no file. Land Task 1 first if both are ready, so `rag`'s package doc lands once.
- **Task 6** depends on 1, 2, 3.
- **Task 7** depends on 4, 5, 6.
- **Task 8** depends on everything.

---

### Task 1: `packages/shared/rag` — ranked lists, reciprocal-rank fusion, and the floor decision

Everything in this task is a pure function over synthetic inputs: no database, no embedder, no clock. That is deliberate and it is the only place in the phase where fusion and the floor can be tested at all — see rule 5 above. It also means this task is the one that can prove the claim the whole floor design rests on: **a fused score carries no quality signal**.

**Files:**
- Create: `packages/shared/rag/rag.go`, `packages/shared/rag/fuse.go`, `packages/shared/rag/decide.go`
- Test: `packages/shared/rag/fuse_test.go`, `packages/shared/rag/decide_test.go`

**Interfaces:**
- Consumes: standard library only (`math`, `sort`).
- Produces:
  - `type Arm string`, `ArmVector = "vector"`, `ArmLexical = "lexical"`
  - `type Mode string`, `ModeVector`, `ModeLexical`, `ModeHybrid`; `func ParseMode(string) (Mode, error)`
  - `type Hit struct { SpanID, Path string; StartLine int; Score float64 }` — one arm's result in that arm's own units
  - `type Fused struct { SpanID, Path string; StartLine int; Score, VectorScore float64; VectorRank, LexicalRank int }`
  - `func Fuse(k int, vector, lexical []Hit) []Fused`
  - `type Floor struct { Value float64; Calibrated bool }`; `func (Floor) Validate() error`; `func DefaultFloor() Floor`
  - `type Outcome string` (`OutcomeAnswered`, `OutcomeRefused`); `type Reason string` (`ReasonNoSpans`, `ReasonBelowFloor`, `ReasonUnscored`)
  - `func Decide(f Floor, hits int, top float64, vectorRan bool) (Outcome, Reason)`

**Decisions, with their reasoning:**

- **`Fuse` takes the two arms as named parameters, not a `map[Arm][]Hit`.** Two reasons, and only the second is obvious: the arm set is closed by spec §8, and a map would make the fusion loop's iteration order random, so a tie among fused scores would resolve differently on every call. The tie-break sort would still fix that — but a determinism bug that is *masked* by a sort is one that returns the moment someone changes the sort.
- **Fusion reads ranks, never scores.** That is what reciprocal-rank fusion *is*, and it is also the only thing that makes two arms with incomparable units combinable: a cosine similarity in `[-1,1]` and a `ts_rank_cd` in `[0,∞)` have no exchange rate.
- **`Fused.VectorScore` is carried through anyway.** It is the number the floor reads and the number P6 calibrates from, and losing it here would mean re-querying to get it back.
- **Absent means rank 0, not rank `len+1`.** A span the lexical arm never returned contributes nothing to the sum. Ranking it last-plus-one would give every document a lexical contribution and quietly turn "fused" into "vector, damped".
- **Ties break by `(path, start_line, span_id)`.** Deterministic, so two runs over an unchanged corpus are diffable, and readable, so a human comparing two results sees a file order rather than a hash order. Span id is the last resort because a hash orders nothing a person can follow.
- **`Decide` refuses on a `NaN` top score.** `NaN < floor` is `false` in Go, so the natural spelling of the check *answers* on a NaN. P2 measured that pgvector returns `NaN` for the cosine distance of a zero vector; a span with a zero embedding is prevented at the chunker, and this is the second line of defence at the place where the failure would otherwise be a confident answer.
- **`Decide` is inclusive at the floor:** it refuses when `top < floor`, so a score exactly equal to the floor answers. Arbitrary, but it has to be pinned somewhere or the boundary drifts; the tests fix it.

- [x] **Step 1: Write the failing tests**

`fuse_test.go`:

```go
package rag

import "testing"

// hits builds an arm's ranked list. Scores descend so the list is already in
// the order the arm would return it; Fuse must not re-sort by score.
func hits(ids ...string) []Hit {
	out := make([]Hit, len(ids))
	for i, id := range ids {
		out[i] = Hit{SpanID: id, Path: id + ".go", StartLine: 1, Score: 1 - float64(i)/100}
	}
	return out
}

func ids(fs []Fused) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.SpanID
	}
	return out
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// The fixture is built so the fused order matches neither arm's own order and
// neither alphabetical nor insertion order: vector is [A B C D], lexical is
// [C A E], and the answer is [A C B E D]. A Fuse that returns one arm verbatim,
// or that sorts by id, or that keeps insertion order, fails here.
//
// k=60: A=1/61+1/62=0.0325224, C=1/63+1/61=0.0322664, B=1/62=0.0161290,
// E=1/63=0.0158730, D=1/64=0.0156250.
func TestFuseIsReciprocalRankOverBothArms(t *testing.T) {
	got := Fuse(60, hits("A", "B", "C", "D"), hits("C", "A", "E"))
	eq(t, ids(got), []string{"A", "C", "B", "E", "D"})
	if got[0].VectorRank != 1 || got[0].LexicalRank != 2 {
		t.Fatalf("A has ranks v=%d l=%d, want v=1 l=2", got[0].VectorRank, got[0].LexicalRank)
	}
	// E was never in the vector arm: absent is 0, not last-plus-one.
	last := got[3]
	if last.SpanID != "E" || last.VectorRank != 0 {
		t.Fatalf("E has vector rank %d, want 0", last.VectorRank)
	}
}

// k is the whole of what separates RRF from "sum of 1/rank", and it changes the
// answer exactly where one arm ranks a document very deep. X is (1st, 20th) and
// Y is (2nd, 2nd): with k=60 the deep rank is heavily discounted and Y wins;
// with k=0 the top rank dominates and X wins. A fixture where both arms agree
// cannot show this, because k then scales every score identically.
func TestKChangesTheOrderWhereItShould(t *testing.T) {
	vec := hits("X", "Y")
	lex := make([]Hit, 20)
	lex[0] = Hit{SpanID: "Z0", Path: "z0.go"}
	lex[1] = Hit{SpanID: "Y", Path: "Y.go"}
	for i := 2; i < 19; i++ {
		lex[i] = Hit{SpanID: "pad" + string(rune('a'+i)), Path: "pad.go"}
	}
	lex[19] = Hit{SpanID: "X", Path: "X.go"}

	if got := ids(Fuse(60, vec, lex))[0]; got != "Y" {
		t.Fatalf("k=60 ranked %q first, want Y", got)
	}
	if got := ids(Fuse(0, vec, lex))[0]; got != "X" {
		t.Fatalf("k=0 ranked %q first, want X", got)
	}
}

// Two documents at ranks (1,2) and (2,1) fuse to exactly the same score. The
// order must then come from the tie-break, not from map iteration or from the
// order the arms were walked in. P is walked first and sorts second, and P's id
// also sorts before Q's, so a tie-break on id alone fails here too.
func TestTiesBreakOnPathAndLineNotOnWalkOrder(t *testing.T) {
	p := Hit{SpanID: "aaa", Path: "a.go", StartLine: 10}
	q := Hit{SpanID: "bbb", Path: "a.go", StartLine: 5}
	for i := 0; i < 50; i++ {
		got := ids(Fuse(60, []Hit{p, q}, []Hit{q, p}))
		eq(t, got, []string{"bbb", "aaa"})
	}
}

// Vector-only mode is Fuse with one arm, and it must be order-preserving: RRF
// over a single list is a monotone function of rank, so anything that reorders
// it is a bug in the sum rather than a policy.
func TestSingleArmFusionPreservesThatArmsOrder(t *testing.T) {
	eq(t, ids(Fuse(60, hits("A", "B", "C"), nil)), []string{"A", "B", "C"})
	eq(t, ids(Fuse(60, nil, hits("C", "B", "A"))), []string{"C", "B", "A"})
}

// The claim the floor design rests on: a fused score says nothing about
// quality. Two runs with identical rank order and wildly different arm scores
// produce identical fused scores, so a floor on Score would be a floor on "did
// anything come back at all".
func TestFusedScoreCarriesNoQualitySignal(t *testing.T) {
	good := []Hit{{SpanID: "A", Path: "a.go", Score: 0.99}, {SpanID: "B", Path: "b.go", Score: 0.98}}
	junk := []Hit{{SpanID: "A", Path: "a.go", Score: 0.01}, {SpanID: "B", Path: "b.go", Score: 0.002}}
	g, j := Fuse(60, good, nil), Fuse(60, junk, nil)
	if g[0].Score != j[0].Score {
		t.Fatalf("a good arm fused to %v and a worthless one to %v", g[0].Score, j[0].Score)
	}
	// And the number the floor actually reads does differ.
	if g[0].VectorScore == j[0].VectorScore {
		t.Fatalf("both arms carried VectorScore %v", g[0].VectorScore)
	}
}
```

`decide_test.go`:

```go
package rag

import (
	"math"
	"testing"
)

// Nothing retrieved is a refusal, not an answer over an empty citation list —
// and not an error (spec §10). It refuses at the shipped default too, which is
// what keeps the refusal path live before P6 measures a floor.
func TestNothingRetrievedRefusesEvenAtTheDefaultFloor(t *testing.T) {
	out, why := Decide(DefaultFloor(), 0, math.NaN(), true)
	if out != OutcomeRefused || why != ReasonNoSpans {
		t.Fatalf("got %s/%s, want refused/no_spans", out, why)
	}
}

// The boundary has to be pinned somewhere: equal to the floor answers.
func TestTheFloorIsInclusive(t *testing.T) {
	f := Floor{Value: 0.5}
	if out, why := Decide(f, 3, 0.5, true); out != OutcomeAnswered {
		t.Fatalf("a top score of exactly 0.5 gave %s/%s, want answered", out, why)
	}
	if out, why := Decide(f, 3, 0.49999999, true); out != OutcomeRefused || why != ReasonBelowFloor {
		t.Fatalf("just under the floor gave %s/%s, want refused/below_floor", out, why)
	}
}

// NaN < floor is false, so the obvious spelling of the check answers on a score
// that is not a number. pgvector returns NaN for the cosine distance of a zero
// vector (measured in P2), and a confident answer is the worst possible
// response to that.
func TestANaNTopScoreRefuses(t *testing.T) {
	out, why := Decide(Floor{Value: -1}, 3, math.NaN(), true)
	if out != OutcomeRefused || why != ReasonUnscored {
		t.Fatalf("got %s/%s, want refused/unscored", out, why)
	}
}

// The floor is defined on cosine similarity. In lexical-only mode there is no
// cosine similarity, and ts_rank_cd is unbounded, so the floor does not apply
// and only an empty result can refuse.
func TestTheFloorDoesNotApplyWhenTheVectorArmDidNotRun(t *testing.T) {
	if out, _ := Decide(Floor{Value: 0.9}, 3, math.NaN(), false); out != OutcomeAnswered {
		t.Fatalf("a lexical-only result was refused by a cosine floor")
	}
}

// The shipped default is not a threshold: -1 is the bottom of the cosine range
// and cannot exclude anything. If this test starts failing because the default
// moved, the README's "not calibrated" claim moved with it.
func TestTheShippedDefaultRefusesNothingOnScoreAlone(t *testing.T) {
	f := DefaultFloor()
	if f.Calibrated {
		t.Fatal("the default floor claims to be calibrated")
	}
	if out, why := Decide(f, 1, -1, true); out != OutcomeAnswered {
		t.Fatalf("the worst possible cosine gave %s/%s at the default floor", out, why)
	}
}

// Fail closed like chunk.Options and walk.Limits: a floor outside the cosine
// range is a misconfiguration, not a preference, and NaN is one a comparison
// would silently accept.
func TestFloorValidateRefusesWhatCosineCannotProduce(t *testing.T) {
	for _, v := range []float64{1.0001, -1.0001, math.NaN(), math.Inf(1)} {
		if err := (Floor{Value: v}).Validate(); err == nil {
			t.Fatalf("floor %v was accepted", v)
		}
	}
	for _, v := range []float64{-1, 0, 0.42, 1} {
		if err := (Floor{Value: v}).Validate(); err != nil {
			t.Fatalf("floor %v: %v", v, err)
		}
	}
}
```

- [x] **Step 2: Implement**

`fuse.go`, in outline — the sum, then one sort:

```go
// Fuse combines the arms by reciprocal rank: score = Σ 1/(k + rank), over the
// arms that returned the span, ranks 1-based.
//
// Ranks, not scores, because the two arms have no common unit — a cosine
// similarity and a ts_rank_cd cannot be added, and normalising them would
// invent an exchange rate nobody measured. Spec §8 names this fusion; spec:316
// says whether it beats the vector arm alone is P6/P7's experiment, so every
// hit keeps the per-arm ranks that make the comparison possible.
//
// The returned Score is a function of ranks alone. It is deliberately not the
// number the floor reads: the top hit of any non-empty result scores 1/(k+1)
// whether it is perfect or worthless.
func Fuse(k int, vector, lexical []Hit) []Fused
```

Accumulate into a `map[string]*Fused` keyed by span id, walking `vector` then `lexical`; carry `VectorScore` from the vector arm only; collect the values into a slice; `sort.Slice` on `Score` descending then `Path`, `StartLine`, `SpanID` ascending.

`decide.go`:

```go
// Decide is the grounded-or-refused rule (spec §8), and the reason it returns
// is a metric label: spec §10 requires a refusal and an error to be distinct
// outcomes, so "we had nothing to say" never hides inside an error rate.
//
// top is the cosine similarity of the best-ranked span in the vector arm, not
// the fused score — see Fuse. vectorRan is false in lexical-only mode, where
// there is no such number and the floor therefore does not apply.
func Decide(f Floor, hits int, top float64, vectorRan bool) (Outcome, Reason) {
	if hits == 0 {
		return OutcomeRefused, ReasonNoSpans
	}
	if !vectorRan {
		return OutcomeAnswered, ""
	}
	// Explicit, because NaN < f.Value is false and the natural spelling of the
	// floor check therefore answers on a score that is not a number.
	if math.IsNaN(top) {
		return OutcomeRefused, ReasonUnscored
	}
	if top < f.Value {
		return OutcomeRefused, ReasonBelowFloor
	}
	return OutcomeAnswered, ""
}

// DefaultFloor is -1: the bottom of the cosine range, and so the one value that
// cannot be mistaken for a tuned threshold. Spec:315 puts the number in P6,
// measured from the eval's distribution; shipping a plausible default here
// would be a guess wearing a measurement's clothes.
func DefaultFloor() Floor { return Floor{Value: -1, Calibrated: false} }
```

- [x] **Step 3: Run**

`go test ./packages/shared/rag/ -count=1 -v` — expect PASS.

- [x] **Step 4: Commit, then prove the tests discriminate**

`git status --porcelain` must be empty first.

**M1 — `1/float64(k+rank)` → `1/float64(rank)` (k dropped).**
- *Why the code exists:* `k` is the discount that stops a single arm's top rank from dominating the fusion; it is the parameter P6 sweeps.
- *Fixture that separates mutant from original:* the `(1st, 20th)` versus `(2nd, 2nd)` fixture in `TestKChangesTheOrderWhereItShould`. A fixture where both arms rank the same documents in the same order cannot: `k` then scales every score by the same monotone function and the order is unchanged.
- *Must fail:* `TestKChangesTheOrderWhereItShould`
- *Observed:* killed. `fuse_test.go:71: k=60 ranked "X" first, want Y` — prediction correct.
- *Compiles and vets:* yes; it is an arithmetic change with the same types.

**M2 — fuse on the arm's score instead of its rank: `1/float64(k+rank)` → `h.Score`.**
- *Why the code exists:* the two arms' units are not comparable, so ranks are the only common currency.
- *Fixture that separates mutant from original:* `hits()` gives descending scores `1, 0.99, 0.98…` in both arms, so summing scores ranks by "how high each arm put it" with a *different* discount curve than RRF. In `TestFuseIsReciprocalRankOverBothArms`: A=1+0.99=1.99, C=0.98+1=1.98, B=0.99, E=0.98, D=0.97 — **which is the same order**. Expected to survive on that test. The kill is `TestFusedScoreCarriesNoQualitySignal`, whose two fixtures have identical rank order and different scores by construction.
- *Must fail:* `TestFusedScoreCarriesNoQualitySignal`
- *Observed:* killed. `fuse_test.go:133: a good arm fused to 0.99 and a worthless one to 0.01` — prediction correct. **The predicted survival held**: `TestFuseIsReciprocalRankOverBothArms` passed under the mutant, exactly as this block says it would. Two incidental kills the block did not predict, both because a hand-built `Hit` literal leaves `Score` at 0 while `hits()` fills it: `fuse_test.go:71: k=60 ranked "X" first, want Y`, and `fuse_test.go:121: got [alpha zulu3 mike4 delta nine], want [nine delta alpha zulu3 mike4]` (every span scores 0, so the whole order falls to the tie-break).
- *Compiles and vets:* yes.
- *Note:* this mutation is listed with its predicted **survival** on the first test on purpose. A mutation that survives the test you expected to kill it is the single most useful thing a mutation round produces, and dropping it quietly is how a plan grows a test that proves nothing.

**M3 — delete the tie-break, sorting on `Score` alone.**
- *Why the code exists:* two runs over an unchanged corpus must be diffable; without it the order among equal scores comes from map iteration.
- *Fixture that separates mutant from original:* `TestTiesBreakOnPathAndLineNotOnWalkOrder`, whose two documents fuse to bit-identical scores. Any fixture with distinct scores cannot detect this at all.
- *Must fail:* `TestTiesBreakOnPathAndLineNotOnWalkOrder`
- *Observed:* killed on the first run; no loop-count increase needed. `fuse_test.go:87: got [aaa bbb], want [bbb aaa]`. Predicted as: `got [aaa bbb], want [bbb aaa]` — on *some* iteration of the 50. Go's `sort.Slice` is not stable and map iteration is randomised, so the failure is overwhelmingly likely rather than certain: 50 draws that all happen to come out in the intended order is roughly `2^-50`. **If it passes, do not record a kill** — record what happened and raise the loop count, because the claim being tested is determinism and a flaky kill is not evidence of it.
- *Compiles and vets:* yes.

**M4 — tie-break on `SpanID` only (drop `Path`/`StartLine`).**
- *Why the code exists:* a human comparing two result sets reads file order, not hash order.
- *Fixture that separates mutant from original:* the same fixture, built so `aaa` sorts before `bbb` by id but *after* it by `(path, start_line)`. A fixture whose ids happen to agree with its paths cannot tell the two rules apart.
- *Must fail:* `TestTiesBreakOnPathAndLineNotOnWalkOrder`
- *Observed:* killed. `fuse_test.go:87: got [aaa bbb], want [bbb aaa]` — prediction correct, and deterministic as stated.
- *Compiles and vets:* yes.

**M5 — absent arm scored as `len(list)+1` instead of skipped.**
- *Why the code exists:* a span one arm never returned must contribute nothing; giving it a synthetic worst rank turns fusion into "vector, damped by a constant".
- *Fixture named in the plan:* `TestFuseIsReciprocalRankOverBothArms`. **It cannot separate them, and the plan's arithmetic was wrong.** D=1/64+1/64=0.0312500 and E=1/63+1/65=0.0312576, so E still outranks D and the order is unchanged — `[A C B E D]` under mutant and original alike. The plan predicted a swap; there is none.
- *Fixture that actually separates them (added):* `TestAbsentFromAnArmContributesNothing`, whose arms have deliberately different lengths (2 and 4), so the synthetic terms are 1/65 and 1/63 rather than 1/65 and 1/64, and the vector-only `delta` and lexical-only `alpha` sit 0.00026 apart — close enough for the near-constant the mutant adds to swap them.
- *Must fail:* `TestAbsentFromAnArmContributesNothing`
- *Observed (score-only mutant, matching the wording "absent arm **scored** as len+1"):* **survived** `TestFuseIsReciprocalRankOverBothArms`. Killed by the added test: `fuse_test.go:121: got [nine alpha delta zulu3 mike4], want [nine delta alpha zulu3 mike4]`. One unpredicted kill: `fuse_test.go:71: k=60 ranked "Z0" first, want Y` — `Z0` is lexical-only against a 2-long vector arm, so it gains 1/63 and passes `Y`.
- *Observed (M5b — the same mutant also writing the synthetic value into the rank field):* killed by the prescribed test, but on its **rank** assertion rather than its order assertion: `fuse_test.go:51: E has vector rank 5, want 0`. So the prescribed test detects only the variant that corrupts the reported rank; the variant that corrupts only the score is invisible to it.
- *Compiles and vets:* yes, both variants.

**M6 — `if top < f.Value` → `if top <= f.Value`.**
- *Why the code exists:* the boundary has to be fixed somewhere or it drifts between the code, the tests and the README.
- *Fixture that separates mutant from original:* `TestTheFloorIsInclusive`, which hands `Decide` a top score of *exactly* the floor. Only a unit test can: through pgvector, an exact float equality against a configured floor is not reproducible.
- *Must fail:* `TestTheFloorIsInclusive`
- *Observed:* killed. `decide_test.go:22: a top score of exactly 0.5 gave refused/below_floor, want answered` — prediction correct. Also killed `TestTheShippedDefaultRefusesNothingOnScoreAlone`: `decide_test.go:58: the worst possible cosine gave refused/below_floor at the default floor`, because `-1 <= -1`. The shipped default sits exactly on the boundary, so the "ships at -1" test doubles as a second inclusivity assertion.
- *Compiles and vets:* yes.

**M7 — delete the `math.IsNaN` branch.**
- *Why the code exists:* `NaN < x` is false, so without the branch a score that is not a number is answered on.
- *Fixture that separates mutant from original:* `TestANaNTopScoreRefuses`. No fixture with a real score can reach this branch.
- *Must fail:* `TestANaNTopScoreRefuses`
- *Observed:* killed. `decide_test.go:36: got answered/, want refused/unscored` — prediction correct, empty reason included.
- *Compiles and vets:* yes, confirmed — `Validate` still calls `math.IsNaN`, so the import survives and the `&& false` fallback was not needed. `Validate` must keep that call for its own sake: `NaN < -1` and `NaN > 1` are both false, so a range check alone accepts NaN.

**M8 — `if hits == 0` → `if false`.**
- *Why the code exists:* an empty result is a refusal, not an answer with no citations, and it is the refusal that stays live while the floor is uncalibrated.
- *Fixture that separates mutant from original:* `TestNothingRetrievedRefusesEvenAtTheDefaultFloor`, which passes `hits=0` and a NaN top. Note the mutant then falls through to the NaN branch and *also* refuses — with the wrong reason. The test asserts the reason, which is what makes it a kill; asserting only `OutcomeRefused` would pass.
- *Must fail:* `TestNothingRetrievedRefusesEvenAtTheDefaultFloor`
- *Observed:* killed. `decide_test.go:14: got refused/unscored, want refused/no_spans` — prediction correct, and the fall-through to the NaN branch happened exactly as described, which is what makes asserting the reason load-bearing.
- *Compiles and vets:* yes.

**M9 — `DefaultFloor` returns `Floor{Value: 0.35, Calibrated: true}`.**
- *Why the code exists:* spec:315. The value is P6's, and the flag is what stops a reader mistaking a placeholder for a measurement.
- *Fixture that separates mutant from original:* `TestTheShippedDefaultRefusesNothingOnScoreAlone`, which asserts both halves — the flag and the behaviour at the bottom of the cosine range.
- *Must fail:* `TestTheShippedDefaultRefusesNothingOnScoreAlone`
- *Observed:* killed. `decide_test.go:55: the default floor claims to be calibrated` — prediction correct. The test stops at the flag, so its behavioural half is unreached under this mutant; M6's second kill covers that half.
- *Compiles and vets:* yes.

**M10 — `ParseMode` returns `Mode(s), nil` for anything (added: the plan prescribed `ParseMode` with no test).**
- *Why the code exists:* `RETRIEVAL_MODE` is validated at boot; defaulting an unrecognised value would hide a typo behind a service retrieving differently than it was asked to.
- *Fixture that separates mutant from original:* `TestParseModeIsAClosedSet`, whose reject list includes `""` — the value a deployment hits by not setting the variable at all — and `"Hybrid"`, so a case-insensitive parse fails too.
- *Must fail:* `TestParseModeIsAClosedSet`
- *Observed:* killed. `rag_test.go:19: ParseMode("") accepted, returning ""`
- *Compiles and vets:* yes.

- [x] **Step 5: Commit**

```bash
gofmt -l apps packages && go vet ./... && go test ./... -count=1
git add packages/shared/rag
git commit -m "feat(rag): reciprocal-rank fusion and the grounded-or-refused rule

Fusion reads ranks, never scores: a cosine similarity and a ts_rank_cd
have no exchange rate, and inventing one would be a normalisation nobody
measured. Every fused hit keeps its per-arm ranks, because spec:316 makes
fusion-versus-vector-only an experiment for P6/P7 rather than a claim.

The floor is compared against the vector arm's cosine similarity, not
against the fused score. A fused score is a function of ranks alone — the
top hit of any non-empty result scores 1/(k+1) whether it is perfect or
worthless — and a test asserts exactly that, because it is the reason the
comparison is where it is.

It ships at -1, the bottom of the cosine range, so it cannot be mistaken
for a tuned threshold. Spec:315 puts the number in P6."
```

**Definition of Done**
- `Fuse` combines two arms by reciprocal rank, keeps per-arm ranks, and is deterministic under ties.
- A fused score is proven to carry no quality signal, which is what justifies where the floor is read.
- `Decide` separates refusal reasons into the closed set the metric labels use, refuses on `NaN`, and does not apply a cosine floor to a lexical-only result.
- The shipped floor is `-1`, uncalibrated, and a test fails if that changes.
- M1–M10 each recorded with observed output; M2's predicted survival held and is recorded; M5 survived the fixture the plan named for it, and the fixture that does kill it was added.

**Deviations from this task as written, and why:**
- **`type Arm string` / `ArmVector` / `ArmLexical` are not shipped.** Nothing in P3 consumes them: `Fuse` takes named arms by this task's own argument against `map[Arm][]Hit`, Task 6's metric labels are `mode`/`outcome`/`reason`, and Task 7's response fields are `vector_rank`/`lexical_rank`. They are residue of the design this task rejects, and shipping them would be dead exported API.
- **`ParseMode` gained a test (`TestParseModeIsAClosedSet`) and a mutation (M10).** It was listed under Produces with neither, and it is a fail-closed boot validator — precisely the shape this codebase tests elsewhere (`chunk.Options.Validate`).
- **`TestAbsentFromAnArmContributesNothing` added.** See M5: the fixture the plan named cannot detect the mutation it was named for.
- **Left open for Task 6:** `Fuse` does not guard `k`. `k = -1` makes `rrf(k, 1)` a division by zero (`+Inf`), and `k <= -2` yields negative scores that invert the ranking. `Fuse` returns no error and `Fuse(0, …)` is a required call, so the guard belongs where `RETRIEVAL_MODE` is validated: **Task 6 should validate `RETRIEVAL_RRF_K >= 0` at boot**, which it does not currently mention.

---

### Task 2: The vector arm

pgvector cosine over spans, filtered by repo (spec §8). Plus the guard that stops a query embedded by one model being ranked against a corpus embedded by another.

**Files:**
- Create: `packages/shared/store/search.go`
- Test: `packages/shared/store/search_live_test.go` (`//go:build live`)

**Interfaces:**
- Consumes: `models.Cite`, `vecLiteral` (already in `spans.go`).
- Produces:
  - `func (s *Store) VectorSearch(ctx context.Context, repoID string, q []float32, limit int) ([]models.Cite, error)`
  - `func (s *Store) SpanEmbedder(ctx context.Context, repoID string) (model string, dim int, err error)`
  - `var ErrMixedEmbedders = errors.New("store: repo has spans from more than one embedder")`

**Decisions, with their reasoning:**

- **The score returned is cosine *similarity*, `1 - (embedding <=> q)`, not the distance.** `<=>` is a distance: smaller is better, and it is the operator the HNSW index is built for, so the `ORDER BY` must use it directly. But the number that leaves this package is compared against a floor and put in a response, and a threshold on a quantity where lower is better is the sort of thing that survives review and inverts a product.
- **`ORDER BY embedding <=> $2::vector` with a `LIMIT`, not `ORDER BY score DESC`.** Only the first form uses `spans_embedding_idx`. Sorting on the computed similarity is the same ordering and a sequential scan.
- **`embedding IS NOT NULL` in the `WHERE`.** The column is nullable; `PutSpans` always writes one, so this is for a row written by something else — and the consequence without it is not a bad ranking, it is a scan error, because the similarity of a NULL is NULL and the destination is a `float64`.
- **`SpanEmbedder` returns the repo's own `embed_model`/`embed_dim` and refuses a repo carrying two.** `PutSpans` replaces a repo's spans wholesale so a repo is single-model by construction; this reads what is there rather than trusting that, because the failure it prevents is a query and a corpus in different vector spaces, which produces confident nonsense and nothing about the query looks wrong (spec §3 says this about widths; it is equally true of models).
- **No `kind` filter and no `kind` weighting.** `kind=file` does not say which arm produced a row (P2), so any heuristic over it is a heuristic over a label that means two different things.

- [ ] **Step 1: Write the failing live tests**

The fixture is the load-bearing part. Two repos; four spans in the target repo whose vectors are chosen so **cosine order, L2 order and inner-product order are three different orders**; and the best answer inserted last so a missing `ORDER BY` cannot pass.

With `e0` and `e1` the first two basis vectors of the 768-wide space, query `q = e0`:

| span | vector | cosine sim | L2 distance | inner product |
| --- | --- | --- | --- | --- |
| `A` | `10·e0` | 1.000 | 9.000 | 10.0 |
| `B` | `e0 + 0.1·e1` | 0.995 | 0.100 | 1.0 |
| `D` | `2·e0 + 5·e1` | 0.371 | 5.099 | 2.0 |
| `C` | `e1` | 0.000 | 1.414 | 0.0 |

- cosine (`<=>`): `A, B, D, C`
- L2 (`<->`): `B, C, D, A`
- inner product (`<#>`, negated): `A, D, B, C`

All three differ, so one fixture kills all three operator mutations. Insert in the order `C, D, B, A` so physical order is the reverse of the answer, and give the spans paths whose alphabetical order is also not the answer.

```go
//go:build live

package store_test

// TestVectorSearchRanksByCosineAndNotByDistance seeds the table above, plus a
// second repo whose span is an exact copy of the query vector — a better match
// than anything in the target repo, so a missing repo filter shows up as that
// span appearing rather than as a subtly different order.
func TestVectorSearchRanksByCosineAndNotByDistance(t *testing.T) { /* … */ }

// The score in the response is a similarity: 1 for the parallel vector, 0 for
// the orthogonal one. A pinned pair of exact values, because "sorted
// descending" holds for the distance too.
func TestVectorSearchScoresAreCosineSimilarity(t *testing.T) { /* … */ }

func TestVectorSearchIsScopedToOneRepo(t *testing.T)      { /* … */ }
func TestVectorSearchRespectsTheLimit(t *testing.T)        { /* … */ }

// A span with a NULL embedding is skipped rather than scanned into a float64.
// Written with a direct INSERT: PutSpans cannot produce one, which is exactly
// why the guard needs a test that does not go through PutSpans.
func TestSpansWithNoEmbeddingAreNotRetrieved(t *testing.T)  { /* … */ }

func TestSpanEmbedderNamesWhatTheRepoWasIndexedWith(t *testing.T) { /* … */ }
func TestSpanEmbedderRefusesARepoWithTwoModels(t *testing.T)      { /* … */ }
```

Every live suite creates its own database through `packages/shared/testdb` (P2's review round). Follow `spans_live_test.go`'s `TestMain` exactly; do not add a suite that connects to `DATABASE_URL` directly.

- [ ] **Step 2: Implement**

```go
// VectorSearch ranks a repo's spans by cosine similarity to q (spec §8).
//
// ORDER BY on the distance operator rather than on the returned similarity:
// only that form uses spans_embedding_idx. The similarity is what leaves the
// package, because a floor on a distance would be a floor whose sense is
// inverted and whose tests would still pass.
func (s *Store) VectorSearch(ctx context.Context, repoID string, q []float32, limit int) ([]models.Cite, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, repo_id, file_id, path, kind, symbol, start_line, end_line,
		       text, digest, 1 - (embedding <=> $2::vector) AS score
		FROM spans
		WHERE repo_id = $1 AND embedding IS NOT NULL
		ORDER BY embedding <=> $2::vector
		LIMIT $3`, repoID, vecLiteral(q), limit)
	…
}
```

`SpanEmbedder` is `SELECT DISTINCT embed_model, embed_dim FROM spans WHERE repo_id = $1` with a row count check: zero rows is `ErrNotFound` (an unindexed or empty repo), more than one is `ErrMixedEmbedders`.

- [ ] **Step 3: Run**

`go test -tags=live ./packages/shared/store/ -count=1 -v` against a live Postgres.

- [ ] **Step 4: Commit, then prove the tests discriminate**

**M1 — `<=>` → `<->` (L2 distance) in both the SELECT and the ORDER BY.**
- *Why the code exists:* the schema's index is `vector_cosine_ops` and embeddings are direction, not magnitude.
- *Fixture that separates mutant from original:* the four-vector table, whose L2 order (`B, C, D, A`) shares no prefix with its cosine order. **A fixture of unit vectors cannot:** for unit vectors L2 distance is a monotone function of cosine distance and the two orders are identical.
- *Must fail:* `TestVectorSearchRanksByCosineAndNotByDistance`
- *Expected (verify and correct):* `ranked [B C D A], want [A B D C]`
- *Compiles and vets:* yes — both operators exist on `vector`, so this is a different ranking and not a SQL error. Confirm that: if `<->` errors on the pinned image, the mutation is void and must be replaced.

**M2 — `<=>` → `<#>` (negative inner product).**
- *Why the code exists:* same as M1, and inner product is the operator that *looks* right because it agrees with cosine whenever magnitudes are equal.
- *Fixture that separates mutant from original:* `B` and `D` — cosine puts `B` above `D` (0.995 vs 0.371), inner product puts `D` above `B` (2.0 vs 1.0). Two vectors of equal magnitude cannot separate these operators at all.
- *Must fail:* `TestVectorSearchRanksByCosineAndNotByDistance`
- *Expected (verify and correct):* `ranked [A D B C], want [A B D C]`
- *Compiles and vets:* yes.

**M3 — `1 - (embedding <=> $2)` → `(embedding <=> $2)`.**
- *Why the code exists:* the number crossing the package boundary is compared against a floor, and a distance inverts the comparison.
- *Fixture that separates mutant from original:* `TestVectorSearchScoresAreCosineSimilarity`, which asserts the parallel span scores `1` and the orthogonal one `0`. The ranking tests cannot: the order is unchanged.
- *Must fail:* `TestVectorSearchScoresAreCosineSimilarity`
- *Expected (verify and correct):* `A scored 0, want 1`
- *Compiles and vets:* yes.

**M4 — drop `WHERE repo_id = $1`.**
- *Why the code exists:* retrieval is per repository; the corpus holds up to `KEEP_REPOS` of them.
- *Fixture that separates mutant from original:* the second repo's span is an *exact copy of the query vector*, so it outranks everything in the target repo and appears at position 1. A second repo whose spans are poorer matches would leave the target repo's order intact and the test would pass.
- *Must fail:* `TestVectorSearchIsScopedToOneRepo`
- *Expected (verify and correct):* `span from repo other-repo appeared at rank 1`
- *Compiles and vets:* **check this one.** Dropping the clause leaves `$1` unbound, which is a pgx protocol error, not a ranking change — P1 scored two void kills exactly this way. Apply it as `WHERE repo_id = $1 OR TRUE` instead, which keeps every parameter bound.

**M5 — drop the `ORDER BY` (keep the `LIMIT`).**
- *Why the code exists:* it is the ranking, and it is the clause that uses the ANN index.
- *Fixture that separates mutant from original:* the insertion order `C, D, B, A` is the reverse of the expected answer, so rows returned in physical order fail immediately. A fixture inserted best-first would pass under this mutant.
- *Must fail:* `TestVectorSearchRanksByCosineAndNotByDistance`
- *Expected (verify and correct):* `ranked [C D B A], want [A B D C]` — physical order is not guaranteed by Postgres, so record what actually came back; any order other than the expected one is the kill.
- *Compiles and vets:* yes.

**M6 — drop `AND embedding IS NOT NULL`.**
- *Why the code exists:* a NULL similarity cannot be scanned into a `float64`.
- *Fixture that separates mutant from original:* the directly-inserted NULL-embedding row in `TestSpansWithNoEmbeddingAreNotRetrieved`. No corpus written by `PutSpans` contains one.
- *Must fail:* `TestSpansWithNoEmbeddingAreNotRetrieved`
- *Expected (verify and correct):* a scan error naming a NULL destination. **If the failure is a `t.Fatal(err)` rather than the test's own assertion, that is an incident, not a kill** (rule 7): rewrite the test to assert `err != nil` *and* that the error is not a bare scan panic, or to assert on the returned ids with the row present. Record which shape you ended up with.
- *Compiles and vets:* yes.

**M7 — `SpanEmbedder` returns the first row instead of refusing two.**
- *Why the code exists:* a query and a corpus in different vector spaces rank nonsense confidently.
- *Fixture that separates mutant from original:* the two-model repo, built with direct INSERTs. `PutSpans` cannot produce one.
- *Must fail:* `TestSpanEmbedderRefusesARepoWithTwoModels`
- *Expected (verify and correct):* `got model "a-model", want ErrMixedEmbedders`
- *Compiles and vets:* yes.

- [ ] **Step 5: Commit**

```bash
gofmt -l apps packages && go vet ./... && go test ./... -count=1
go test -tags=live ./packages/shared/store/ -count=1
git add packages/shared/store
git commit -m "feat(store): cosine retrieval over one repository's spans

ORDER BY runs on the distance operator because that is what the HNSW
index is built for; the similarity is what leaves the package, because a
score floor compared against a distance is a filter with its sense
inverted and tests that still pass.

The fixture's vectors are deliberately not unit length: for unit vectors
cosine and L2 induce the same order, so a unit-vector fixture cannot tell
the operators apart. Cosine, L2 and inner product each rank these four
spans differently.

SpanEmbedder reads the model the repo was indexed with rather than
trusting PutSpans's replacement contract, because the failure it prevents
is a query ranked against another vector space, which looks like an
answer."
```

**Definition of Done**
- Cosine ranking, scoped to one repo, limited, index-using, with a similarity in the response.
- The fixture distinguishes cosine from both L2 and inner product, and its expected order disagrees with insertion order, path order and id order.
- A NULL embedding cannot reach a scan.
- A repo carrying two embedders is refused rather than silently half-searched.
- M1–M7 recorded with observed output; M4's rewrite (to keep every parameter bound) recorded.

---

### Task 3: The lexical arm

Spec §8: "a lexical arm over symbol names and identifiers". Postgres full-text search over a weighted `tsvector`, with the query tokenised in Go so a question containing punctuation is a query and not a syntax error.

**Files:**
- Create: `packages/shared/store/migrations/0008_span_lexical.sql`, `packages/shared/store/lexical.go`, `packages/shared/rag/terms.go`
- Test: `packages/shared/store/lexical_live_test.go`, `packages/shared/rag/terms_test.go`

**Interfaces:**
- Produces:
  - `func (s *Store) LexicalSearch(ctx context.Context, repoID string, terms []string, limit int) ([]models.Cite, error)`
  - `func rag.Terms(q string, split bool) []string`

**Decisions, with their reasoning:**

- **A `STORED GENERATED` column plus a GIN index, not an on-the-fly `to_tsvector`.** Computing the vector per query is a sequential scan over the repo's spans and re-tokenises text that never changes. The two-argument `to_tsvector(regconfig, text)` is immutable — the one-argument form is only stable, because it reads `default_text_search_config` — so the generated column is legal only with the configuration named explicitly. **Verify that against the pinned image before writing the migration** (step 1); if it is rejected, the fallback is an expression index and the query must then repeat the expression verbatim.
- **The `simple` configuration, not `english`.** `english` stems and drops stopwords: `Files` and `filing` collapse together, and `Get`, `New`, `Do` are near-stopwords in Go. Code is not English, and a stemmer is a lossy rename of every identifier in the corpus.
- **`symbol` at weight A and `text` at weight B.** The spec asks for symbol names *and* identifiers; a span's identifiers are its text's tokens, so indexing the text is how identifiers get indexed at all. It also indexes comment prose, which is deliberate: in production, doc comments are the best signal a span has (spec §5), and in the eval corpus they are blanked, so the arm behaves the same way in both.
- **Deviation, recorded rather than hidden:** the arm therefore indexes keywords and string literals too, not only identifiers. Narrowing it to identifiers means extracting them from the AST at index time, which is a new column, an indexer change and a full re-index. That is a P7 change, listed in Open Questions.
- **The query is tokenised in Go and passed as a bound parameter to `to_tsquery('simple', $2)`.** Handing user text to `to_tsquery` raw is a `500` for any question containing `&`, `|`, `!`, `:` or `(` — which is most questions about code. Building `a | b | c` from an allowlisted charset makes the arm OR-semantic (recall over precision, which is what fusion wants from a second arm) and makes a syntax error impossible.
- **Identifier splitting is query-side only, and it is a knob.** `parseConfig` yields `parseconfig`, `parse`, `config`. The *index* holds `parseconfig` as one token — Postgres does not split camel case — so the split does not make `parse config` find `parseConfig`; it makes `parseConfig` also match prose that says "parse the config". That asymmetry is real and is written down rather than implied. `LEXICAL_SPLIT_IDENTIFIERS` defaults to true and P6 can sweep it.
- **Terms are capped (32) and deduped.** A 1,000-character question is already refused at the edge; this bounds the `tsquery` a 1,000-character question could still build.

- [ ] **Step 1: Measure the tokeniser before writing the tokeniser**

Against the pinned `pgvector/pgvector:pg17` image, and **paste the output into the commit message**:

```sql
SELECT to_tsvector('simple', 'func parseConfig(p *Store) error // parse the config file');
SELECT to_tsvector('simple', 'parse_config httpServer HTTPServer v2 x.y');
SELECT to_tsquery('simple', 'parseconfig | parse | config');
-- the generated column must be accepted at all:
CREATE TEMP TABLE t (a text, b text,
  lex tsvector GENERATED ALWAYS AS
    (setweight(to_tsvector('simple', a), 'A') || setweight(to_tsvector('simple', b), 'B')) STORED);
```

The tokeniser in `rag.Terms` must produce terms the index actually contains. Two things this measurement settles and this plan deliberately does not guess: whether `parse_config` is one token or two, and whether `x.y` is one. Write `Terms` against the observed answer, and record it in `terms.go`'s comment.

- [ ] **Step 2: Write the failing tests**

`terms_test.go` is hermetic. The table is written *after* step 1 and reflects what Postgres actually does:

```go
// Terms lowercases, splits on anything that is not a letter or digit, and — when
// splitting is on — adds the camel-case parts alongside the whole identifier.
// The whole identifier is kept because that is the token the index holds; the
// parts are kept because prose in a doc comment spells them separately.
func TestTermsKeepsTheWholeIdentifierAndItsParts(t *testing.T)
func TestTermsDropsPunctuationSoAQuestionIsNotASyntaxError(t *testing.T)
func TestTermsIsCappedAndDeduped(t *testing.T)
func TestTermsWithSplittingOffKeepsOnlyWholeIdentifiers(t *testing.T)
```

`lexical_live_test.go`. The fixture is three spans in the target repo and one in a second repo, with `symbol` deliberately decoupled from `text` — which a corpus written by the indexer never is, and which is the only way to test the weighting:

| span | symbol | text contains | why it is there |
| --- | --- | --- | --- |
| `S` | `parseConfig` | nothing matching | the A-weight case: symbol only |
| `T` | `` (empty) | `parseConfig` once, in 400 tokens | the B-weight case: body only |
| `U` | `` (empty) | "parse the config file", no `parseConfig` | only reachable via the split terms |
| `V` (other repo) | `parseConfig` | `parseConfig` ten times | outranks everything if the repo filter is missing |

```go
func TestLexicalSearchFindsASpanBySymbol(t *testing.T)
func TestSymbolOutweighsBody(t *testing.T)          // expects S above T
func TestSplitTermsReachProseThatNamesThePartsSeparately(t *testing.T) // expects U present
func TestLexicalSearchIsScopedToOneRepo(t *testing.T)                  // expects V absent
func TestNoUsableTermsReturnsNothingRatherThanErroring(t *testing.T)   // terms == nil
```

`TestSymbolOutweighsBody` is the one that can pass vacuously: assert the returned slice is non-empty *and* that `S` precedes `T` by index, not that `S` is "in" the results.

- [ ] **Step 3: Implement**

`0008_span_lexical.sql`:

```sql
-- The lexical arm (spec §8). A stored generated column rather than a
-- to_tsvector per query: the vector never changes, and computing it per query
-- is a sequential scan over the repo's spans.
--
-- 'simple', not 'english': a stemmer collapses Files and filing, and Get, New
-- and Do are near-stopwords in Go. Code is not English.
--
-- symbol at weight A, text at weight B. A span's identifiers are its text's
-- tokens, so indexing the text is how "identifiers" get indexed at all; the
-- weight is what keeps a definition above a mention.
ALTER TABLE spans ADD COLUMN IF NOT EXISTS lex tsvector
    GENERATED ALWAYS AS (
        setweight(to_tsvector('simple', symbol), 'A') ||
        setweight(to_tsvector('simple', text), 'B')
    ) STORED;
CREATE INDEX IF NOT EXISTS spans_lex_idx ON spans USING gin (lex);
```

`lexical.go`:

```sql
SELECT id, repo_id, file_id, path, kind, symbol, start_line, end_line, text, digest,
       ts_rank_cd('{0.1, 0.2, 0.4, 1.0}', lex, q)::float8 AS score
FROM spans, to_tsquery('simple', $2) AS q
WHERE repo_id = $1 AND lex @@ q
ORDER BY score DESC, path, start_line, id
LIMIT $3
```

Go side: `strings.Join(terms, " | ")` — terms come from `rag.Terms`, whose charset is letters and digits only, so the join cannot produce an operator. An empty `terms` returns `nil, nil` without a round trip, because `to_tsquery('simple', '')` is a syntax error and "the question had no words in it" is not an error.

- [ ] **Step 4: Commit, then prove the tests discriminate**

**M1 — hand the raw query string to `to_tsquery` instead of the joined terms.**
- *Why the code exists:* `to_tsquery` is an operator language; user text is not.
- *Fixture that separates mutant from original:* `TestNoUsableTermsReturnsNothingRatherThanErroring` cannot — it passes no terms. The kill needs a live test whose query text contains an operator character: add `TestAQuestionWithPunctuationIsNotASyntaxError` with `where is parseConfig() defined? (a & b)`.
- *Must fail:* `TestAQuestionWithPunctuationIsNotASyntaxError`
- *Expected (verify and correct):* a `42601 syntax error in tsquery` from pgx.
- *Compiles and vets:* yes. Note this mutation produces a **SQL error rather than a ranking change**, which rule 2 normally calls void — it is admitted here precisely because *not erroring* is the property under test.

**M2 — drop both `setweight` calls.**
- *Why the code exists:* a symbol match is a definition; a body match is a mention.
- *Fixture that separates mutant from original:* `S` (symbol only) versus `T` (one occurrence in a long body). Without weights, `ts_rank_cd` ranks on frequency and length, and `T` is expected to rise. **A fixture whose symbol also appears in its own text — which is every real span — cannot separate these**, because both documents then carry both weights.
- *Must fail:* `TestSymbolOutweighsBody`
- *Expected (verify and correct):* `parseConfig ranked [T S], want S first`
- *Compiles and vets:* yes.

**M3 — drop `WHERE repo_id = $1` (as `OR TRUE`, to keep `$1` bound — see Task 2 M4).**
- *Why the code exists:* retrieval is per repository.
- *Fixture that separates mutant from original:* `V`, in the other repo, matches the term ten times and outranks everything in the target repo.
- *Must fail:* `TestLexicalSearchIsScopedToOneRepo`
- *Expected (verify and correct):* `span V from repo other-repo appeared at rank 1`
- *Compiles and vets:* yes.

**M4 — `rag.Terms` stops splitting (`split` ignored, always false).**
- *Why the code exists:* the split is what lets a query naming an identifier reach prose that spells its parts separately.
- *Fixture that separates mutant from original:* span `U`, whose text says "parse the config file" and never contains `parseConfig`. `S` and `T` cannot separate this: both match the whole-identifier term.
- *Must fail:* `TestSplitTermsReachProseThatNamesThePartsSeparately` and `TestTermsKeepsTheWholeIdentifierAndItsParts`
- *Expected (verify and correct):* `U was not retrieved for "parseConfig"`
- *Compiles and vets:* yes.

**M5 — `Terms` drops the whole identifier and keeps only the parts.**
- *Why the code exists:* `parseconfig` is the token the index actually holds; the parts are a widener, not the mechanism. This mutation is the one that proves the asymmetry documented above is real.
- *Fixture that separates mutant from original:* span `S`, whose only match is the whole identifier in the symbol.
- *Must fail:* `TestLexicalSearchFindsASpanBySymbol`
- *Expected (verify and correct):* `S was not retrieved for "parseConfig"` — **expected to survive if `U`-style prose exists in `S`'s text**; check `S`'s fixture text contains neither "parse" nor "config" as separate words.
- *Compiles and vets:* yes.

**M6 — `ts_rank_cd` → `ts_rank`.**
- *Why the code exists:* cover density prefers a span where the query's terms appear close together, which for code is a span that is *about* the thing rather than one that mentions it in passing.
- *Fixture that separates mutant from original:* a two-term query and two spans in which the terms are adjacent versus 300 tokens apart. **The current fixture cannot** — with single-term queries the two functions frequently tie. Either add that fixture and predict a kill, or record this mutation as a **deliberate survivor** and say the choice of ranking function is unpinned by any test. Do not record a kill without the fixture.
- *Must fail:* `TestCoverDensityPrefersTermsThatAppearTogether`, if written.
- *Expected (verify and correct):* record the observed outcome either way.
- *Compiles and vets:* yes.

**M7 — the generated column drops `symbol` (index `text` only).**
- *Why the code exists:* spec §8 says the arm is over symbol names.
- *Fixture that separates mutant from original:* `S`, whose symbol carries the term and whose text does not.
- *Must fail:* `TestLexicalSearchFindsASpanBySymbol`
- *Expected (verify and correct):* `S was not retrieved`. **Applying this mutation requires re-running the migration on a fresh database** — the generated column is materialised, so editing the SQL without recreating the table changes nothing. The live suites create their own database per run (`testdb.Scratch`), so this happens automatically; confirm it rather than assuming it, because a mutation that silently tests the old column is a phantom kill of exactly P1's kind.

- [ ] **Step 5: Commit**

```bash
gofmt -l apps packages && go vet ./... && go test ./... -count=1
go test -tags=live ./packages/shared/store/ -count=1
git add packages/shared/store packages/shared/rag
git commit -m "feat(store): a lexical arm over symbol names and identifiers

A stored generated tsvector with symbol at weight A and text at weight B,
in the simple configuration: an English stemmer collapses Files and
filing, and Get, New and Do are near-stopwords in Go.

The query is tokenised in Go and passed as a parameter, so a question
containing & or ? is a query rather than a tsquery syntax error. Terms
keep the whole identifier — that is the token the index holds, since
Postgres does not split camel case — and add the split parts, which only
widens the arm towards prose that spells them separately. That asymmetry
is real and recorded; closing it means splitting at index time, which is
a re-index.

Whether this arm improves retrieval at all is spec:316's experiment, not
a claim made here.

Tokeniser measured against pgvector/pgvector:pg17, output below:
<paste>"

```

**Definition of Done**
- The migration applies to a fresh database and to one that already holds spans.
- The arm finds a span by symbol, ranks a symbol match above a body mention, is scoped to one repo, and cannot be made to error by punctuation.
- The tokeniser's behaviour was measured against the pinned image before it was written, and the measurement is in the commit.
- M1–M7 recorded, including M6's survivor status if its fixture was not written.

---

### Task 4: Citations — the permalink, and what staleness can honestly claim

Spec §8: `(repo, commit, path, startLine, endLine, digest)`, rendered as an immutable forge permalink, saying explicitly when the indexed ref has moved on.

**Files:**
- Create: `packages/shared/rag/cite.go`
- Modify: `packages/shared/store/repos.go` (one query)
- Test: `packages/shared/rag/cite_test.go`, `packages/shared/store/repos_live_test.go` (extend)

**Interfaces:**
- Produces:
  - `type Citation struct { RepoID, Remote, Commit, Ref, Path string; StartLine, EndLine int; Digest, Permalink string; Staleness Staleness }`
  - `type Staleness struct { State string; ForgeChecked bool; IndexedAt time.Time; NewerCommit string; NewerIndexedAt time.Time; Note string }`
  - `func Permalink(remote, commit, path string, start, end int) string`
  - `func (s *Store) NewerCommit(ctx, repoID string) (commit string, at time.Time, err error)`

**Decisions, with their reasoning:**

- **Per-forge permalink formats, and no guess for an unknown host.** Spec §8's example is `…/blob/<sha>/path#L10-L20`, which is GitHub's shape. Codeberg runs Forgejo and its permalink is `…/src/commit/<sha>/path#L10-L20`. The P1-settled default allowlist is `github.com` **and** `codeberg.org`, so the spec's single example is wrong for half of the shipped default. An operator may also add a host neither of those covers. **An unknown host renders no permalink**, and the citation still carries the tuple that is the actual claim. A wrong permalink is worse than none: the entire point of the link is that a reader can check it, and a 404 from the forge reads as "the code is gone" rather than "we guessed the URL".
- **Every path segment is escaped, and `/` is not.** Git paths may contain spaces and `#`. An unescaped `#` truncates the URL at the fragment and the link silently points at the wrong file's top.
- **Staleness is three-valued and one of the values is "we did not check".** §8 says "if the ref has moved since indexing, the API says the citation is from an older commit" without saying how the API learns that. There are exactly two ways: ask the forge, or infer from the corpus.
  - **Asking the forge is refused in P3.** It puts a network call to a stranger's host on the public gateway's read path — the egress P1 deliberately confined to the indexer — and makes every answer's latency depend on a third party.
  - **So the corpus is what is inferred from**, and it supports one positive claim: if this repository's *same ref* is also indexed at a **different, later** commit, then the ref demonstrably moved. That is `superseded`, with the newer commit named.
  - Otherwise the state is `unknown`, `ForgeChecked` is `false`, and the note says so in words: *"Correct at commit abc1234, indexed 92 days ago. codetrail has not checked whether main has moved since."* That is the spec's sentence made true. Claiming freshness we did not verify is the exact failure §8 exists to prevent, one level up.
- **`NewerCommit` matches on `lower(remote)` and `ref`.** `repos.remote` holds the first submitter's spelling for display while identity is `admit.Remote.Key`; two commits of one repository can therefore carry two spellings. This is the same fold `jobs_active_idx` uses, and for the same reason.

- [ ] **Step 1: Write the failing tests**

`cite_test.go` is hermetic and table-driven. **The fixture must contain more than one forge or a per-forge bug is undetectable**, and more than one path shape or the escaping is untested:

```go
func TestPermalinkPerForge(t *testing.T) {
	for _, c := range []struct{ remote, want string }{
		{"https://github.com/rs/zerolog",
			"https://github.com/rs/zerolog/blob/dfd11cca/log.go#L10-L20"},
		{"https://codeberg.org/forgejo/forgejo",
			"https://codeberg.org/forgejo/forgejo/src/commit/dfd11cca/log.go#L10-L20"},
		// A host an operator added. No format is known, so no link is
		// rendered: a guessed URL that 404s reads as "the code is gone".
		{"https://gitlab.com/group/repo", ""},
	} { … }
}

// A git path may contain a space or a '#'. An unescaped '#' truncates the URL
// at the fragment and the link points at the top of the wrong file.
func TestPermalinkEscapesEachSegmentButNotTheSeparators(t *testing.T) {
	got := Permalink("https://github.com/o/n", "abc", "docs/a b#c.md", 1, 2)
	want := "https://github.com/o/n/blob/abc/docs/a%20b%23c.md#L1-L2"
	…
}

func TestPermalinkKeepsOneBasedInclusiveLines(t *testing.T)  // #L1-L1 for a one-line span
```

Staleness, hermetic where it can be and live for the corpus query:

```go
// The default claim names what was NOT checked. A citation that implied
// freshness we never verified would be the same failure as an ungrounded
// answer, one level up.
func TestUncheckedStalenessSaysSoInWords(t *testing.T)

// Superseded is the one positive claim the corpus supports: the same remote
// and ref indexed at a later commit is proof the ref moved.
func TestSupersededNamesTheNewerCommit(t *testing.T)
```

Live, in `repos_live_test.go`: **two repo rows for the same remote and ref at different commits**, inserted oldest-first. A single-commit fixture cannot detect a staleness bug at all, and a fixture whose two rows differ in `ref` cannot detect a missing `ref` predicate.

```go
func TestNewerCommitFindsALaterIndexOfTheSameRef(t *testing.T)
func TestNewerCommitIgnoresADifferentRef(t *testing.T)
func TestNewerCommitFoldsTheRemotesCase(t *testing.T)   // github.com/O/N vs github.com/o/n
func TestTheNewestIndexHasNoNewerCommit(t *testing.T)
```

- [ ] **Step 2: Implement**

```go
// forges maps a host to its permalink shape. Two entries, because P1 settled
// the default allowlist at github.com and codeberg.org, and their formats
// differ: Codeberg runs Forgejo, whose blob URL is /src/commit/<sha>/.
//
// Spec §8's example is GitHub's, which is why this is a table and not a
// format string.
var forges = map[string]string{
	"github.com":   "%s/blob/%s/%s#L%d-L%d",
	"codeberg.org": "%s/src/commit/%s/%s#L%d-L%d",
}
```

`Permalink` parses `remote` with `url.Parse`, looks the host up, returns `""` when it is absent, and joins `url.PathEscape`d segments with `/`.

- [ ] **Step 3: Commit, then prove the tests discriminate**

**M1 — unknown hosts fall back to the GitHub format.**
- *Why the code exists:* a permalink exists to be checked; a guessed one that 404s is a worse claim than no link.
- *Fixture that separates mutant from original:* the `gitlab.com` row. A fixture with only allowlisted hosts cannot reach the branch.
- *Must fail:* `TestPermalinkPerForge`
- *Expected (verify and correct):* `gitlab.com rendered ".../blob/abc/log.go#L10-L20", want ""`

**M2 — one format for every forge (drop the Codeberg entry).**
- *Why the code exists:* the two forges genuinely differ.
- *Fixture that separates mutant from original:* the `codeberg.org` row. **A GitHub-only fixture cannot** — which is the shape a plan writes by default, since the spec's example is GitHub's.
- *Must fail:* `TestPermalinkPerForge`
- *Expected (verify and correct):* `codeberg rendered ".../blob/..." want ".../src/commit/..."` — or `""`, depending on whether the entry was deleted or rewritten; record which.

**M3 — `url.PathEscape` dropped.**
- *Why the code exists:* `#` and space are legal in a git path and both break a URL.
- *Fixture that separates mutant from original:* the `docs/a b#c.md` path. Every ordinary path (`log.go`) is byte-identical escaped and unescaped, so a realistic-looking fixture cannot detect this.
- *Must fail:* `TestPermalinkEscapesEachSegmentButNotTheSeparators`

**M4 — escape the whole path in one call (so `/` becomes `%2F`).**
- *Why the code exists:* the separators are structure, not content.
- *Fixture that separates mutant from original:* any multi-segment path — `docs/a b#c.md` has two. A single-segment fixture would pass.
- *Must fail:* `TestPermalinkEscapesEachSegmentButNotTheSeparators`

**M5 — `#L%d-L%d` → `#L%d-%d`.**
- *Why the code exists:* both forges want the `L` on both ends; without it the anchor selects one line.
- *Fixture that separates mutant from original:* any, since it is a literal — but the test must compare the **whole string**, not `strings.Contains`, or it survives.
- *Must fail:* `TestPermalinkPerForge`

**M6 — `NewerCommit` drops `AND ref = $3`.**
- *Why the code exists:* a citation into `v1.2.3` is not stale because `main` was indexed later; a tag does not move.
- *Fixture that separates mutant from original:* the third repo row, same remote, ref `v1.0.0`, indexed last. Without it the mutant is invisible.
- *Must fail:* `TestNewerCommitIgnoresADifferentRef`

**M7 — `NewerCommit` drops `AND commit_sha <> $4`.**
- *Why the code exists:* a repository re-indexed at the same commit has not moved.
- *Fixture that separates mutant from original:* two rows... **there cannot be two rows at the same commit**: `RepoID = hash(key, commit)` makes them one row. So the predicate is unreachable through the store's own writes, and the mutation is **expected to survive**. Record it as a survivor and consider deleting the predicate rather than leaving an untested clause. Verify by trying: `PutRepo` twice at the same commit and count rows.

**M8 — `ForgeChecked` hard-wired to `true`.**
- *Why the code exists:* it is the field that stops the note being read as a freshness guarantee.
- *Fixture that separates mutant from original:* `TestUncheckedStalenessSaysSoInWords`, which asserts the flag *and* the note's wording. Asserting only the state string would survive.
- *Must fail:* `TestUncheckedStalenessSaysSoInWords`

- [ ] **Step 4: Commit**

```bash
git commit -m "feat(rag): citations that render a permalink and say what they did not check

Two forge formats, not one: P1 settled the default allowlist at
github.com and codeberg.org, and Codeberg runs Forgejo, whose blob URL is
/src/commit/<sha>/. Spec §8's example is GitHub's shape. A host with no
known format renders no link at all — a guessed URL that 404s reads as
'the code is gone' rather than 'we guessed'.

Staleness claims only what the corpus proves. Asking the forge would put
a network call to a stranger's host on the public read path, which is the
egress P1 confined to the indexer, so the API says 'correct at commit X,
indexed N days ago; codetrail has not checked whether main has moved',
and upgrades to 'superseded' only when this repository's same ref is also
indexed at a later commit."
```

**Definition of Done**
- Permalinks are correct for both default forges, absent for any other host, and escaped per segment.
- Staleness distinguishes "not checked" from "demonstrably moved", and the words in the API say which.
- M1–M8 recorded, M7 with its survival and the decision that followed.

---

### Task 5: The history the read endpoints imply — tombstones, job retention, and the job's repo

Spec §14: "How much job history to keep… is the P3 read endpoints' question — so it is decided there." Spec §10: an evicted repo answers `410 Gone`, "it existed, and that is a different fact". Both are the same decision — how much of the past a caller may still see — so they are one task.

**Files:**
- Create: `packages/shared/store/migrations/0009_history.sql` (renumber if Task 3 lands second)
- Modify: `packages/shared/store/evict.go`, `packages/shared/jobs/jobs.go`, `apps/indexer/cmd/main.go`
- Test: `packages/shared/store/evict_live_test.go`, `packages/shared/jobs/jobs_live_test.go`, `apps/indexer/cmd/main_test.go`

**Interfaces:**
- Produces:
  - `func (s *Store) RepoGone(ctx context.Context, id string) (bool, error)`
  - `func (q *Queue) Sweep(ctx context.Context, keepFor time.Duration) (int, error)`
  - `Complete` gains a `repoID` parameter; `jobs.Job` gains `RepoID`

**Decisions, with their reasoning:**

- **`410` needs a tombstone, because eviction is a `DELETE`.** After `Evict`, nothing in the database distinguishes "this repository was indexed and evicted" from "nobody ever submitted it", so a gateway cannot answer `410` at all — spec §10 is unimplementable as the schema stands. `evicted_repos` holds `(id, remote, ref, commit_sha, indexed_at, evicted_at)` and is written **in the same statement** as the delete (`WITH gone AS (DELETE … RETURNING …) INSERT INTO evicted_repos SELECT … FROM gone`), so the two cannot disagree — a second statement would leave a window in which a crash loses the fact that eviction happened.
- **The tombstone is bounded by count.** It is a record of what used to be here, not a ledger; the newest 500 (`KEEP_TOMBSTONES`) is enough for a caller holding a stale link. Beyond that a repo id goes back to `404`, which is the honest answer once we no longer remember.
- **Job retention is by age, not by count, and never touches a non-terminal job.** `Sweep` deletes `done` and `failed` jobs whose `updated_at` is older than `JOB_HISTORY_HOURS` (default **168h**, one week).
  - **The cheap bound is refused:** dropping terminal jobs when the same repository is re-enqueued makes a caller's `GET /api/jobs/:id` start returning `404` at a moment chosen by an unrelated stranger submitting the same URL. A caller cannot distinguish that from "the id was never real", and it is a data-loss race triggered by a public, unauthenticated endpoint.
  - **A count cap is refused for the same reason with different arithmetic:** under a flood, "keep the newest N" evicts jobs that are minutes old and still being polled. Age is the only bound that can be *stated to a caller*: "a job id is readable for seven days after it finishes".
  - **What this does not bound is a flood inside the window.** A week of submissions at a rate nobody limits is a week of rows; a job row is a few hundred bytes, so a million submissions is a couple of hundred megabytes. The missing control is a rate limit on `POST /api/repos`, which the spec notes does not exist and this phase does not add. Recorded in the README as a known limit rather than implied away by a cap that would break polling.
- **The sweep runs in the indexer, on a timer, not in the gateway.** Spec §2: the gateway writes job rows and `repos.last_queried_at` and nothing else. The indexer already owns terminal states. `JOB_SWEEP_MINUTES` defaults to 60, and the sweep runs from the lease loop, not from a goroutine with its own lifetime, so a shutting-down worker stops sweeping when it stops leasing.
- **`jobs.repo_id`, written by `Complete`.** Without it, a caller who submits a repository and polls a job to `done` has no way to name the repository they just indexed: the job knows a remote and a ref, and the repo id is `hash(key, commit)` where the commit is only known to the indexer. Every P3 read endpoint is keyed on the repo id, so this is what makes the phase reachable rather than a nice-to-have. It is nullable, because a job that is still running has not got one.
- **Metrics for eviction and sweeping are deliberately *not* added.** The indexer serves no HTTP: it has no `/metrics` for a counter to be scraped from. Spec §11 implies both services expose one; that gap is recorded in "What the spec leaves underspecified", not closed here, because giving the indexer an HTTP server is P0-shaped work and would grow this task past one sitting.

- [ ] **Step 1: Write the failing tests**

Live, in `evict_live_test.go`:

```go
// The tombstone is what makes 410 possible: after a DELETE, nothing else in
// the database remembers the repo existed.
func TestEvictionLeavesATombstone(t *testing.T)

// Written in the same statement as the delete. A repo evicted, re-indexed and
// evicted again must still be tombstoned exactly once and still be counted —
// an ON CONFLICT DO NOTHING would return a count that undercounts the delete.
func TestReEvictingAKnownRepoStillCountsAndRefreshesTheTombstone(t *testing.T)

func TestRepoGoneIsFalseForARepoThatNeverExisted(t *testing.T)
func TestTombstonesAreBoundedToTheNewest(t *testing.T)
```

Live, in `jobs_live_test.go`. **The fixture must contain a non-terminal job older than the window** or the "never touches a running job" claim is untested, and a terminal job *inside* the window or the boundary is untested:

```go
func TestSweepDeletesTerminalJobsPastTheWindow(t *testing.T)
func TestSweepKeepsATerminalJobInsideTheWindow(t *testing.T)
func TestSweepNeverDeletesAPendingOrLeasedJobHoweverOld(t *testing.T)
func TestCompleteRecordsTheRepoTheJobProduced(t *testing.T)
func TestAFailedJobHasNoRepoID(t *testing.T)
```

Hermetic, in the indexer: `TestTheSweepRunsAtMostOncePerInterval` against a fake clock, and `TestSweepFailureDoesNotStopLeasing`.

- [ ] **Step 2: Implement**

```sql
-- An evicted repo answers 410 Gone, not 404: it existed, and that is a
-- different fact (spec §10). Eviction is one DELETE with a cascade, so after
-- it nothing in the database remembers — hence a tombstone, written in the
-- same statement so the two cannot disagree.
CREATE TABLE IF NOT EXISTS evicted_repos (
    id         TEXT PRIMARY KEY,
    remote     TEXT NOT NULL,
    ref        TEXT NOT NULL,
    commit_sha TEXT NOT NULL,
    indexed_at TIMESTAMPTZ NOT NULL,
    evicted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS evicted_repos_at_idx ON evicted_repos (evicted_at);

-- What a finished job produced. Without it a caller who polls a job to done
-- cannot name the repository it indexed: the repo id is hash(key, commit) and
-- only the indexer ever saw the commit.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS repo_id TEXT;
```

- [ ] **Step 3: Commit, then prove the tests discriminate**

**M1 — write the tombstone in a second statement after the delete.**
- *Why the code exists:* two statements can disagree; a crash between them loses the only record that the repo existed.
- *Fixture that separates mutant from original:* none in a test suite — this is a crash-window argument, not a behaviour difference, and **no fixture can kill it**. Do not list it as a kill. It is recorded here as a design claim whose justification is the counterfactual, not a test. (Rule 8: if you cannot describe the failing output, it is not evidence — so this is documented as reasoning, and the *comment* in the code must not claim a test proves it.)

**M2 — `ON CONFLICT (id) DO UPDATE` → `DO NOTHING`.**
- *Why the code exists:* `Evict` returns the number of repos removed, and with the CTE form that number is the *insert's* row count.
- *Fixture that separates mutant from original:* the evict → re-index → evict fixture. A fixture that evicts each repo once cannot reach the conflict.
- *Must fail:* `TestReEvictingAKnownRepoStillCountsAndRefreshesTheTombstone`
- *Expected (verify and correct):* `Evict reported 0, want 1` — and confirm the repo row *was* deleted, so the mutation is a wrong count rather than a failed eviction.

**M3 — `Sweep` drops `status IN ('done','failed')`.**
- *Why the code exists:* a running job's row is the lease; deleting it hands the same repository to a second worker with no record of the first.
- *Fixture that separates mutant from original:* the leased job with an `updated_at` older than the window. A fixture of only terminal jobs cannot.
- *Must fail:* `TestSweepNeverDeletesAPendingOrLeasedJobHoweverOld`
- *Expected (verify and correct):* `a leased job was swept`

**M4 — `updated_at < now() - $1` → `updated_at > now() - $1`.**
- *Why the code exists:* it is the retention window.
- *Fixture that separates mutant from original:* one terminal job inside the window and one outside it. With only one job, either mutant deletes something and a count assertion passes.
- *Must fail:* `TestSweepKeepsATerminalJobInsideTheWindow`
- *Expected (verify and correct):* `the job finished 1h ago was swept`

**M5 — `Complete` writes a constant repo id.**
- *Why the code exists:* the id is the only link from a submission to what it produced.
- *Fixture that separates mutant from original:* the live test asserts the stored id equals `store.RepoID(key, commit)` computed in the test from the same inputs — not merely "non-empty", which the mutant satisfies.
- *Must fail:* `TestCompleteRecordsTheRepoTheJobProduced`
- *Expected (verify and correct):* `job repo_id "x", want "9f2c…"`

**M6 — the sweep interval check removed, so it sweeps every poll.**
- *Why the code exists:* the loop polls every two seconds by default; a `DELETE` scan per poll is a self-inflicted load with no upside.
- *Fixture that separates mutant from original:* the fake clock in `TestTheSweepRunsAtMostOncePerInterval`, counting calls. A real-clock test would need a wall-clock wait and would be a rule 6 violation.
- *Must fail:* `TestTheSweepRunsAtMostOncePerInterval`
- *Expected (verify and correct):* `swept 5 times in one interval, want 1`

**M7 — `KEEP_TOMBSTONES` trimming removed.**
- *Why the code exists:* the tombstone table is otherwise the second unbounded table in a schema that just spent a task bounding the first.
- *Fixture that separates mutant from original:* `TestTombstonesAreBoundedToTheNewest`, which needs `keep+1` evictions. A fixture with two evictions cannot.
- *Must fail:* `TestTombstonesAreBoundedToTheNewest`

- [ ] **Step 4: Commit**

```bash
git commit -m "feat(store): tombstone evicted repos, and bound job history by age

410 Gone was unimplementable: eviction is one DELETE with a cascade, so
afterwards nothing distinguishes a repository that was evicted from one
that was never submitted. The tombstone is written in the same statement
as the delete so the two cannot disagree, and is itself bounded.

Job history is bounded by age, not by count, and never touches a
non-terminal job. Dropping terminal jobs on re-enqueue — the cheap bound
— would 404 a job id a caller still holds, at a moment chosen by an
unrelated stranger submitting the same URL; a count cap does the same
thing under a flood. An age window is the only rule that can be stated to
a caller: a job id is readable for a week after it finishes. What that
does not bound is a flood inside the window, whose control is a rate
limit this phase does not add; the README says so.

Complete now records the repo the job produced, because the repo id is
hash(key, commit) and only the indexer ever saw the commit."
```

**Definition of Done**
- An evicted repo is distinguishable from an unknown one, and the tombstone table is bounded.
- Terminal jobs age out on a stated window; running jobs never do; the sweep runs on a timer inside the lease loop.
- A completed job names its repository.
- M2–M7 recorded; M1 recorded as a design claim with no test, not as a kill.

---

### Task 6: The retriever, the gateway's embedder, and the metrics that separate a refusal from an error

**Files:**
- Create: `packages/shared/rag/retrieve.go`, `packages/shared/embed/fromenv.go`
- Modify: `packages/shared/metrics/metrics.go`, `apps/gateway/cmd/main.go`, `apps/indexer/cmd/embed.go` (delete `newEmbedder`, call the shared one)
- Test: `packages/shared/rag/retrieve_test.go`, `packages/shared/embed/fromenv_test.go`, `apps/gateway/cmd/main_test.go`

**Interfaces:**
- Produces:
  - `type Searcher interface { VectorSearch(...); LexicalSearch(...); SpanEmbedder(...) }`
  - `type Retriever struct { Store Searcher; Emb embed.Embedder; Mode Mode; K, Candidates int; Split bool; Floor Floor }`
  - `func (r *Retriever) Search(ctx context.Context, repoID, q string, limit int) (Result, error)`
  - `type Result struct { Hits []Fused; Spans map[string]models.Cite; TopScore float64; VectorRan bool; Mode Mode }`
  - `var ErrModelMismatch = errors.New("rag: repo was indexed by another embedder")`
  - `func embed.FromEnv(ctx context.Context, defaultDim int, timeout time.Duration) (Embedder, error)`
  - metrics: `ObserveRetrieval(mode string, d time.Duration)`, `ObserveTopScore(float64)`, `CountAnswer(outcome string)`, `CountRefusal(reason string)`, `SetFloor(value float64, calibrated bool)`

**Decisions, with their reasoning:**

- **The gateway needs an embedder, and that is new.** Retrieval embeds the *query*, so `EMBED_PROVIDER`, `EMBED_MODEL`, `EMBED_DIM` and `OLLAMA_URL` become gateway configuration. `newEmbedder` currently lives in `apps/indexer/cmd`, package `main`, and cannot be imported; it moves to `embed.FromEnv` so both binaries validate the same knobs the same way. `FromEnv` takes the width as an argument rather than importing `store`, so `embed` does not acquire a dependency on pgx; the caller still runs `store.CheckDim`.
- **The gateway refuses to boot on a bad embedder, exactly as the indexer does.** The cost is real and stated: with `EMBED_PROVIDER=ollama` and Ollama down, submission and job polling go down with retrieval. Parity is chosen over a partial-service state machine because the alternative — booting into a degraded mode — is the "silent downgrade" §8 calls the failure that costs a week, and there are no states in this codebase for it yet. Listed in Open Questions with the alternative.
- **The model guard is an error, not a refusal.** A repo indexed by another embedder is a *misconfiguration*: the corpus and the query are in different vector spaces, and every score is meaningless rather than low. Refusing would file it under "we had nothing to say", which is exactly the confusion §10 forbids in the other direction.
- **Candidate depth is per arm and separate from the returned limit.** RRF needs depth to have anything to fuse: fusing two lists of 10 is mostly an intersection test. `RETRIEVAL_CANDIDATES` defaults to **40**, which is also pgvector's default `hnsw.ef_search` — asking the ANN index for more rows than `ef_search` degrades recall silently. **Verify `SHOW hnsw.ef_search` against the pinned image**; if the default differs, either match it or `SET LOCAL hnsw.ef_search` in the same transaction, and record which.
- **Timing is measured in the retriever, outcome is counted in the handler.** The retriever owns the two arms and is the only thing that knows what "retrieval latency" covers; the handler owns the response and is the only thing that knows whether the request ended as an answer, a refusal or an error. Splitting them is what stops a single call site double-counting.
- **`codetrail_retrieval_top_score` is a histogram over `[0,1]`, and it is P3's contribution to P6.** It is the production instrument, not the calibration: §9 calibrates the floor from *the eval's* distribution over a generated golden set, where a hit is labelled correct. A Prometheus histogram has no labels for correctness and cannot distinguish a confident wrong answer from a confident right one. Say that in the metric's `Help` string, so nobody calibrates from Grafana.
- **Every metric label is a closed set** (`metrics` package doc): `mode` ∈ 3, `outcome` ∈ 3, `reason` ∈ 3. No repo id, no path, no question.

- [ ] **Step 1: Write the failing tests**

`retrieve_test.go` is hermetic, against a fake `Searcher` and `embed.Fake`. The fake searcher is what makes arm behaviour controllable — an end-to-end test cannot separate the arms (rule 5).

```go
// Mode decides which arms run. A fake Searcher that records its calls is the
// only way to assert an arm did NOT run: an empty result from an arm that ran
// looks identical to an arm that did not.
func TestModeVectorDoesNotRunTheLexicalArm(t *testing.T)
func TestModeLexicalDoesNotRunTheVectorArmOrEmbedTheQuery(t *testing.T)
func TestModeHybridRunsBoth(t *testing.T)

// The floor reads the vector arm's top cosine similarity, and Result carries
// it. The fake returns descending scores so "top" is unambiguous.
func TestTopScoreIsTheVectorArmsBestSimilarity(t *testing.T)

// In lexical mode there is no cosine similarity at all, and VectorRan is what
// tells Decide not to apply a cosine floor to a ts_rank.
func TestLexicalOnlyResultsAreNotScoredForTheFloor(t *testing.T)

// A corpus embedded by another model is a misconfiguration, not a low score.
func TestARepoIndexedByAnotherModelIsAnError(t *testing.T)

// Candidate depth is per arm; the limit applies after fusion. Fusing two
// lists of `limit` would make fusion an intersection test.
func TestArmsAreQueriedAtCandidateDepthAndTrimmedAfterFusion(t *testing.T)
```

`fromenv_test.go` moves the indexer's existing coverage of `newEmbedder` (bad provider, bad URL, bad dim, probe failure) into `embed`, unchanged in substance — a move, not a rewrite, so the existing kills stay valid.

`main_test.go` (gateway) mutates the **wiring**, which was P1's blind spot and P2's Task 6 lesson:

```go
func TestGatewayRefusesToBootOnAnEmbedderOfTheWrongWidth(t *testing.T)
func TestGatewayRefusesToBootOnAnUnknownRetrievalMode(t *testing.T)
func TestGatewayRefusesAFloorOutsideTheCosineRange(t *testing.T)
func TestTheFloorGaugeIsSetFromTheConfiguredValue(t *testing.T)
func TestTheDefaultRetrieverIsHybridAtTheUncalibratedFloor(t *testing.T)
```

- [ ] **Step 2: Implement**

`Search`: validate `limit`; `SpanEmbedder(repoID)` and compare against `r.Emb.Model()`; embed the query once (skipped entirely in lexical mode — an unnecessary model call on every lexical query is the sort of thing that only shows up in a bill); run the arms; `Fuse(r.K, vec, lex)`; trim to `limit`; record `TopScore` from the vector arm's first hit (`NaN` when it did not run).

Metrics:

```go
// topScore is the distribution spec §11 asks for and the instrument P6 reads.
// It is NOT the calibration: §9 calibrates the floor from the eval's own
// distribution over a labelled golden set, and a histogram has no label for
// "was this hit correct" — a confident wrong answer and a confident right one
// land in the same bucket.
topScore = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "codetrail_retrieval_top_score",
	Help:    "Cosine similarity of the best-ranked span. Production instrument; the floor is calibrated in P6 from the eval's labelled distribution, not from this.",
	Buckets: prometheus.LinearBuckets(0, 0.05, 21),
})
```

- [ ] **Step 3: Commit, then prove the tests discriminate**

**M1 — `Mode` ignored: always run both arms.**
- *Why the code exists:* the mode is the switch that makes spec:316's experiment runnable without a code change.
- *Fixture that separates mutant from original:* the call-recording fake `Searcher`. **A fixture that asserts on results cannot** — under `embed.Fake` both arms return nearly the same spans, so "the results look right" is true for every mode.
- *Must fail:* `TestModeVectorDoesNotRunTheLexicalArm`
- *Expected (verify and correct):* `lexical arm was queried in vector mode`

**M2 — embed the query even in lexical mode.**
- *Why the code exists:* a model call per query that nothing reads.
- *Fixture that separates mutant from original:* a counting `Embedder` wrapper. The results are identical either way, so nothing else can see it.
- *Must fail:* `TestModeLexicalDoesNotRunTheVectorArmOrEmbedTheQuery`
- *Expected (verify and correct):* `embedder called 1 time in lexical mode`

**M3 — `Candidates` replaced by `limit` in both arm calls.**
- *Why the code exists:* fusion over two short lists degenerates into an intersection test.
- *Fixture that separates mutant from original:* the fake records the limit it was asked for; and a second assertion that a span ranked 30th by the vector arm and 1st by the lexical arm survives into a top-10 fused result. With `limit=10` depth, that span is never seen.
- *Must fail:* `TestArmsAreQueriedAtCandidateDepthAndTrimmedAfterFusion`
- *Expected (verify and correct):* `vector arm asked for 10 candidates, want 40`

**M4 — the model guard deleted.**
- *Why the code exists:* a query and a corpus in different vector spaces produce confident nonsense.
- *Fixture that separates mutant from original:* the fake `SpanEmbedder` returning `"some-other-model"`. No real corpus in the suite has one.
- *Must fail:* `TestARepoIndexedByAnotherModelIsAnError`
- *Expected (verify and correct):* `got 3 hits and nil error, want ErrModelMismatch`

**M5 — the model guard returns a refusal instead of an error.**
- *Why the code exists:* spec §10's distinction runs in both directions; a misconfiguration filed as "nothing to say" is the same lie as an error rate hiding a refusal.
- *Fixture that separates mutant from original:* the same fake; the test asserts `errors.Is(err, ErrModelMismatch)` rather than "an unsuccessful outcome".
- *Must fail:* `TestARepoIndexedByAnotherModelIsAnError`

**M6 — `TopScore` taken from the fused score instead of the vector similarity.**
- *Why the code exists:* Task 1 proved the fused score carries no quality signal.
- *Fixture that separates mutant from original:* the fake vector arm returns a top similarity of `0.83`; the fused top is `1/61 ≈ 0.0164`, and the test asserts the exact value. A test asserting only "TopScore > 0" survives.
- *Must fail:* `TestTopScoreIsTheVectorArmsBestSimilarity`
- *Expected (verify and correct):* `TopScore 0.0163934, want 0.83`

**M7 — `store.CheckDim` deleted from the gateway's boot path.**
- *Why the code exists:* an embedder of the wrong width means every query is ranked against a column it cannot be compared to; the indexer refuses to boot for this and the gateway must too.
- *Fixture that separates mutant from original:* `EMBED_DIM=64` in the boot test. Nothing in a default-configured test reaches it.
- *Must fail:* `TestGatewayRefusesToBootOnAnEmbedderOfTheWrongWidth`

**M8 — the floor's `Validate` skipped at boot.**
- *Why the code exists:* fail closed, as `chunk.Options` and `walk.Limits` do.
- *Fixture that separates mutant from original:* `ANSWER_SCORE_FLOOR=7` in the boot test.
- *Must fail:* `TestGatewayRefusesAFloorOutsideTheCosineRange`

**M9 — `SetFloor` called with `calibrated: true`.**
- *Why the code exists:* it is the gauge that shows an operator a guess as a guess.
- *Fixture that separates mutant from original:* `TestTheFloorGaugeIsSetFromTheConfiguredValue`, reading the registry.
- *Must fail:* `TestTheFloorGaugeIsSetFromTheConfiguredValue`

- [ ] **Step 4: Commit**

```bash
git commit -m "feat(rag): the retriever, and the gateway's own embedder

Retrieval embeds the query, so the gateway acquires the four embedder
knobs and boots on the same validation the indexer does — newEmbedder
moves out of package main into embed.FromEnv rather than being written
twice. The cost of parity is stated: with Ollama down, submission goes
down with retrieval.

A repo indexed by another embedder is an error, not a refusal. The corpus
and the query are then in different vector spaces and every score is
meaningless rather than low; filing that under 'we had nothing to say' is
the confusion spec §10 forbids, read in the other direction.

The top-score histogram is P3's contribution to P6's calibration and is
labelled as an instrument, not as the calibration: a histogram has no
label for whether a hit was correct."
```

**Definition of Done**
- Three modes, each provably running only its own arms, switched by configuration.
- Candidate depth separate from the returned limit, verified against `hnsw.ef_search`.
- Model mismatch is an error and says so.
- Both binaries validate the embedder through one function.
- Metrics exist with closed label sets, and the floor's gauges say uncalibrated.
- M1–M9 recorded.

---

### Task 7: The read endpoints — search, span, repo, ask, refusal

**Files:**
- Create: `apps/gateway/internal/handler/read.go`, `packages/shared/rag/answer.go`
- Modify: `apps/gateway/internal/handler/handler.go` (Mount), `apps/gateway/cmd/main.go` (wire the reader)
- Test: `apps/gateway/internal/handler/read_test.go`, `packages/shared/rag/answer_test.go`

**Routes:**

| route | answers |
| --- | --- |
| `GET /api/repos` | the indexed corpus, newest-used first, bounded |
| `GET /api/repos/:repo` | commit, ref, indexed_at, files, spans, files_with_spans, staleness — `404` unknown, `410` evicted |
| `GET /api/repos/:repo/spans/:span` | one span with its citation — `404`, `410` |
| `POST /api/repos/:repo/search` | ranked hits with per-arm ranks and citations |
| `POST /api/repos/:repo/ask` | an extractive answer, or a refusal |

**Decisions, with their reasoning:**

- **`POST` for search and ask, with the query in the body.** A question in a query string is logged by every proxy, load balancer and access log between the caller and the process, and this is the one string in the phase that must not be. It is also unbounded in a way a URL is not.
- **A refusal is `200` with `"refused": true`, not `4xx` or `5xx`.** Spec §10 requires refusal and error to be distinct outcomes; expressing a refusal as an HTTP error would put it in every error-rate panel in existence, which is precisely the hiding §10 forbids. The response says which reason.
- **Query validation at the edge, naming the rule** (spec §10's "never a generic refusal"): non-empty after trimming, ≤ 1,000 characters, and at least one usable term. Without the last check, `"???"` reaches `embed.Fake`, which refuses a text it can hash no token from, and a user's punctuation becomes a `500`.
- **`limit` is bounded 1..50** and a value outside that is a `400`, not a silent clamp: a caller asking for 5,000 spans has misunderstood something, and quietly serving 50 hides it.
- **`TouchRepo` on every successful read.** This is the LRU clock spec §4 designed and P1 shipped with nothing to wind it — `evict.go` says so in its comment, and that comment becomes false in this task and must be updated. A failure to touch is logged and dropped: it is a hint for a future eviction, not part of the caller's answer.
- **The assembler never truncates a span.** A citation's digest is a claim about the text; showing 200 of a span's 400 lines under a digest of all 400 is a citation that lies, and it is the exact failure mode the digest exists to make impossible. Over budget, whole spans are dropped and the response says how many.
- **The response always names what answered it** — `"answered_by": "extractive"` — even though there is only one answerer until P7. §8 calls a silent downgrade the failure that costs a week; the field has to exist *before* there is something to downgrade from, or the console and every client will be written without it.
- **`top_score` is a `*float64`.** `json.Marshal` fails outright on `NaN` — the response would be a `500` with an empty body — and a lexical-only or empty result has no score. `null` is the honest encoding.

- [ ] **Step 1: Write the failing tests**

`answer_test.go`, hermetic:

```go
// Over budget, whole spans are dropped. Truncating one would put a digest on
// text that is not the text the digest was taken of.
func TestAssembleDropsWholeSpansAndNeverTruncatesOne(t *testing.T) {
	// Fixture: three spans of 4,000 characters each with a budget of 9,000.
	// A budget larger than the corpus cannot detect a truncating assembler,
	// and equal-length spans cannot detect a wrong drop order.
}
func TestAssembleNumbersCitationsInRankOrder(t *testing.T)
func TestAssembleReportsWhatItDropped(t *testing.T)
func TestEveryRenderedSpanStillMatchesItsDigest(t *testing.T)
```

`read_test.go`, hermetic against fakes for the retriever and the store. The fixtures:

- a repo with three spans (rule 1);
- an unknown repo id;
- a tombstoned repo id;
- a retriever that returns nothing (`no_spans`);
- a retriever that returns hits with a top score of `0.2` against a floor of `0.5` (`below_floor`);
- a retriever that returns an error (the outcome that must **not** be counted as a refusal).

```go
func TestSearchReturnsPerArmRanksSoFusionCanBeMeasured(t *testing.T)
func TestSearchDoesNotRefuse(t *testing.T)            // the floor lives on ask only
func TestAskAnswersWithCitationsAndAPermalink(t *testing.T)
func TestAskRefusesUnderTheFloorWithTwoHundredAndAReason(t *testing.T)
func TestAskRefusesWhenNothingWasRetrieved(t *testing.T)
func TestARetrievalErrorIsFiveHundredAndIsNotCountedAsARefusal(t *testing.T)
func TestARefusalIsNotCountedAsAnError(t *testing.T)
func TestAnEmptyOrTokenlessQuestionIsFourHundredNamingTheRule(t *testing.T)
func TestAnUnknownRepoIsFourOhFour(t *testing.T)
func TestAnEvictedRepoIsFourTenNotFourOhFour(t *testing.T)
func TestASuccessfulReadWindsTheLRUClock(t *testing.T)
func TestTheQuestionIsNeverLogged(t *testing.T)       // capture the zerolog output
func TestANaNTopScoreSerialisesAsNull(t *testing.T)
```

`TestARetrievalErrorIsFiveHundredAndIsNotCountedAsARefusal` and its mirror are the two that carry spec §10. Both read the Prometheus counters before and after and assert the *delta on both counters*, not just the one they expect to move — a test that only checks its own counter passes under a mutant that increments both.

- [ ] **Step 2: Implement**

Sketch of the ask handler's shape:

```go
out, err := h.Rag.Search(ctx, repoID, req.Q, h.AnswerSpans)
if err != nil {
	metrics.CountAnswer("error")
	return h.fail(c, err, "ask")
}
outcome, reason := rag.Decide(h.Floor, len(out.Hits), out.TopScore, out.VectorRan)
if outcome == rag.OutcomeRefused {
	metrics.CountAnswer("refused")
	metrics.CountRefusal(string(reason))
	return c.JSON(http.StatusOK, refusal{…})   // 200: a refusal is an outcome, not an error
}
metrics.CountAnswer("answered")
```

- [ ] **Step 3: Commit, then prove the tests discriminate**

**M1 — a refusal answered as `422` (or `404`).**
- *Why the code exists:* an HTTP error code puts a refusal in every error-rate panel, which is §10's failure with a different spelling.
- *Fixture that separates mutant from original:* the below-floor retriever. The no-hits one works too; keep both, since they take different branches.
- *Must fail:* `TestAskRefusesUnderTheFloorWithTwoHundredAndAReason`
- *Expected (verify and correct):* `want 200 with refused=true, got 422`

**M2 — `metrics.CountAnswer("error")` in the refusal branch.**
- *Why the code exists:* spec §10, the sentence this whole phase is built around.
- *Fixture that separates mutant from original:* the counter delta assertion on **both** counters. A test asserting only that the refusal counter moved passes under this mutant, because the mutant still counts the refusal reason.
- *Must fail:* `TestARefusalIsNotCountedAsAnError`
- *Expected (verify and correct):* `answer_total{outcome="error"} moved by 1 on a refusal`

**M3 — `metrics.CountAnswer("refused")` in the error branch.**
- *Why the code exists:* the same sentence, read the other way.
- *Fixture that separates mutant from original:* the erroring fake retriever.
- *Must fail:* `TestARetrievalErrorIsFiveHundredAndIsNotCountedAsARefusal`

**M4 — the assembler truncates the last span to fit the budget.**
- *Why the code exists:* a digest over text that was then cut is a citation that lies.
- *Fixture that separates mutant from original:* three 4,000-character spans against a 9,000-character budget, so the third span *must* be dealt with. A budget bigger than the corpus never reaches the branch. The assertion is `strings.Contains(answer, span.Text)` for every cited span — under the mutant the truncated span's full text is absent while its citation is present.
- *Must fail:* `TestAssembleDropsWholeSpansAndNeverTruncatesOne`
- *Expected (verify and correct):* `citation 3 names a digest whose text is not in the answer`

**M5 — `TouchRepo` removed from the read path.**
- *Why the code exists:* it is the LRU clock; without a reader winding it, "least recently *queried*" means "least recently indexed" and a popular repository is evicted while it is being used.
- *Fixture that separates mutant from original:* a fake store recording touches. Nothing about the response changes, so no assertion on the body can see it.
- *Must fail:* `TestASuccessfulReadWindsTheLRUClock`
- *Expected (verify and correct):* `TouchRepo was not called`

**M6 — a `TouchRepo` failure returned to the caller.**
- *Why the code exists:* the touch is a hint for a future eviction, not part of the answer; failing a correct answer on it trades a good response for a bookkeeping error.
- *Fixture that separates mutant from original:* a fake store whose `TouchRepo` errors while everything else succeeds.
- *Must fail:* `TestASuccessfulReadWindsTheLRUClock` (extend it with the erroring case) — **verify this predicts a kill**; if the test only asserts the call happened, the mutation survives and the test needs the second case.

**M7 — the evicted branch answers `404`.**
- *Why the code exists:* spec §10 — it existed, and that is a different fact.
- *Fixture that separates mutant from original:* the tombstoned id. An unknown id is `404` under both.
- *Must fail:* `TestAnEvictedRepoIsFourTenNotFourOhFour`

**M8 — query validation removed.**
- *Why the code exists:* `"???"` otherwise reaches the embedder, which refuses a text with no tokens, and the user sees a `500`.
- *Fixture that separates mutant from original:* the `"???"` case with a **real `embed.Fake`** behind the retriever, not a stub that returns hits regardless. A stubbed retriever swallows the mutation and the test passes.
- *Must fail:* `TestAnEmptyOrTokenlessQuestionIsFourHundredNamingTheRule`
- *Expected (verify and correct):* `want 400 with a rule, got 500`

**M9 — `top_score` marshalled as a plain `float64`.**
- *Why the code exists:* `json.Marshal` errors on `NaN`, and echo turns that into an empty `500`.
- *Fixture that separates mutant from original:* the lexical-only or empty-result case, whose top score is `NaN`. Every scored fixture passes.
- *Must fail:* `TestANaNTopScoreSerialisesAsNull`
- *Expected (verify and correct):* `json: unsupported value: NaN`

**M10 — the question logged at info level.**
- *Why the code exists:* a public endpoint's user input must not reach an operator's log aggregator.
- *Fixture that separates mutant from original:* a distinctive question string and a captured zerolog writer. A test that checks the response body cannot see a log line.
- *Must fail:* `TestTheQuestionIsNeverLogged`

- [ ] **Step 4: Commit**

```bash
git commit -m "feat(gateway): search, read, and an extractive answer that can be refused

A refusal is 200 with refused=true and a reason. Expressing it as an HTTP
error would file 'we had nothing to say' inside every error-rate panel,
which is the confusion spec §10 exists to prevent; both counters are
asserted on both paths so neither can absorb the other.

The question travels in a POST body: a query string is logged by every
proxy between the caller and the process, and it is the one string in
this phase that must not be.

The assembler drops whole spans rather than truncating one, because a
digest over text that was then cut is a citation that lies — which is the
one thing a code citation is supposed to make impossible.

TouchRepo now runs on the read path, which is what evict.go's comment
said would happen in P3; that comment is updated rather than left true by
accident."
```

**Definition of Done**
- Five routes, with `400` naming a rule, `404` for unknown, `410` for evicted, `500` withholding internals.
- A refusal is a `200` with a reason and is never counted as an error; an error is never counted as a refusal; both directions asserted on both counters.
- No question in any log line.
- No citation whose digest does not match the text shown.
- M1–M10 recorded.

---

### Task 8: End to end on a live database, CI, and the README

**Files:**
- Create: `apps/gateway/internal/handler/read_live_test.go`, `assets/screenshots/ask.png`
- Modify: `.github/workflows/ci.yml`, `README.md`, `Makefile`, `infra/docker-compose.yml` (gateway's embedder env)
- Test: as above

- [ ] **Step 1: Write the end-to-end live tests**

Index a fixture repository through the real indexer path (as `apps/indexer/cmd/index_live_test.go` already does), then drive the handler over the result on the fake embedder:

```go
func TestAskOverARealIndexReturnsACitationWhoseDigestMatchesTheSpan(t *testing.T)
func TestAHighFloorRefusesTheSameQueryTheDefaultAnswers(t *testing.T)
func TestAnEvictedRepoAnswersGoneOnEveryReadRoute(t *testing.T)

// The asymmetry P2 measured, pinned rather than papered over: a file holding
// only a package comment produces no AST spans, so under the production
// strategy its prose is unretrievable and the honest answer is a refusal. The
// same corpus under CHUNK_STRATEGY=window answers it.
func TestPackageDocProseIsUnretrievableUnderTheASTStrategy(t *testing.T)
```

`TestAHighFloorRefusesTheSameQueryTheDefaultAnswers` is the test that proves the floor is a live mechanism without asserting anything about its value: the same query, the same corpus, two floors, two outcomes.

- [ ] **Step 2: Extend CI**

The `live datastore tests` step already sets `EMBED_PROVIDER: fake`. Add the gateway's retrieval knobs to the same step so the composition under test is the one CI describes, and add a comment saying **CI cannot measure fusion** — the fake is a hashed bag of words over the same text the lexical arm indexes, so the two arms are near-duplicates there. A green build is a mechanics check (spec §9's banner), and this is where a reader of the build log will look.

- [ ] **Step 3: Verify against a real repository, and record the numbers**

Not optional, and not a formality — P2 found four cross-task defects here that no per-task review caught.

```bash
make up
# index rs/zerolog exactly as P2's README records it, then:
curl -s -XPOST localhost:8080/api/repos/$REPO/ask -d '{"q":"how does the sampler decide to drop an event"}' | jq .
```

Record, in the README and in the commit message:

1. the top score distribution over a handful of real questions — **as a range, not as a recommendation**, and with the sentence that P6 is what turns it into a floor;
2. a citation checked the way P2 checked its spans: `git show <sha>:<path> | sed -n '<start>,<end>p' | sha256sum` against the `digest` the API returned;
3. the permalink opened in a browser, landing on the cited lines;
4. retrieval latency for each mode, so the cost of the second arm is a number rather than an impression;
5. the same query with `RETRIEVAL_MODE=vector` and `=hybrid`, **reported as two result sets and not as a winner**.

- [ ] **Step 4: Update the README**

Four edits are mandatory; the rest is new material.

1. **The existing "Grounded, or refused" section is false as of this phase and must be replaced.** It currently reads: *"The score floor that decides this is **measured** — calibrated from the eval's distribution and recorded with the numbers that produced it — not picked because it sounded about right."* That describes P6. Replace with, in substance:

   > **Grounded, or refused.** codetrail refuses when retrieval returns nothing, and when the top cosine similarity falls under `ANSWER_SCORE_FLOOR`. **That floor ships uncalibrated**: its default is `-1`, the bottom of the cosine range, which refuses nothing on score alone, and every answer carries `"floor": {"value": -1, "calibrated": false}`. The number is measured in **P6**, from the eval's distribution over a generated golden set, and will be recorded here with the figures that produced it. What P3 ships is the mechanism and the instrument — the refusal path, the knob, and the top-score histogram the calibration reads — because picking a threshold before measuring one is a guess wearing a measurement's clothes.

   The wording must not be readable as "the floor is calibrated" under any skim. "Measured in P6" and "ships uncalibrated" both appear.

2. **The citation paragraph's staleness claim must become honest.** It currently says citations "say so explicitly when the ref has moved on since indexing". codetrail does not ask the forge. Replace with: a citation states the commit and when it was indexed, says in words that **codetrail has not checked whether the ref has moved**, and upgrades to "superseded by commit X" only when the same ref is also indexed here at a later commit.

3. **"No model call" is false.** The "Offline by default" section says the extractive path makes "no model call, no API key, no marginal cost". The *query* is embedded on every search and every ask, which is a model call — to a local embedder, with no API key and no vendor cost. Say that instead, because it is also what makes the gateway's new boot dependency on Ollama non-obvious to a reader.

4. **The phase table**: P3 → done.

New material: a **Retrieval** section carrying the two arms, the fusion constant, the modes, the sentence that **nothing here claims fusion retrieves better** (spec:316), the per-arm ranks in the response, the knob list, the `410`/`404` distinction, the job retention window and the flood it does not bound, and the `doc.go` asymmetry as a *retrieval* limitation rather than only a chunking one.

- [ ] **Step 5: Commit and prove the tests discriminate**

**M1 — `RETRIEVAL_MODE` default flipped to `vector`.**
- *Why the code exists:* §8 defines retrieval as hybrid; the default is spec conformance, not a quality claim.
- *Must fail:* `TestTheDefaultRetrieverIsHybridAtTheUncalibratedFloor` (Task 6)

**M2 — `ANSWER_SCORE_FLOOR` default set to `0.35`.**
- *Why the code exists:* spec:315.
- *Fixture that separates mutant from original:* the live pair in `TestAHighFloorRefusesTheSameQueryTheDefaultAnswers`, plus Task 1's `TestTheShippedDefaultRefusesNothingOnScoreAlone`. **Expected to kill in Task 1 and to be visible here as a refusal on a query the default answers** — record whether the live test actually changes outcome, because it depends on the fake's scores, and if it does not, say so rather than claiming a kill.

**M3 — the end-to-end digest assertion weakened to "the digest is non-empty".**
- *Why the code exists:* this test is the one that proves a citation is checkable, which is the project's second differentiating claim.
- *Fixture that separates mutant from original:* the mutation is to the *test*, which is legitimate here: the question is whether the assertion is load-bearing. Apply a truncation to the assembler and confirm the weakened test passes while the real one fails.

- [ ] **Step 6: Commit**

```bash
git commit -m "feat(gateway): P3 end to end, and a README that does not claim a calibrated floor

The README said the score floor was measured. It is not, and P6 is where
it becomes so; the wording now says 'ships uncalibrated' and 'measured in
P6' in the same paragraph, and every answer carries calibrated=false.

It also said citations say so when the ref has moved. codetrail never
asks the forge — that would put a call to a stranger's host on the public
read path — so the claim is now what it can actually support: correct at
this commit, indexed then, not checked since, and 'superseded' only when
a later commit of the same ref is also indexed here.

Measured against rs/zerolog at dfd11cca: <paste>. The two retrieval modes
are reported as two result sets and not as a winner; which one is better
is spec:316's experiment."
```

**Definition of Done**
- The whole path runs on a live database on the fake embedder in CI.
- A citation returned by the API was checked byte-for-byte against `git show`, and the permalink was opened.
- The README's floor and staleness claims are true, and a skim of either cannot produce "the floor is calibrated".
- The `doc.go` asymmetry is pinned by a test and named in the README as a retrieval limitation.

---

## Definition of done for P3

- [ ] Hybrid retrieval: pgvector cosine filtered by repo, fused by reciprocal rank with a lexical arm over symbol names and identifiers, switchable and measurable per arm.
- [ ] Citations carry `(repo, commit, path, startLine, endLine, digest)`, render an immutable permalink for each supported forge and none for a forge whose format is unknown, and state exactly what is known about staleness and what is not.
- [ ] An extractive answer assembles ranked spans with citation markers, never truncating a span under its own digest, with no model call and no API key.
- [ ] A refusal happens when nothing is retrieved, when the top score falls under the floor, and when the top score is not a number — and it is a `200` with a reason.
- [ ] Refusal and error are distinct in the metrics, asserted in both directions on both counters.
- [ ] The floor is a knob, ships at `-1`, is labelled uncalibrated in the payload, the gauges, the boot log and the README, and P6 is named as where the number comes from.
- [ ] The top-score histogram exists and its `Help` string says it is an instrument, not a calibration.
- [ ] An evicted repo answers `410`; an unknown one `404`; job history is bounded by a stated age window that never deletes a running job.
- [ ] A completed job names the repository it produced.
- [ ] Every mutation is recorded with its observed output, and every survivor is recorded as a survivor with what it revealed.

**Not in P3, deliberately:** no symbol graph and no `definition_of`/`callers_of` (P4) — the spec lists them as tools for the LLM loop, which is P7. No LLM answering: the response already names `answered_by` so the field exists before there is anything to downgrade from. No console (P5). No eval harness, golden set or calibration (P6) — P3's contribution to it is the instrument and the switchable arms. No incremental re-index (P7). No rate limit on submission, which is the control the job-retention window explicitly does not replace. No `/metrics` on the indexer, so its eviction and job-outcome counters have nowhere to live yet.

---

## Open questions

Nothing here is blocking. Each is something the spec does not settle, with the tradeoff and a recommendation, so a reviewer can disagree with a decision rather than discover it.

1. **The fusion constant `k`.** Spec §8 names reciprocal-rank fusion and no constant.
   *Recommendation:* **60**, the value from the paper the method comes from (Cormack, Clarke & Buettcher, 2009), as `RETRIEVAL_RRF_K`. It is a discount on deep ranks and its effect is only visible where the arms disagree sharply — Task 1's test pins that. It is a knob because spec:316 makes fusion itself an experiment, and sweeping `k` is part of that experiment. **It is not a measured value for this corpus** and the README must not imply it is.

2. **Candidates per arm.** Spec §8 says nothing about depth.
   *Recommendation:* **40 per arm**, returning 10, as `RETRIEVAL_CANDIDATES`. Two reasons: fusing two lists of 10 is mostly an intersection test, and pgvector's `hnsw.ef_search` defaults to 40 — asking the ANN index for more rows than `ef_search` degrades recall with no error. Verify the pinned image's actual default before fixing the number; if it differs, either match it or `SET LOCAL hnsw.ef_search` in the same transaction and say so in a comment.

3. **Permalink formats per forge.** Spec §8 gives one example, `…/blob/<sha>/path#L10-L20`, which is GitHub's. Codeberg runs Forgejo: `…/src/commit/<sha>/path#L10-L20`.
   *Recommendation:* a table of two, and **no link at all** for a host that is not in it. The alternatives are worse in both directions: guessing GitHub's shape for an operator-added GitLab produces a URL that 404s, which a reader will read as "the code is gone"; and refusing to cite at all would throw away the tuple that is the real claim. A third option — deriving the format from a probe of the forge — puts a network call to a stranger's host on the read path, which is the egress P1 confined to the indexer.

4. **What the lexical arm indexes.** Spec §8 says "symbol names and identifiers"; this plan indexes `symbol` at weight A and the whole span `text` at weight B, which also covers keywords, string literals and comment prose.
   *Recommendation:* ship it, because a span's identifiers *are* its text's tokens and extracting only identifiers means a new column written from the AST at index time, an indexer change and a full re-index. Two consequences to revisit in P7 with evidence: comment prose in the index makes the lexical arm partly redundant with the vector arm in exactly the case where doc comments are the vector arm's best signal (which is one honest reason fusion's value is genuinely unknown), and identifier splitting is query-side only, so `parse config` does not find `parseConfig` unless the prose spells it out. Index-side splitting fixes the second and costs a re-index.

5. **Query prefixes for `nomic-embed-text`.** The model is trained with `search_document:` and `search_query:` prefixes; P2 embedded the corpus with neither, and P3 embeds queries with neither.
   *Recommendation:* **do not add them in P3.** Adding a query prefix against a corpus embedded without a document prefix is the worst of the three combinations, and doing it properly invalidates every existing span and costs a full re-index. It is a clean P6 experiment: two corpora, one metric, an answer. Recorded here because it is invisible in the code and someone will otherwise "fix" it in a one-line commit.

6. **The gateway's boot parity on the embedder.** With `EMBED_PROVIDER=ollama` and Ollama down, the gateway now refuses to start — so submission and job polling go down with retrieval.
   *Recommendation:* parity with the indexer for now. The alternative is booting into a degraded state where retrieval `503`s and submission works, which is better availability and worse honesty: it is a silent downgrade of exactly the kind §8 calls the failure that costs a week, and there is no state in the codebase for it. Revisit in P5, when a console makes the difference visible to a user.

7. **Whether `/ready` should probe the embedder.** It probes Postgres only.
   *Recommendation:* Postgres only, for now. An embed call per readiness check is ~100 ms of CPU on the same machine serving queries, every 10 s, and its failure mode is per-request rather than per-process. The cost of being wrong is that `/ready` answers 200 while every ask errors — which the answer-outcome counter shows. Revisit with the console.

8. **The answer budget.** Spec §8 says "ranked spans assembled with citation markers" and gives no size.
   *Recommendation:* `ANSWER_MAX_SPANS=5`, `ANSWER_MAX_CHARS=8000`. Five citations is what a person reads; 8,000 characters is roughly two screens and is also the right order of magnitude for P7's tool loop, so the two paths will not need different numbers. Both knobs, so P6 can vary them.

9. **The tie-break rule.** Ties are real: two documents at ranks (1,2) and (2,1) fuse identically.
   *Recommendation:* `(path, start_line, span_id)`. Deterministic — two runs over an unchanged corpus diff cleanly — and readable, which id-only ordering is not. The cost is that a repository with one very long file gets its ties resolved by position, which is arbitrary but at least legible.

10. **The job retention window.** Spec §14 asks what a caller may still poll for.
    *Recommendation:* **168 hours after a job goes terminal**, as `JOB_HISTORY_HOURS`; running jobs are never swept. Stated to the caller in the README, because a retention rule nobody can read is a `404` with no explanation. What it does not bound is a flood inside the window; that control is a rate limit on `POST /api/repos`, which does not exist and is not added here.

11. **The tombstone bound.** `KEEP_TOMBSTONES=500`, ten times `KEEP_REPOS`.
    *Recommendation:* as above. Past it, an evicted repo goes back to `404`, which is honest — we no longer remember. A time-based bound would be equally defensible; count is chosen because the table's purpose is "recently evicted" and its growth is bounded by eviction, which is itself bounded.

12. **Generated column versus expression index for the lexical arm.**
    *Recommendation:* the generated column. It costs a table rewrite on migration and roughly a third more storage on `spans`, and it buys a query that reads `lex @@ q` instead of repeating a two-`setweight` expression that has to match the index's definition character for character. Revisit if the corpus grows two orders of magnitude.

13. **How a caller finds a repo id.** `jobs.repo_id` covers the submit-then-poll flow, and `GET /api/repos` lists the corpus.
    *Recommendation:* both, as specified. What is deliberately *not* added is `GET /api/repos?remote=…&ref=…`: it is the right endpoint for the console, and P5 is where the console's needs are known. After the job history window, a caller's only route to an id is the listing — which is correct, since the corpus is itself LRU-evicted and a link to a repo that is gone should not resolve.

14. **Where the eval's two arms live** (carried forward from P2, unchanged and still open). `RepoID` and `SpanID` have no room for a strategy, so the arms want a database each. P3 does not foreclose it: the retriever takes a store, the store takes a DSN, and nothing in this phase joins across arms or infers a strategy from a row. P6 decides.

---

## What the spec leaves underspecified or in tension

Recorded so a reviewer sees them as findings rather than as choices made quietly.

- **§8's floor has no well-defined subject.** "A refusal when the top score falls under a floor" presumes a single score, but §8 also specifies reciprocal-rank fusion, whose output score is a function of ranks alone: the top hit of any non-empty result scores `1/(k+1)` whether it is perfect or worthless. Taken literally, the floor would be a redundant spelling of "did anything come back". This plan resolves it by comparing the floor against the **vector arm's cosine similarity**, the only bounded, query-comparable, quality-carrying number in the pipeline, and pins the reasoning with a test that shows the fused score is identical for a perfect arm and a junk one. **P6 must calibrate the same quantity**, or the number it measures will not be the number the code compares.
- **§8's permalink example is one forge's, and the shipped default allowlist has two.** `…/blob/<sha>/path#L10-L20` is GitHub. Codeberg — settled into the default allowlist in P1 — is `…/src/commit/<sha>/path#L10-L20`. A plan that implemented §8 literally would ship a citation format that is wrong for half the forges the product accepts by default, and the failure is invisible in any GitHub-only test.
- **§8 requires a staleness claim it gives no mechanism for.** "If the ref has moved since indexing, the API says the citation is from an older commit" can only be learned by asking the forge — a network call to a stranger's host on the public read path, which contradicts P1's confinement of egress to the indexer — or by inferring from the corpus, which supports only the narrower claim "this ref is also indexed here at a later commit". P3 does the latter and says the rest out loud. A future phase that adds a forge check should do it in the indexer, on a schedule, not on the read path.
- **§10's status vocabulary has no entry for two failures this phase creates.** A repository indexed by a different embedder, and an embedder that is unreachable, are both server-side and neither is a `400`, `404`, `410` or a `500` in any satisfying sense. P3 answers `500` with the reason in the log and the request id in the body, matching the existing `fail` path, and records that `409` or `503` would each be more informative and neither is in the spec's list.
- **§10 requires `410` for an evicted repo, and the schema as built cannot produce it.** Eviction is one `DELETE` with a cascade (spec §3 makes that an invariant), after which nothing distinguishes an evicted repository from one that was never submitted. The requirement is unimplementable without a tombstone, which no earlier phase had a reason to add. Task 5 adds one; the invariant "eviction is one DELETE" survives, because the tombstone is written by the same statement.
- **§11 implies both services expose `/metrics`; the indexer exposes nothing.** It has no HTTP server at all. So job outcomes and durations, admission rejections by reason, and evictions — three of the six counter groups §11 names — have nowhere to live. P3 ships only the gateway-side metrics (retrieval latency, top-score distribution, answer outcomes) and records the gap rather than inventing an unscrapable counter. Giving the indexer a probe server is P0-shaped work and belongs in its own task.
- **§2 says the gateway "writes only two things: job rows, and `repos.last_queried_at`".** P3 keeps that literally true: the read path winds `last_queried_at` and nothing else, and both deletes this phase adds — the job sweep and the eviction tombstone — run in the indexer. Worth stating, because the natural place to put a retention sweep is the process that serves the endpoint whose question it answers.
- **The README claims the extractive path makes "no model call".** It does make one: the *query* has to be embedded, which is a request to the local embedder on every search and every ask. What is true is that it needs no API key, has no per-request cost to a vendor, and works offline. Task 8 corrects the sentence; the distinction matters because it is also what makes the gateway's new boot dependency on Ollama non-obvious.
- **P2 recorded that `evict.go`'s comment says "nothing calls `TouchRepo` yet, since no endpoint reads a repo before P3".** That sentence becomes false in Task 7 and must be updated in the same commit that makes it false — P2's review round found twelve comments that had drifted this way, two of them introduced by commits fixing others.
- **§9 says the eval "runs the *same* retriever the gateway serves from".** That constrains the package layout of this phase, not P6's: `rag` must stay free of echo, of the handler's request types and of anything gateway-shaped, so `apps/evalrunner` can construct a `Retriever` directly. Task 6's `Searcher` interface is what keeps that true, and a P6 that has to reimplement retrieval has found a P3 defect.
