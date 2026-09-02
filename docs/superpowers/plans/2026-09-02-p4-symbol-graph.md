# codetrail P4 — symbol graph, per-edge provenance, graph endpoints

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the declarations P2 already parses into a graph that can answer "who calls this", and make every edge say how much it knows. Definitions from the AST. Call edges attempted through `go/packages` with type information, `resolved` where `types.Info.Uses` names a real object and `syntactic` with a **null target** where it does not. The label is a per-row property, so a repository where three packages type-check and two do not gets precise edges for three and honest approximations for two. Type-checking runs inside P1's sandbox, on the *job's own remaining wall-clock budget*, and its failure is an expected outcome that downgrades edges rather than an exception that fails a job.

**Architecture:** A new `packages/shared/symbols` holds the pure parts: definition extraction from the AST (sharing one identity function with `chunk`, so a symbol is spelled the same way in a span, in the lexical index and in the graph), call-site extraction keyed by byte offset, and the loader that runs `go/packages` under an environment policy that is the whole security story of this phase. `packages/shared/store` grows `PutGraph` (one transaction, whole-repo replace, deterministic ids) and the three graph reads, of which `CallersOf` is a recursive CTE — that CTE is the reason this project has one datastore and not a graph database beside it. `apps/indexer` gains one stage between chunking and completion. `apps/gateway` gains three read routes. One migration.

**Tech Stack:** Go 1.27, Postgres 17 recursive CTEs, `go/ast` + `go/parser` (already used by `chunk`), `go/types` + `golang.org/x/tools/go/packages`, echo v4, zerolog, prometheus/client_golang. **One new dependency:** `golang.org/x/tools`, which spec §7 names explicitly as in scope.

**Spec:** `docs/superpowers/specs/2026-08-31-codetrail-design.md` — **§6 is this phase and it is fifteen lines; read it exactly** (spec:184–197), then §7 (language scope), §3 (the `symbols` and `edges` rows and the "syntactic edge has a null target" invariant), §10 (error handling), §11, §13 and §14. §8's `definition_of` and `callers_of` are named there as *tools for P7's LLM loop*; P4 ships the endpoints beneath them, not the loop.

**Predecessor:** `docs/superpowers/plans/2026-09-01-p3-retrieval.md`. P3 merged at `6179a38`. This plan keeps P3's mutation *blocks* and adds two things P3's own review round earned: an explicit ledger of side effects with the test that reads each one back, and an explicit ledger of fakes with the test that sets each one's error. Both are in Task 7, and both exist because P3's per-task rounds each reported "no survivors" while the whole-branch sweep after them found about twenty.

---

## What §6 decides, and what this plan may therefore not do

§6 is short enough that every clause in it is load-bearing. Four of them are decisions, not suggestions, and each has a plausible-looking opposite that a plan could ship without noticing.

### 1. Provenance is per **edge**. Not per repo, not per package, not per file.

Spec:190 — "**Provenance is per-edge, not per-repo.** … This makes the label a per-row property a test can pin."

The tempting implementation is the wrong one: run `go/packages`, ask "did it work", and stamp every edge in the repository with the answer. It is one boolean, it is easy, and it is what a per-repo `type_checked` column would encourage. It is also exactly what the spec forbids, and the failure is invisible in any fixture with one package in it.

**What this plan does instead:** the edge *set* is produced once, by the AST, and type information only ever **upgrades individual rows**. Every call site is a row before the type-checker runs; `types.Info.Uses` then resolves as many of them as it can, one at a time, and each one it resolves gets a `to_symbol_id` and the label `resolved`. Everything else keeps its null target and the label `syntactic`.

Three properties fall out, and all three are tests rather than claims:

- **The number of edges is the same whether type-checking ran or not.** Only `provenance` and `to_symbol_id` differ. Task 3 pins it by running the same fixture twice.
- **Resolution is per call site, so one package can carry both labels.** A package that type-checks with errors still resolves most of its identifiers; the ones `Uses` has no object for stay syntactic *inside a package that loaded*. Task 3's fixture has exactly that shape.
- **A repository with two packages, one loadable and one not, produces both labels.** Task 3's fixture is two packages, and the assertion is on the label of a **named edge in each**, not on a count — a count assertion passes under a mutant that swaps which package got which label.

### 2. One deadline for the whole job.

Spec:194 — "it runs inside the same sandbox and draws on the *same per-job wall-clock budget* as the clone — one deadline for the whole job, not a fresh one per stage, so a slow clone cannot buy itself extra time by failing into the type-check."

So the type-check stage takes the job's `context.Context` and does not derive a new timeout from it. Not `context.WithTimeout(ctx, typecheckBudget)` — that is a *smaller* deadline, which is defensible and still wrong, because it is a second budget and the spec asked for one. Not `context.WithTimeout(context.Background(), …)`, which is the failure the sentence was written against.

The fixture that separates them is the one where the job's budget is **already nearly spent**: the original resolves nothing and the repo's edges are all syntactic; a mutant with its own fresh budget resolves them. A fixture with a generous deadline cannot tell the two apart, which is why Task 3 ships an expired-context test and not a slow one.

### 3. The type-check's failure is an outcome, not an error.

Spec:196 — "Its failure is expected rather than exceptional, and downgrades the repo's edges to `syntactic` instead of failing the job."

So there is no error return from the stage that reaches the job's failure path. The stage returns what it managed to resolve plus a *reason* it did not resolve more, the indexer logs it, a counter counts it by reason, and the job goes `done`. The one thing that may still fail the job is the database write, which is not the type-checker failing.

This is also the reason the module-fetch policy in Task 3 can be closed by default: with `GOPROXY=off`, a repository with third-party imports simply does not type-check, and "does not type-check" is a documented, counted, per-package outcome rather than a broken build.

### 4. A syntactic edge has a null target, and nothing may bind it later.

Spec:84 — "**A syntactic edge has a null target.** It knows it calls something named `Close` and cannot say which one. Recording a null rather than a guess is what makes the `provenance` label mean anything."

The subtle way to break this is not in the writer but in the **reader**: a `callers_of` query that joins syntactic edges to symbols by name has re-introduced the guess, at query time, where no column records it. A corpus with one `Close` in it makes that join look correct forever.

**What this plan does:** `CallersOf` traverses `to_symbol_id` only. Name-matched edges are returned in a **separate, labelled set** (`approximate`), at depth 1 only, never traversed, never merged, and the response says how many there are so a caller can see the size of what is not known. Task 5's fixture has two distinct `Store.Get`-shaped methods so that a merged implementation returns a caller of the *other* one and fails.

---

## Global Constraints

- Go **1.27**; module `github.com/mralaminahamed/codetrail`.
- Default branch is **`trunk`**. Branch from it, one PR per task, **merge commits, never squash**. Never `--no-verify`.
- Commit author and committer must be `Al Amin Ahamed <alamin.ahamed.dev@gmail.com>`. The pre-push hook enforces it; do not bypass it.
- Comments are **minimal and short** — explain *why*, never restate *what*.
- Line numbers are **1-based and inclusive** (spec §3). A symbol's own `start_line`/`end_line` follow the same convention as a span's, and Task 1 asserts the two agree for a declaration that produced both.
- `gofmt -l apps packages` empty and `go vet ./...` clean before every commit.
- **Migrations:** `NNNN_name.sql`, filename-ordered, safe to run twice **and against a populated database**. `migrate()` runs the whole ledger in one transaction holding `pg_advisory_xact_lock`, so `CREATE INDEX CONCURRENTLY` is illegal in a migration file and every statement must be `IF NOT EXISTS`-shaped. Task 2 adds `0010`; if another branch lands `0010` first, renumber before merge — two files with the same number is a silent skip, because the ledger is keyed on the name.
- **Live suites create and drop their own database through `packages/shared/testdb`.** Never connect a suite to `DATABASE_URL` directly; P2's review round found two suites that emptied the developer's database while printing `ok`. The CI guard that refuses to let a skipped tagged suite print `ok` applies to every suite this phase adds.
- **Nothing in this phase may foreclose the eval's two arms living in separate databases.** `RepoID = hash(key, commit)` and `SpanID = hash(repo, path, start, end, digest)` have no room for a strategy, so the AST arm and the window arm are two corpora in two databases. Every query in this phase is scoped to one repo id in one store, and nothing joins across arms or infers a strategy from a row.
- **No new egress from the gateway.** All of this phase's outbound risk is in the indexer, where P1 put it.

---

## How to mutate, and what counts as a kill

P1–P3 ran roughly 350 mutations. The audit across them found **~30 predicted failure messages wrong**, **four false rationales** (the plan's stated reason for the code was wrong, so a mutation derived from it proved nothing about anything), several void mutations, and — the finding that matters most here — that **per-task mutation rounds are not enough**. P3's eight tasks each closed with "no survivors"; the whole-branch sweep run afterwards found about twenty, and almost every one was one of two shapes:

- **(a) a side effect nothing reads back** — a counter incremented, a field set, a log line emitted, a column written, that no assertion anywhere observes; and
- **(b) an error branch behind a fake nobody wired** — a fake store or a fake loader whose error path is never set by any test, so deleting the handler's response to it changes nothing.

Both are structural, not accidental, and both are cheap to prevent if the plan enumerates them up front. Task 7 carries the two ledgers: **every side effect this phase creates, with the test that reads it back**, and **every fake, with the test that sets its error**. A row in either ledger with no test named against it is an unfinished task, not a nice-to-have.

The rules, unchanged from P3 except where noted:

1. **Commit before you mutate.** `git status --porcelain` must be empty before the first `sed`; `git checkout -- <file>` restores from `HEAD`, and a mutation reverted against a dirty tree restores the mutant.
2. **The mutant must compile and vet.** `go build ./... && go vet ./...` after applying it. Void kills, all of which have already happened at least once in this project: removing an import's only use is a build break; a mutant `go vet` flags as unreachable code is void; one that panics or `t.Fatal`s before the assertion is an incident, not a kill; and **a SQL mutation that produces a pgx protocol error rather than different rows is void** — unbind a parameter and you have tested pgx. Mutate `WHERE repo_id = $1` to `WHERE repo_id = $1 OR TRUE`, never by deletion.
3. **Predicted output is an expectation, not a fact.** Every block says what the failure is expected to look like. Run it, **paste the observed output into the plan**, correct the prediction. A row still reading as a prediction when the task is done is an unfinished task.
4. **Name the fixture that separates mutant from original, and say why it can.** See the graph-specific version below, which is where this phase would otherwise waste its whole budget.
5. **State the rationale for the code you are mutating.** If the stated reason is wrong the mutation tests nothing. Four false rationales have shipped in this project already.
6. **If you cannot describe the failing output, the test is not evidence.** "It would hang" or "it would be flaky" means redesign until the failure is a comparison against a value.
7. **A kill is an assertion failing.** A `t.Fatal(err)` on the way to the assertion, a panic, or a timeout is an incident. Fix the test so its own claim is what fails, then re-run.
8. **Assert the discriminating property, not a proxy for it.** New in P4, and the reason is in the next section: assert the **rank**, not that the right thing came first; assert **which** reason, not that a refusal happened; assert the **whole set** of metric series that moved, not the one you expected to move.
9. **A comment claiming safety is the highest-risk line in the file.** P2 corrected twelve false comments, two of them introduced by commits fixing others. Run the counterfactual, paste the output into the commit message, then write the comment. If the counterfactual passes, the claim is false — delete it.

Format, unchanged from P3:

```
**M<n> — <one-line description of the edit>.**
- *Why the code exists:* <the rationale a reviewer can disagree with>
- *Fixture that separates mutant from original:* <which fixture, and why it can>
- *Must fail:* <test name>
- *Expected (verify and correct):* <message>
- *Compiles and vets:* <why the mutant is a behaviour change, not a build break>
```

---

## Fixtures that can tell a graph bug from a passing test

Six rules. The first four are this phase's traps, each of which invalidates a fixture that would otherwise look thorough; the last two are P3's, restated because they apply unchanged.

1. **A fixture where every edge resolves cannot detect a provenance bug.** If the type-checker resolves everything, `provenance` is a constant, and a mutant that hard-wires `resolved` passes every assertion. Every graph fixture in this phase contains **at least one call that cannot resolve** — a method call on a value whose type comes from a package that did not load, or a call through an interface variable whose dynamic target is not a single object.
2. **A single-package fixture cannot detect per-edge versus per-repo labelling.** With one package, "the package loaded" and "this edge resolved" are the same sentence. Every fixture that asserts anything about provenance has **two packages, one of which cannot load**, and asserts the label of a **named edge in each** — never a count, since a count survives a mutant that swaps them.
3. **A call graph with no cycles cannot detect a runaway recursive CTE.** A CTE with no cycle guard terminates on a DAG and returns the right answer. Every `CallersOf` fixture contains **a direct self-call (`f` calls `f`), a two-node cycle (`a` calls `b` calls `a`), and a diamond** (two paths to the same node, which is what turns a missing `DISTINCT` into duplicate rows rather than an infinite loop). The cycle test asserts the query *returns*, and asserts the row set, and asserts the reported depth — a test that only asserts termination passes under a CTE that returns one row.
4. **A fixture whose symbol names are unique cannot detect a syntactic edge binding to the wrong one.** Name matching is only visibly wrong when two definitions share a name. Every fixture that exercises the approximate set contains **two `Get` methods on two types in two packages**, so a reader that binds by name returns both and a reader that binds by target returns one, and the assertion names *which*.
5. **Every graph fixture holds two repositories** (P3 rule 3, unchanged). A missing `WHERE repo_id = $1` is otherwise invisible, and the second repo's rows must be **strictly better** candidates than the target repo's — for the graph that means the other repo holds a symbol with the *same name* and *more* callers, so a missing filter changes the answer rather than adding a row nobody looks at.
6. **The expected order must disagree with insertion order, path order and id order** (P3 rule 2, corrected there twice). P3 measured that an unordered result comes back in *path* order, served from `spans_path_idx`, and that a two-row result came back in the same order whichever way it was inserted. So the disagreement a kill depends on is **asserted in the test body**, not constructed and trusted.

One more, from the shape of this phase rather than from a rule: **a fixture that needs the network cannot run in CI.** Every Go fixture module in this phase imports the standard library only, so it type-checks under `GOPROXY=off` with an empty module cache. The one fixture that deliberately imports a module that is not there exists to prove the *failure* path, and it must fail by "module not in cache", never by a DNS timeout.

---

## What P4 carries forward

Each of these is a constraint on the design, not context.

- **`chunk.classify` spells a method `Store.Get`.** The graph must spell it the same way or a symbol has two names in one system. Task 1 does not re-derive it: the naming moves into one exported function that `chunk` and `symbols` both call, and the live test in Task 7 asserts that every span carrying a non-empty `symbol` has a `symbols` row with an equal `name`. Anything less is two implementations agreeing today.
- **`to_tsvector('simple','Store.Get')` is the single token `store.get`**, which is why P3's generated column is `replace(symbol, '.', ' ')`. The graph inherits the consequence: a caller searching lexically finds `Store.Get` through its parts, so `definition_of` must accept the *whole* name exactly, and offer last-segment matching as an explicit, labelled opt-in rather than a silent fallback. Task 5.
- **`doc.go` and `tools.go` produce zero AST spans** — `f.Doc` is not a `Decl`. **Definitions have the same hole and one more.** Zero declarations means zero definitions, which is consistent and harmless. The one that is not harmless: P2 sub-windows a declaration longer than `MaxDeclLines` into `kind=file` spans, so a large function has **no span whose range is the declaration's**. A `symbols.span_id` derived by exact range match would therefore be null for exactly the biggest functions in a repository — the ones most likely to be asked about. Task 2 resolves it: symbols carry their own `start_line`/`end_line` from the AST, and `span_id` links to the span **containing** the declaration's first line. Open question 3 records the deviation from §3's column list.
- **`RepoID = hash(key, commit)`.** Re-indexing at a new commit is a *new repo row*, so the old commit's symbols and edges are untouched and are removed by LRU eviction with everything else. Within one repo id, a retried job must converge: `PutGraph` replaces a repo's graph wholesale in one transaction, and ids are deterministic, so two runs write byte-identical rows. Open question 4.
- **Eviction is one `DELETE` that cascades** (§3). `symbols` and `edges` must cascade from `repos` and — for `symbols.span_id` — degrade rather than block when a span row goes. Task 2.
- **The indexer has no `/metrics` endpoint** (P3's finding, unchanged). This phase's counters are indexer-side counters with nowhere to be scraped from. They are still written, because the alternative is a stage with no instrumentation at all, and the gap is recorded again rather than quietly fixed with an unscrapable exporter.
- **Spec §10 keeps a refusal distinct from an error.** The graph endpoints have no refusal — a symbol that is not there is a `404`, not "we had nothing to say" — and so this phase must not increment the answer-outcome counters at all. Task 6's M8 checks that over the whole series set of both counters.
- **P1's sandbox is deliberately hostile** and this phase runs a compiler inside it. That is Task 3, and it is the sharpest new risk in the project since P1's clone.

---

## Task independence

- **Tasks 1, 2 and 5 are independent** and can be worked in parallel from `trunk`.
  - Task 1 creates `packages/shared/symbols` (AST only, no database, no `go/packages`) and moves one function in `chunk`.
  - Task 2 creates the migration, the models and `store.PutGraph`.
  - Task 5 creates `packages/shared/store/graph.go` — the reads. It depends on Task 2's *schema* but not on its writer; work it against the migration once Task 2's migration file lands, or write the migration in whichever task lands first and rebase the other onto it.
- **Task 3** (the sandboxed type-check) depends on Task 1 for the call-site keys.
- **Task 4** (indexer wiring) depends on 1, 2 and 3.
- **Task 6** (gateway endpoints) depends on 5.
- **Task 7** (end to end, the sweep, the README) depends on everything.

Land Task 1 first if more than one is ready: it is the only task that touches `chunk`, and the shared naming function is a dependency of both the extractor and the live identity assertion in Task 7.

---

### Task 1: `packages/shared/symbols` — definitions and call sites from the AST

`packages/shared/symbols` exists as an empty directory, created at P0 because spec §2 names it. This task is its first file. Everything here is `go/parser` over bytes: no database, no `go/packages`, no network, no clock.

**Files:**
- Create: `packages/shared/symbols/symbols.go`, `packages/shared/symbols/parse.go`, `packages/shared/symbols/testdata/{calls,twoget,long,docs}.gotxt`
- Modify: `packages/shared/chunk/ast.go` — `classify` becomes one exported function that also reports the line range
- Test: `packages/shared/symbols/parse_test.go`, `packages/shared/chunk/ast_test.go` (the existing tests follow the rename)

**Interfaces:**
- Consumes: `go/ast`, `go/parser`, `go/token`, `packages/shared/models`, `packages/shared/chunk`.
- Produces in `chunk`:
  - `func Decl(fset *token.FileSet, d ast.Decl) (kind models.SpanKind, symbol string, start, end int, ok bool)` — `classify` plus the range `astChunks` already computes. `astChunks` calls it; `classify`, `docOf` and the inline `line()` calls it replaces are deleted rather than left beside it.
- Produces in `symbols`:
  - `type Def struct { Kind models.SpanKind; Name, Pkg, Path string; StartLine, EndLine int }`
  - `type Call struct { Path, Name string; Offset, Line, FromStart int }`
  - `type File struct { Pkg string; Defs []Def; Calls []Call; Unnameable int }`
  - `func Parse(path string, src []byte) (File, error)`

**Decisions, with their reasoning:**

- **The symbol's spelling is not re-derived; it moves.** `chunk.classify` writes a method as `Store.Get` and that string is already in `spans.symbol`, in P3's lexical `tsvector` (through `replace(symbol, '.', ' ')`) and in every citation a user has seen. A second derivation in `symbols` would agree on the day it was written and drift on the day someone adds generic receivers. So `classify` becomes `chunk.Decl` and both callers use it. **The cheap alternative — copy the switch — is what this project has already been bitten by twice** (`kind=file`'s meaning, and `models.go`'s comment about it), both times because two places stated the same fact independently.
- **`Decl` returns the range too, not only the name.** A span's range starts at the doc comment (`astChunks` does `if doc := docOf(d); doc != nil { start = line(fset, doc.Pos()) }`), and a definition that started at the `func` keyword would differ from its own span by however many lines the doc comment is. One function returns one range and the two cannot disagree.
- **A definition exists whether or not a span does.** `MaxDeclLines` sub-windows a long declaration into `kind=file` spans, so the biggest functions in a repository have no span of their own — and they are the ones most worth asking "who calls this" about. Definitions are the AST's, not the chunker's, so a 3,000-line function is one `Def` and several spans. Task 2 links them by containment.

  > **Measured, and it refines Task 2's claim.** The hole opens at `WindowLines`, not at `MaxDeclLines`. A declaration over `MaxDeclLines` but shorter than one window is sub-windowed into exactly **one** `kind=file` span whose range *equals* the declaration's — measured on `symbols/testdata/long.gotxt` (a 27-line declaration, `MaxDeclLines=10`, the shipped `WindowLines=40`: one span, 3..29, identical to the `Def`). So "an exact-range match would be null for the largest declarations" is true only for declarations longer than a window; between the two thresholds an exact match still finds a span, and it finds one that is a window rather than the declaration. Containment is right either way, and this is the reason the fixture scales the window down (`MaxDeclLines=10`, `WindowLines=10`, `WindowOverlap=2` → 3..12, 11..20, 19..28, 27..29) rather than trusting the defaults to produce the hole.
- **`Pkg` is the package clause name (`f.Name.Name`), not the import path.** The import path is not knowable without a module-aware load, and this pass must work when that load fails — which §6 says is expected. The file's directory is already in `Path`, so a column holding the directory would be a copy. Open question 5 records what is lost: two packages named `store` in one repository are distinguishable only by path.
- **A call's `Name` is the last identifier of the callee**, which is exactly what spec:84 says a syntactic edge knows: "it calls something named `Close`". `pkg.Fn` → `Fn`; `s.Get` → `Get`; `x.y.Z` → `Z`; `Close` → `Close`. Nothing here can tell a package qualifier from a variable — that is what the type-checker is for — so recording `s.Get` as a name would record a receiver *expression* as though it were a type.
- **A callee that is not an identifier or a selector produces no edge and is counted.** `f()()`, `fns[i]()`, `func(){}()` — there is no name to record, and an edge with an empty `to_name` would violate the `NOT NULL` §3 gives it and would mean "calls something called nothing". `File.Unnameable` carries the count so the indexer can log it beside `vanished`, `unstrippable` and `tokenless`, which is the existing shape for a per-job soft loss.
- **Calls are keyed by byte offset, not by line.** `a(b(), b())` is two calls on one line, and a line key collapses them into one edge — and then into *one row*, because Task 2's `EdgeID` is a hash of the key. Offsets also survive `chunk.StripDocs`, which blanks doc bytes in place and preserves length; Task 3 depends on that, and this task's test is where it is pinned.
- **A call belongs to the top-level declaration that encloses it**, found by iterating `f.Decls` and `ast.Inspect`ing each. A call inside a func literal inside `var handler = func() { g() }` belongs to the `var` declaration, because that is the only definition there is. There is no code outside a declaration in Go, so no call is orphaned.
- **`Parse` returns an error only when the file does not parse**, in which case there are no definitions and no calls for it — matching `chunk`, which falls back to windows. The caller counts it; it is not a job failure.

- [x] **Step 1: Write the failing tests**

Fixtures are `.gotxt` under `testdata/`, following `chunk`'s convention (a `.go` file under `testdata` would be compiled by the toolchain and would have to be valid for the whole module).

```go
// The one test that would have caught the drift this task exists to prevent:
// it asserts against chunk's output, not against a literal.
func TestEveryDeclarationsSymbolIsSpelledAsItsChunkSpellsIt(t *testing.T)

func TestAMethodIsNamedByItsReceiverBase(t *testing.T)        // Store.Get, *Store, Store[K,V]
func TestADefinitionsRangeIncludesItsDocComment(t *testing.T)  // equal to the chunk's range
func TestADeclarationTooLongForASpanStillHasADefinition(t *testing.T)
func TestAFileWithNoDeclarationsHasNoDefinitions(t *testing.T) // doc.go and tools.go shapes
func TestImportsAreNotDefinitions(t *testing.T)

func TestTwoCallsOnOneLineAreTwoCalls(t *testing.T)            // a(b(), b())
func TestTheCalleeNameIsItsLastSegment(t *testing.T)           // table: pkg.Fn, s.Get, x.y.Z, Close
func TestACalleeWithNoNameIsCountedRatherThanGuessed(t *testing.T)
func TestACallInsideAFuncLiteralBelongsToTheEnclosingDeclaration(t *testing.T)
func TestOffsetsAndRangesSurviveDocCommentStripping(t *testing.T)
```

**`TestOffsetsAndRangesSurviveDocCommentStripping` is the load-bearing one and it is not obvious.** `StripDocs` blanks doc bytes and keeps every `\n`, so byte offsets and line numbers are expected to be identical between stripped and unstripped source — but the *ranges* are not, because a blanked doc comment is no longer a comment and `Decl` therefore starts the definition at the `func` keyword. So the test asserts **offsets equal, ranges not necessarily equal**, and records the measured difference. If it turns out offsets move, Task 3's whole keying scheme is wrong and must be found here rather than in a resolution rate nobody can explain.

> **MEASURED — the premise above is false, and this is the finding of Task 1.** `StripDocs` does *not* blank in place: it **removes the prose bytes and keeps only their line terminators** (`strip.go`'s copy loop skips every dropped byte that is not `\n` or `\r`). Line numbers are invariant; **byte offsets are not**. On `symbols/testdata/docs.gotxt`: the call to `len` is at offset 194 raw and 69 stripped, `errors.New` at 226 and 101, and the file goes from 265 bytes to 140 — every offset below a doc comment moves by the prose removed above it. Ranges move too (`Parse` is 5..14 raw, 8..14 stripped), as predicted.
>
> Three consequences. (a) The test is named for what it measures: `TestLineNumbersSurviveDocCommentStrippingAndOffsetsDoNot`, and it pins the offsets as values so the day stripping preserves length it says so. (b) **Task 3's "No `packages.Config.ParseFile` hook, and the reason is a measurement" is a false rationale** — the two streams do *not* share a coordinate system — and the comment it asks for would have been a false comment. (c) **Task 4 must hand the graph pass the indexer's raw `body`, never its stripped `src`** (`apps/indexer/cmd/main.go:486`), or with `STRIP_DOC_COMMENTS=true` every offset key misses and the resolution rate collapses to zero with no error anywhere. Open question 12's recommendation is wrong as written and is answered by "parse the same bytes", not by "they are the same bytes".

- [x] **Step 2: Implement**

`chunk.Decl` is `classify` with the range folded in:

```go
// Decl reports what a declaration becomes: its kind, its symbol, and its
// 1-based inclusive line range including any doc comment. The symbol graph
// needs the same three, and two derivations of a name are two names.
func Decl(fset *token.FileSet, d ast.Decl) (models.SpanKind, string, int, int, bool)
```

`symbols.Parse` parses with `parser.ParseComments|parser.SkipObjectResolution` — the same flags `chunk` uses, and `SkipObjectResolution` matters here: `ast.Object` resolution is the thing that would tempt a reader into believing this pass can bind names. It cannot. That is Task 3's job.

- [x] **Step 3: Run**

`go test ./packages/shared/symbols/ ./packages/shared/chunk/ -count=1 -v`

- [x] **Step 4: Commit, then prove the tests discriminate**

**M1 — `chunk.Decl` drops the receiver, naming a method `Get`.**
- *Why the code exists:* `spans.symbol`, the lexical index and every citation already spell a method `Store.Get`; the graph must use the same string or one symbol has two names in one system.
- *Fixture that separates mutant from original:* `twoget.gotxt`, which declares `Store.Get` and `Cache.Get`. **A fixture with one `Get` cannot:** the assertion would still find "a definition named Get" and the collision that makes the receiver load-bearing would not exist.
- *Must fail:* `TestAMethodIsNamedByItsReceiverBase`, and the existing chunk tests.
- *Observed — killed, in both packages:* `parse_test.go:110: definition 1 is func/"Get" twoget sample.go 7..8, want func/"Store.Get" twoget sample.go 7..8`, plus `ast_test.go:49: chunk 3 is func/Add, want func/Counter.Add`, `ast_test.go:160: chunk 0 is "Push", want "Stack.Push"`, and three more in `chunk`. One edit, five failures in two packages: that is what the move buys.
- **Plan defect — the "must fail" claim above was backwards.** `TestEveryDeclarationsSymbolIsSpelledAsItsChunkSpellsIt` **passes** under M1 applied to `Decl`, because both packages read the same function and move together. It fails only when a *second* derivation exists — verified by applying M8's copy and then M1 to `Decl` alone: `definition 1 is func/"Get" 7..8, its span is func/"Store.Get" 7..8`. The cross-package test detects **drift between two derivations**, which is precisely the failure a copy would cause and the reason M8 is worth refusing.
- *Compiles and vets:* yes — `base` is still read by the `if` guard, so nothing is left unused. (The build-break warning belongs to M2, not here: see below.)

**M2 — the definition's range starts at the declaration rather than at its doc comment.**
- *Why the code exists:* a span starts at the doc comment, so a definition that did not would differ from its own span and `span_id` linkage would go from exact to approximate for every documented declaration in the corpus.
- *Fixture that separates mutant from original:* `docs.gotxt`, a declaration with a three-line doc comment. **A fixture of undocumented declarations cannot** — and an undocumented corpus is not hypothetical, it is what `STRIP_DOC_COMMENTS=true` produces.
- *Must fail:* `TestADefinitionsRangeIncludesItsDocComment`
- *Observed — killed:* `parse_test.go:136: Parse starts at line 8, which holds "func Parse(src []byte) error {", want its doc comment`, plus six more in `symbols` and two in `chunk` (`ast_test.go:82: ErrGone starts at line 9 …`).
- **Plan defect — the predicted message could not happen.** `definition Parse spans 8..14, its chunk spans 5..14` assumes the definition and the chunk can disagree; under one shared `Decl` they move together, so the def-vs-chunk comparison **passes** under M2. What kills it is the assertion anchored on the fixture's own line text. Both assertions are kept: the comparison catches a copy, the anchor catches a shared wrong answer.
- **Plan defect — M2 is the mutation that breaks the build, not M1.** Deleting the `if doc != nil` block leaves `doc` assigned and never read: `packages/shared/chunk/ast.go:70:6: declared and not used: doc`. Spelled `_ = doc`, it compiles, vets and kills.
- *Compiles and vets:* yes, as `_ = doc`.

**M3 — calls keyed by line instead of by byte offset.**
- *Why the code exists:* two calls on one line are two edges, and Task 2 hashes this key into the edge id, so a line key silently merges them.
- *Fixture that separates mutant from original:* `calls.gotxt` containing `a(b(), b())`. **A fixture with one call per line cannot**, and one call per line is what every hand-written fixture looks like unless it is written for this.
- *Must fail:* `TestTwoCallsOnOneLineAreTwoCalls`
- *Observed — killed:* `parse_test.go:245: two calls to b on line 12 share the key 12, so they are one edge` (and the stripping test, which pins the offsets as values). **The predicted alternative was the real one:** the mutant yields *two* calls with *equal keys*, so an assertion of `len(calls) == 2` would have passed. The assertion is on the keys being distinct, not on the count.
- *Compiles and vets:* yes.

**M4 — the callee name keeps the whole selector expression (`s.Get` rather than `Get`).**
- *Why the code exists:* spec:84 — a syntactic edge "knows it calls something named `Close`". `s` is a variable, not a type, and this pass cannot tell the difference; storing `s.Get` records a receiver expression as though it were a qualified name, and no `to_name` would ever match a definition.
- *Fixture that separates mutant from original:* the `TestTheCalleeNameIsItsLastSegment` table, whose rows are `pkg.Fn`, `s.Get`, `x.y.Z`, `Close`. The `x.y.Z` row is the one that separates "last segment" from "the two last segments".
- *Must fail:* `TestTheCalleeNameIsItsLastSegment`
- *Observed — killed:* `parse_test.go:266: callee 0 named "pkg.Fn", want "Fn"` — index 0, because the mutant catches every selector, not only the method call.
- *Also run, M4b — only nested selectors keep two segments:* `parse_test.go:266: callee 2 named "y.Z", want "Z"`. This is the run that proves the `x.y.Z` row earns its place: M4b is invisible to the other three rows.
- *Compiles and vets:* yes.

**M5 — an unnameable callee is recorded with an empty name instead of counted.**
- *Why the code exists:* `edges.to_name` is `NOT NULL` in §3, and an edge naming nothing is not a weaker claim than a syntactic edge, it is a meaningless one.
- *Fixture that separates mutant from original:* `calls.gotxt`'s `fns[i]()` and `f()()`. Every other fixture in this package has a nameable callee.
- *Must fail:* `TestACalleeWithNoNameIsCountedRatherThanGuessed`
- *Observed — killed:* `parse_test.go:280: got 9 calls and 0 unnameable, want 6 and 3`. The fixture holds three unnameable callees (`fns[0]()`, `f()()`, `func(){ b() }()`) and six nameable ones; the inner calls of the last two are themselves nameable, which is why the counts are 6/3 rather than the predicted 3/2.
- *Compiles and vets:* yes.

**M6 — calls are collected file-wide rather than per declaration, with `FromStart` set to the first declaration.**
- *Why the code exists:* an edge's tail must be the definition the call is *in*, or "who calls this" names the wrong function — which is a wrong answer that looks exactly like a right one.
- *Fixture that separates mutant from original:* `calls.gotxt` has three declarations and puts the call in the **last**. A fixture with one declaration cannot see this at all, and a fixture whose call is in the first declaration passes under the mutant by coincidence.
- *Must fail:* `TestACallInsideAFuncLiteralBelongsToTheEnclosingDeclaration`
- *Observed — killed:* `parse_test.go:317: call to g is in the declaration starting at line 3, want 22` — 22 rather than 21 is where the fixture's `var handler` doc comment starts; the shape is exactly as predicted.
- *Compiles and vets:* yes.

**M7 — `Parse` reports a parse error as a fatal error rather than an empty file.**
- *Why the code exists:* an unparseable file is a normal thing in a stranger's repository and `chunk` already windows it rather than failing; a graph pass that fails the job on one bad file is stricter than the chunker for no reason.
- *Fixture that separates mutant from original:* a fixture that does not parse (reuse `chunk/testdata/broken.gotxt`).
- *Must fail:* the parse-error subtest of `TestAFileWithNoDeclarationsHasNoDefinitions` — added; it reads `chunk/testdata/broken.gotxt`, so both packages agree on what "does not parse" means.
- *Observed — killed:* `parse_test.go:205: Parse returned 10 definitions, 0 calls, pkg "broken" and 0 unnameable with error "symbols: parse sample.go: sample.go:6:1: expected operand, found '}' (and 2 more errors)", want an empty File`. The mutation that has teeth is not "error instead of empty" (the original returns both) but **returning the partial AST go/parser recovers**: ten definitions whose ranges are the parser's guesses about code that is not there. `f` is never nil when the source is in memory, so the mutant asserts rather than panicking.
- *Compiles and vets:* yes.

**M8 — `astChunks` keeps its own copy of the naming switch instead of calling `Decl`.**
- *Why the code exists:* this is the whole point of the task, and it is the mutation that tests the *refactor* rather than the code.
- *Fixture that separates mutant from original:* **none, and that is the finding to record.** Re-introducing the duplicate leaves every test in both packages passing, because the copy is correct on the day it is made. Record this as a **known survivor** with its reason: what this refactor buys is not a behaviour today, it is that M1 applied in one place fails tests in two packages.
- *Must fail:* nothing. Recorded as a survivor.
- *Observed — survivor, as predicted:* with `classify` restored beside `Decl` and `astChunks` calling the copy, `go test ./packages/shared/symbols/ ./packages/shared/chunk/` is `ok` and `ok`. Then applying **M1 to `Decl` alone**, with the copy in place, fails `TestEveryDeclarationsSymbolIsSpelledAsItsChunkSpellsIt/twoget.gotxt`: `definition 1 is func/"Get" 7..8, its span is func/"Store.Get" 7..8`. So the survivor is real and the cross-package test is what would catch the drift the copy makes possible.
- **Plan defect — the instrument as written does not work.** `grep -c 'recvBase' packages/shared/chunk/*.go` counts *lines in one file* and returns 5, not 1, because `recvBase` is defined there and is self-recursive. The instrument that works is `grep -rln recvBase packages apps`, which must return exactly `packages/shared/chunk/ast.go`.
- *Compiles and vets:* yes.

- [x] **Step 5: Commit**

```bash
gofmt -l apps packages && go vet ./... && go test ./... -count=1
git add packages/shared/symbols packages/shared/chunk
git commit -m "feat(symbols): definitions and call sites from the AST

The naming switch moves out of chunk rather than being copied. A method
is Store.Get in spans.symbol, in the lexical tsvector through
replace(symbol,'.',' ') and in every citation already; a second
derivation would agree today and drift the day generic receivers change.
One function, two callers, and a mutation applied to it fails tests in
both packages.

Calls are keyed by byte offset because a(b(), b()) is two calls, and the
edge id is a hash of that key: a line key would merge them into one row.
A callee that is neither an identifier nor a selector is counted, not
named — edges.to_name is NOT NULL and an edge naming nothing is not a
weaker claim, it is an empty one."
```

**Definition of Done** — all met; see the notes above for the two deviations (the stripping premise and the M1/M2 predictions).
- One derivation of a symbol's name in the repository, called from both `chunk` and `symbols`, with a test that compares the two packages' output rather than each against a literal.
- A definition exists for a declaration too long to have a span of its own.
- `doc.go` and `tools.go` shapes yield zero definitions, matching their zero spans.
- Two calls on one line are two calls; a callee with no name is counted, not guessed.
- ~~Byte offsets are pinned as invariant under `StripDocs`~~ — **measured false.** Line numbers are invariant, byte offsets are not, and the measurement is pinned as values with the consequence for Tasks 3 and 4 recorded above.
- M1–M8 recorded with observed output, M8 recorded as a survivor with its reason.

---

### Task 2: The schema, the models, and `PutGraph`

**Files:**
- Create: `packages/shared/store/migrations/0010_symbol_graph.sql`, `packages/shared/store/graph.go`
- Modify: `packages/shared/models/models.go`
- Test: `packages/shared/store/graph_live_test.go` (`//go:build live`)

**Interfaces:**
- Produces in `models`: `Provenance` (`ProvenanceResolved`, `ProvenanceSyntactic`), `EdgeKind` (`EdgeCalls`, `EdgeImports`, `EdgeReferences`), `Symbol`, `Edge`.
- Produces in `store`: `func SymbolID(repoID, path string, start int, kind, name string) string`, `func EdgeID(repoID, fromSymbolID, path string, offset int, toName string) string`, `func (s *Store) PutGraph(ctx context.Context, repoID string, syms []models.Symbol, edges []models.Edge) error`.

**Decisions, with their reasoning:**

- **The schema enforces §3's invariant rather than trusting the writer.** `CHECK ((to_symbol_id IS NOT NULL) = (provenance = 'resolved'))`. Spec:84 says a syntactic edge has a null target and spec:71 says a resolved one points at a symbol row; if those two facts live only in Go, then a bug, a future writer, or a hand-run `UPDATE` can produce a row that claims precision it does not have — and nothing downstream can tell. The constraint is the only place this is true by construction.
- **`PutGraph` validates the same pair in Go anyway**, before the transaction, so the failure names the offending edge instead of surfacing as `edges_provenance_target` on row 4,000 of a batch. The constraint is the guarantee; the Go check is the error message.
- **Whole-repo replacement in one transaction**, exactly as `PutSpans` does it: `DELETE FROM edges WHERE repo_id = $1`, then `DELETE FROM symbols WHERE repo_id = $1`, then batched `INSERT … ON CONFLICT (id) DO UPDATE`. Two reasons for the explicit edge delete even though `symbols`' cascade would take them: it states the order rather than depending on a cascade to imply it, and it means a re-index that produces *fewer* edges cannot leave the extras behind.
- **`ON CONFLICT (id) DO UPDATE`, not `DO NOTHING`.** The ids are deterministic, so a re-index of the same commit writes the same ids — and `DO NOTHING` would then silently keep the *old* row's provenance. A repository indexed once with the type-checker unavailable and again with it working would keep every edge syntactic, and nothing would look wrong.
- **`symbols.span_id` is nullable and links by containment**, not by exact range. A declaration longer than `MaxDeclLines` has no span with its range, so an exact match would be null for exactly the largest declarations. Containment — the span whose `[start_line, end_line]` contains the definition's first line, smallest first — gives the biggest functions a citable span, and the linkage is computed in the indexer from the spans it just wrote rather than by a query.
- **`ON DELETE SET NULL` for `span_id`, `ON DELETE CASCADE` for everything else.** `PutSpans` deletes a repo's spans on every re-index; with a cascade, that would delete the repo's symbols as a side effect of re-chunking, and the graph would vanish between two writes with no error. A definition came from the AST, not from the chunker, so losing its span should cost it its link and nothing else.
- **`to_symbol_id` cascades rather than setting null**, because setting it null would leave `provenance = 'resolved'` beside a null target — a row the CHECK constraint forbids, so the delete would fail instead. Cascade is the only option consistent with the invariant.
- **`edges` carries `path` and `line`.** §3's column list does not, and without them "who calls this" answers with a symbol and no location, in a product whose one-sentence description is "get an answer that cites `file:line`" (spec:5). Recorded as a deviation in Open question 6, not slipped in.
- **`symbols` carries `start_line`/`end_line`.** Same reason, same deviation: §3's list would make a definition citable only through its span, and the sub-window hole means the biggest declarations have none.
- **Ids include what makes the row unique and nothing else.** `SymbolID = hash(repo, path, start, kind, name)`: a file has one declaration starting at a given line, and `kind`/`name` are carried so that two declarations sharing a start line (`type A int; type B int` on one line is legal) do not collide. `EdgeID = hash(repo, from, path, offset, to_name)`: the offset is the call site, which is why Task 1 records it.

- [ ] **Step 1: Verify the DDL against the pinned image before writing the migration**

Against `pgvector/pgvector:pg17`, and **paste the output into the commit message**:

```sql
-- the pair constraint must be accepted, and must actually refuse:
CREATE TEMP TABLE e (to_symbol_id TEXT, provenance TEXT,
  CONSTRAINT pair CHECK ((to_symbol_id IS NOT NULL) = (provenance = 'resolved')));
INSERT INTO e VALUES (NULL, 'resolved');   -- expect: violates check constraint
INSERT INTO e VALUES ('x',  'syntactic');  -- expect: violates check constraint
INSERT INTO e VALUES (NULL, 'syntactic');  -- expect: 1 row
INSERT INTO e VALUES ('x',  'resolved');   -- expect: 1 row
-- and the partial index the approximate reader uses:
EXPLAIN SELECT 1 FROM edges WHERE repo_id = 'r' AND to_name = 'Get' AND to_symbol_id IS NULL;
```

Record what the `NULL = NULL` case does: the constraint's left side is `to_symbol_id IS NOT NULL`, which is never NULL, so there is no three-valued-logic hole — **verify that rather than assuming it**, because a CHECK that evaluates to NULL passes.

- [ ] **Step 2: Write the failing live tests**

Every live suite in `store` is `package store` (external test packages cannot reach `s.pool`, and several of these fixtures need direct `INSERT`s that `PutGraph` cannot produce). Follow `store_live_test.go`'s `TestMain` verbatim.

```go
func TestPutGraphWritesSymbolsAndEdges(t *testing.T)
func TestPutGraphReplacesAnEarlierGraphForTheSameRepo(t *testing.T)   // fewer edges the second time
func TestPutGraphIsIdempotentAcrossTwoIdenticalRuns(t *testing.T)     // identical ids, identical rows
func TestPutGraphLeavesAnotherReposGraphAlone(t *testing.T)           // two repos
func TestPutGraphRefusesARowFromAnotherRepo(t *testing.T)
func TestPutGraphNamesTheEdgeWhoseProvenanceAndTargetDisagree(t *testing.T)
func TestTwoCallsOnOneLineAreTwoRows(t *testing.T)                    // the EdgeID key
func TestARerunUpgradesASyntacticEdgeToResolved(t *testing.T)         // the DO NOTHING trap
func TestTheSchemaRefusesAResolvedEdgeWithNoTargetLive(t *testing.T)  // direct INSERT
func TestTheSchemaRefusesASyntacticEdgeWithATargetLive(t *testing.T)  // direct INSERT
func TestEvictingARepoTakesItsGraphWithIt(t *testing.T)               // the cascade Evict's comment promises
func TestReplacingASpanLeavesTheSymbolWithANullLink(t *testing.T)     // ON DELETE SET NULL
```

`TestEvictingARepoTakesItsGraphWithIt` is the one that would otherwise be nobody's job: `Evict`'s doc comment already promises "whatever P3's symbol graph adds by declaring the same reference", and a `REFERENCES repos(id)` written without `ON DELETE CASCADE` fails eviction with a foreign-key violation rather than orphaning — which is a *different* bug and a different message. Assert the row counts, not the absence of an error.

- [ ] **Step 3: Implement**

`0010_symbol_graph.sql` — every statement `IF NOT EXISTS`, no `CONCURRENTLY` (the ledger runs in one transaction under `pg_advisory_xact_lock`), and a header comment recording that the two FK-bearing `CREATE TABLE`s take a `SHARE ROW EXCLUSIVE` lock on `repos`, `files` and `spans` for the length of the migration, which is what "safe on a populated database" costs here.

- [ ] **Step 4: Commit, then prove the tests discriminate**

**M1 — `ON CONFLICT (id) DO UPDATE` → `DO NOTHING` on the edge insert.**
- *Why the code exists:* ids are deterministic, so the second run's rows collide with the first's by design; `DO NOTHING` keeps the older row's `provenance` and `to_symbol_id`.
- *Fixture that separates mutant from original:* `TestARerunUpgradesASyntacticEdgeToResolved` — the same repo written twice, syntactic then resolved, **with the delete-then-insert path defeated in the test by writing through a path that keeps the row**. Verify this: with the wholesale `DELETE` in place, the second insert has nothing to conflict with and the mutant survives. If it does, the honest fix is to record M1 as a survivor *of the delete*, and to note that `DO NOTHING` is only reachable if the delete is ever narrowed — then keep the mutation as a pair with M2.
- *Must fail:* `TestARerunUpgradesASyntacticEdgeToResolved`
- *Expected (verify and correct):* `edge a->Get is syntactic after the second write, want resolved` — **or a survivor; run it before believing either.**
- *Compiles and vets:* yes.

**M2 — the two `DELETE`s dropped (the writer becomes insert-only).**
- *Why the code exists:* re-indexing must converge, and a graph that only grows keeps edges from a call site that has since been deleted.
- *Fixture that separates mutant from original:* `TestPutGraphReplacesAnEarlierGraphForTheSameRepo`, whose second write has **fewer** edges than the first. A second write with the same or more edges cannot see this.
- *Must fail:* `TestPutGraphReplacesAnEarlierGraphForTheSameRepo`
- *Expected (verify and correct):* `repo has 5 edges after the second write, want 3`
- *Compiles and vets:* yes.

**M3 — the deletes lose their repo scope (`WHERE repo_id = $1 OR TRUE`).**
- *Why the code exists:* the corpus holds up to `KEEP_REPOS` repositories and each is written independently.
- *Fixture that separates mutant from original:* `TestPutGraphLeavesAnotherReposGraphAlone`, which writes repo B **first** and asserts B's rows survive a write to A. Written the other way round it proves nothing.
- *Must fail:* `TestPutGraphLeavesAnotherReposGraphAlone`
- *Expected (verify and correct):* `repo B has 0 symbols after writing repo A, want 2`
- *Compiles and vets:* yes, and `$1` stays bound — deleting the clause outright is the void mutation P1 shipped twice.

**M4 — `SymbolID` drops `start` from the hash.**
- *Why the code exists:* a name is not unique in a file — two `init()` functions in one file are legal, and so is the same method name on two types.
- *Fixture that separates mutant from original:* `twoget`-shaped rows: `Store.Get` and `Cache.Get` are different names, so they do **not** separate this; the fixture needs **two `init` functions in one file**, or the same name at two start lines. Add it — as listed, no test in this task can kill M4.
- *Must fail:* `TestPutGraphWritesSymbolsAndEdges` once its fixture holds two `init`s.
- *Expected (verify and correct):* `wrote 3 symbols, read back 2`
- *Compiles and vets:* yes.

**M5 — `EdgeID` uses the line instead of the offset.**
- *Why the code exists:* two calls on one line are two edges, and the id is what makes them two rows.
- *Fixture that separates mutant from original:* `TestTwoCallsOnOneLineAreTwoRows`, whose fixture is `a(b(), b())`. Every other fixture has one call per line.
- *Must fail:* `TestTwoCallsOnOneLineAreTwoRows`
- *Expected (verify and correct):* `repo has 1 edge to b, want 2`
- *Compiles and vets:* yes.

**M6 — the Go-side provenance/target check deleted.**
- *Why the code exists:* it turns a constraint violation on an anonymous row into an error naming the edge.
- *Fixture that separates mutant from original:* `TestPutGraphNamesTheEdgeWhoseProvenanceAndTargetDisagree`, which asserts the error **mentions the edge's `to_name` and its call site**, not merely that an error happened. An assertion of `err != nil` passes under the mutant, because the constraint still fires — this is precisely the "assert which reason, not that it failed" rule.
- *Must fail:* `TestPutGraphNamesTheEdgeWhoseProvenanceAndTargetDisagree`
- *Expected (verify and correct):* `error was "ERROR: new row for relation \"edges\" violates check constraint \"edges_provenance_target\"", want it to name the edge`
- *Compiles and vets:* yes.

**M7 — the CHECK constraint dropped from the migration.**
- *Why the code exists:* it is §3's invariant, made true by construction rather than by convention.
- *Fixture that separates mutant from original:* the two direct-`INSERT` tests, which do not go through `PutGraph` at all. Nothing that writes through `PutGraph` can see this, because M6's Go check refuses first — which is why both exist.
- *Must fail:* `TestTheSchemaRefusesAResolvedEdgeWithNoTargetLive` and `TestTheSchemaRefusesASyntacticEdgeWithATargetLive`
- *Expected (verify and correct):* `INSERT succeeded, want a check constraint violation`
- *Compiles and vets:* yes — it is a migration edit; note that the live suite creates a fresh database per run so the ledger re-runs from empty and the mutated file takes effect. **Verify that**: if the suite reuses a database, the mutation is invisible and the mutation is void.

**M8 — `symbols.repo_id` declared without `ON DELETE CASCADE`.**
- *Why the code exists:* spec §3 — "Eviction is one `DELETE`" — and `Evict`'s doc comment names this as a contract P4 must join.
- *Fixture that separates mutant from original:* `TestEvictingARepoTakesItsGraphWithIt`. Note the failure shape flips: without the cascade, eviction **errors** rather than orphaning, so assert on `Evict`'s error *and* the row counts.
- *Must fail:* `TestEvictingARepoTakesItsGraphWithIt`
- *Expected (verify and correct):* `Evict returned "update or delete on table \"repos\" violates foreign key constraint … on table \"symbols\""`
- *Compiles and vets:* yes.

**M9 — `span_id` declared `ON DELETE CASCADE` instead of `SET NULL`.**
- *Why the code exists:* `PutSpans` deletes a repo's spans on every re-index; a cascade would delete the repo's symbols as a side effect of re-chunking.
- *Fixture that separates mutant from original:* `TestReplacingASpanLeavesTheSymbolWithANullLink`, which writes a graph, then calls `PutSpans` again, then reads the symbols back. No test that writes spans *before* the graph can see it.
- *Must fail:* `TestReplacingASpanLeavesTheSymbolWithANullLink`
- *Expected (verify and correct):* `repo has 0 symbols after re-writing spans, want 2 with null span_id`
- *Compiles and vets:* yes.

- [ ] **Step 5: Commit**

**Definition of Done**
- `symbols` and `edges` exist, cascade from `repos`, and the pair invariant is a constraint rather than a convention — proven by two direct `INSERT`s that the constraint refuses.
- `PutGraph` replaces a repo's graph wholesale in one transaction, converges across two identical runs, and touches no other repo.
- A re-index upgrades an edge's provenance rather than keeping the old label.
- Eviction takes the graph with it, asserted by row counts.
- Re-writing spans leaves symbols with a null link, not with no symbols.
- M1–M9 recorded with observed output, M1's survivor status resolved either way.

---

### Task 3: The type-checker, and the environment it is allowed to have

**This is the sharpest new risk in the project since P1's clone.** P1 forks `git` at a stranger's URL; this task forks `go` inside a stranger's *source tree*, and `go` is a program whose entire purpose is to fetch things and compile them. Everything below exists because of that sentence.

**Files:**
- Create: `packages/shared/symbols/load.go`, `packages/shared/symbols/policy.go`, `packages/shared/symbols/testdata/mod/{std,absent,newgo,cgo,nomod}/…`
- Modify: `go.mod` (add `golang.org/x/tools`)
- Test: `packages/shared/symbols/load_test.go`, `packages/shared/symbols/policy_test.go`

**Interfaces:**
- Consumes: `golang.org/x/tools/go/packages`, `go/types`, `os/exec`.
- Produces:
  - `type Policy struct { Root, Home, GoBin, Proxy string }`
  - `func (p Policy) Env() []string` — the child's **entire** environment
  - `func (p Policy) Validate() error`
  - `type Key struct { Path string; Offset int }` — a call site, as Task 1 records it
  - `type Target struct { Path string; Line int }` — a definition's position, repo-relative
  - `type Stats struct { Packages, Loaded, Failed, Resolved, External, Unresolved int; Reason string }`
  - `func Resolve(ctx context.Context, p Policy) (map[Key]Target, Stats)` — **no error return**

**Decisions, with their reasoning:**

- **`Resolve` has no error return.** Spec:196 makes failure an outcome. An `error` in the signature is an invitation for a caller to propagate it into the job's failure path, and the compiler would not object. What comes back instead is `Stats.Reason` ∈ `{ok, disabled, no_toolchain, no_module, load_error, deadline}` and however many resolutions it managed before whatever went wrong.
- **The child's environment is an allowlist built from nothing, not `os.Environ()` plus overrides.** `clone.Run` appends to `os.Environ()`, which is correct for it — the git variables it sets are the ones that matter and it sets them last. For `go` it is not: the operator's environment may carry `GOFLAGS`, `GOPRIVATE`, `GOINSECURE`, `GONOSUMDB`, `GOPROXY`, `GOPATH`, `GOEXPERIMENT` and a dozen more, some of which re-open exactly what this policy closes. `GOENV=off` is in the list for the same reason: `~/.config/go/env` is a second place the same settings live, and it survives an empty environment.
- **`GOPROXY=off` by default.** This is the decision the rest of the phase leans on. With it, a repository whose packages import only the standard library type-checks completely, and a repository importing anything else does not — its packages fail to load, its edges stay syntactic, and §6 already calls that an expected outcome. **The alternative is worse than it looks:** a module fetch is driven by `require` lines in a *stranger's* `go.mod`, so with fetching on, the set of hosts the indexer contacts is chosen by the submitter. P1's whole admission design exists to stop that, and it would be undone here by a setting rather than by a decision.
- **`GOVCS=*:off`, always, in both modes.** With `GOPROXY=direct` — or with any `GOPRIVATE` pattern matching — the go command fetches modules with `git`, at a host the `require` line names. That is arbitrary outbound `git clone`, which is precisely what P1's host allowlist refuses, reached by a path the allowlist never sees. `GOVCS=*:off` closes it. So does `Validate` refusing a `Proxy` of `direct` or one containing `,direct`.
- **`GOTOOLCHAIN=local`.** A `go 1.29.0` directive in a stranger's `go.mod` makes the go command **download a toolchain** — a large binary, from the proxy, before any of our policy about modules applies. With `local`, the repository fails to load and says why. This is the trap that is easiest to miss, because nothing in the source tree looks like a dependency.
- **`CGO_ENABLED=0`.** Type-checking a package with `import "C"` runs `cgo`, and a `#cgo LDFLAGS:` directive is code execution on a stranger's terms. Off, and such packages simply do not load.
- **`GOWORK=off`.** A `go.work` file inside the checkout can name directories outside it; `off` means the module is the module.
- **`GOMODCACHE`, `GOCACHE`, `GOPATH` and `GOTMPDIR` all under the job's scratch directory**, which the indexer already removes per job and sweeps at boot and at exit. Two reasons: nothing a stranger's build writes outlives the job, and no two jobs share a cache one of them wrote. The cost is real and is recorded in Open question 10 — with `GOPROXY=off` there is no module cache to reuse, and the build cache starts empty every time.
- **`PATH` contains exactly one directory: the one holding the `go` binary**, resolved once at boot with `exec.LookPath`. `go` needs `PATH` to find its own tools; it does not need the operator's.
- **The git variables from `clone.Run` are set here too** (`GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS=/bin/false`, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`). Redundant under `GOVCS=*:off`, kept because two independent controls against arbitrary outbound `git` is the right number for the one thing in this phase that would be a genuine vulnerability.
- **No `packages.Config.ParseFile` hook, and the reason is a measurement.** The chunker may be reading *stripped* bytes (`STRIP_DOC_COMMENTS`), while `go/packages` reads the file from disk. Those would be two coordinate systems if stripping moved anything — but `StripDocs` **blanks doc bytes in place and keeps every newline**, so byte offsets and line numbers are identical in both streams. Task 1's `TestOffsetsAndRangesSurviveDocCommentStripping` is what makes this true, and it is named in a comment here, because the day someone makes stripping *delete* bytes, this task's resolution rate silently collapses with no error anywhere.
- **Only a call resolving to a `*types.Func` that is package-scoped or a method is resolved.** `Uses` will happily resolve `fn()` where `fn` is a local variable holding a closure — to the *variable*, whose position is inside the enclosing function, which would map to an edge from a function to itself. A self-call that is not a self-call is worse than no edge.
- **A call that resolves to an object outside the checkout is `External`, and produces no target.** `fmt.Println` type-checks perfectly and has no `symbols` row to point at. §3 says a resolved edge points at a symbol row and spec:84 says a null target means syntactic; both cannot hold for this call, so the invariant wins and the fact is kept in `Stats.External`. See "what the spec leaves underspecified".
- **`packages.Load(cfg, "./...")` from the checkout root**, `Tests: false`, mode `NeedName|NeedFiles|NeedSyntax|NeedTypes|NeedTypesInfo`. `NeedDeps` is deliberately absent: it asks for the dependency graph's types, which is what makes the go command build things. `-e` behaviour (errors tolerated, packages still returned) is what `packages.Load` does by default with `NeedSyntax`; **verify that against the version resolved into `go.mod`** rather than trusting this sentence.

- [ ] **Step 1: Measure what the go command does under this policy, before writing the policy**

Fixture modules under `testdata/mod/`, each a directory with a `go.mod` (named `go.mod.txt` and copied into a `t.TempDir()`, so the repository's own `go build ./...` does not try to build them):

| fixture | shape | what it is for |
| --- | --- | --- |
| `std` | two packages, standard library imports only, one calling the other | the resolved path |
| `absent` | `require example.com/nope v1.0.0` | the expected failure |
| `newgo` | `go 1.99.0` | the toolchain trap |
| `cgo` | `import "C"` with a `#cgo LDFLAGS` line | the execution trap |
| `nomod` | no `go.mod` at all | the shape most single-file repositories have |

Run each by hand first, with `GOPROXY` pointed at a listener that logs, and **paste the transcript into the commit message**. The questions this settles, none of which this plan should guess:

1. Does `packages.Load` return syntax and partial type info for the packages that *did* load when a sibling package fails? (The whole per-edge design depends on yes.)
2. What does `Stats` look like for `nomod` — one package, or zero?
3. Does `GOTOOLCHAIN=local` fail `newgo` before or after any network activity?
4. With `GOPROXY=off`, is anything at all attempted over the network? The answer must be **nothing**, and the recorder is how it is proven.

- [ ] **Step 2: Write the failing tests**

```go
// policy_test.go — hermetic.
func TestTheChildEnvironmentIsAnAllowlist(t *testing.T)     // no inherited GO* survives
func TestAProxyOfDirectIsRefused(t *testing.T)              // Validate: "direct", "off,direct", "https://x,direct"
func TestPolicyRefusesAnEmptyGoBinOrRoot(t *testing.T)

// load_test.go — runs the real go command; no network required.
func TestCallsWithinTheModuleResolveToTheirDefinitions(t *testing.T)      // std
func TestACallIntoTheStandardLibraryIsExternalNotResolved(t *testing.T)   // std
func TestACallToALocalClosureIsNotResolved(t *testing.T)                  // std
func TestAPackageThatCannotLoadLeavesItsCallsUnresolved(t *testing.T)     // absent
func TestOnePackageResolvesWhileItsSiblingDoesNot(t *testing.T)           // absent: the per-edge claim
func TestTheLoaderNeverReachesTheModuleProxy(t *testing.T)                // absent + recorder
func TestTheOperatorsGoflagsCannotBreakTheLoad(t *testing.T)              // parent GOFLAGS=-mod=vendor
func TestAToolchainDirectiveDoesNotFetchAToolchain(t *testing.T)          // newgo + recorder, proxy mode
func TestACgoPackageDoesNotLoad(t *testing.T)                             // cgo
func TestARepositoryWithNoGoModRecordsItsReason(t *testing.T)             // nomod
func TestAnExpiredContextResolvesNothingAndSaysWhy(t *testing.T)          // Reason == "deadline"
func TestResolutionIsKeyedByOffsetNotByLine(t *testing.T)                 // a(b(), b())
```

`TestTheLoaderNeverReachesTheModuleProxy` is the test that carries this whole task, and its design is the point: it starts an `httptest` server that counts requests, sets `GOPROXY` **in the parent process** to that server's URL, runs `Resolve` over the `absent` fixture, and asserts the counter is **zero**. The original never contacts it because the policy sets `GOPROXY=off`; a mutant that inherits the environment, or drops the `off`, contacts it and the counter moves. A test that merely asserted `Env()` contains `GOPROXY=off` would pass under a mutant that appends the inherited value afterwards.

`TestOnePackageResolvesWhileItsSiblingDoesNot` carries the spec's per-edge claim: the `absent` fixture is two packages, one importing only the standard library and one importing the missing module, each with one call, and the assertions name **which** call resolved and **which** did not. A count of resolutions would pass under a mutant that swapped them.

**CI note:** these tests run the go command and need no network, so they run in the ordinary `go test ./...` step rather than behind a tag. Keep the fixtures to two files each — the point is the policy, not the corpus. If any of them turns out to need the network, it does not belong in this suite at all.

- [ ] **Step 3: Implement**

```go
// Env is the child's entire environment, not an addition to ours. The go
// command reads a dozen settings that re-open what this policy closes, and
// GOENV=off is here because ~/.config/go/env is a second copy of all of them.
//
// GOPROXY=off by default: a module fetch is driven by require lines in a
// stranger's go.mod, so with fetching on, the set of hosts this process
// contacts is chosen by whoever submitted the repository — which is the thing
// P1's admission allowlist exists to prevent, reached by a road it cannot see.
//
// GOVCS=*:off in both modes: with a direct proxy or a matching GOPRIVATE, the
// go command fetches with git, at a host a require line names.
//
// GOTOOLCHAIN=local: a "go 1.29.0" directive otherwise downloads a toolchain
// before any policy about modules applies.
//
// CGO_ENABLED=0: type-checking import "C" runs cgo, and #cgo LDFLAGS is
// arbitrary execution.
func (p Policy) Env() []string
```

- [ ] **Step 4: Commit, then prove the tests discriminate**

**M1 — `Env()` returns `append(os.Environ(), …)` instead of the allowlist.**
- *Why the code exists:* the operator's environment can re-open every control in this file.
- *Fixture that separates mutant from original:* two, and both are needed. `TestTheOperatorsGoflagsCannotBreakTheLoad` sets `GOFLAGS=-mod=vendor` in the parent and asserts the `std` fixture still resolves — under the mutant the go command refuses the whole load because there is no vendor directory. `TestTheLoaderNeverReachesTheModuleProxy` catches the other half. **An assertion over `Env()`'s contents cannot** replace either: the mutant's slice *contains* `GOPROXY=off`, just not last.
- *Must fail:* `TestTheOperatorsGoflagsCannotBreakTheLoad`
- *Expected (verify and correct):* `resolved 0 of 2 calls, reason "load_error"; want 2 resolved` — verify the reason string the go command's vendor complaint produces.
- *Compiles and vets:* yes.

**M2 — `GOPROXY=off` dropped from the allowlist.**
- *Why the code exists:* it is the control that decides whether this process fetches anything at all.
- *Fixture that separates mutant from original:* the `absent` fixture plus the recording server, with `GOPROXY` set in the parent. **The `std` fixture cannot see this** — it needs no modules, so nothing is fetched under either version. That is the trap: a policy test written over the happy fixture proves nothing.
- *Must fail:* `TestTheLoaderNeverReachesTheModuleProxy`
- *Expected (verify and correct):* `the module proxy received 2 requests (/example.com/nope/@v/list, …), want 0`
- *Compiles and vets:* yes.

**M3 — `GOVCS=*:off` dropped.**
- *Why the code exists:* it is the second lock on arbitrary outbound `git`.
- *Fixture that separates mutant from original:* **none, and this is recorded as a survivor.** With `GOPROXY=off` no fetch of any kind is attempted, so `GOVCS` is never consulted; the only configuration that would consult it is `GOPROXY=direct`, which `Validate` refuses. The instrument for that refusal is `TestAProxyOfDirectIsRefused`, which is a different mutation (M4). Recorded as defence in depth with the reason it cannot be killed, rather than deleted for being untestable.
- *Must fail:* nothing. Survivor.
- *Compiles and vets:* yes.

**M4 — `Validate` accepts `direct` as a proxy.**
- *Why the code exists:* `GOPROXY=direct` turns a stranger's `require` line into an outbound connection to a host of their choosing.
- *Fixture that separates mutant from original:* `TestAProxyOfDirectIsRefused`, whose table includes `"off,direct"` and `"https://proxy.example,direct"` — a naive `p.Proxy == "direct"` check passes the plain case and misses both fallbacks, which is the shape the mutation should take.
- *Must fail:* `TestAProxyOfDirectIsRefused`
- *Expected (verify and correct):* `Validate("https://proxy.example,direct") returned nil, want an error naming the setting`
- *Compiles and vets:* yes.

**M5 — `GOTOOLCHAIN=local` dropped.**
- *Why the code exists:* a `go` directive newer than the installed toolchain otherwise triggers a toolchain download.
- *Fixture that separates mutant from original:* the `newgo` fixture **in proxy mode**, with the recorder as the proxy. In the default mode both versions fail without a fetch, because `GOPROXY=off` blocks the toolchain download too — so a test run in the default mode cannot separate them, which is worth saying out loud since the default mode is what ships.
- *Must fail:* `TestAToolchainDirectiveDoesNotFetchAToolchain`
- *Expected (verify and correct):* `the module proxy received a request for /golang.org/toolchain/@v/…, want 0 requests` — verify the exact path shape; it is a normal module path and the recorder should log it.
- *Compiles and vets:* yes.

**M6 — `CGO_ENABLED=0` dropped.**
- *Why the code exists:* `#cgo LDFLAGS:` in a stranger's source is arbitrary execution during a type-check.
- *Fixture that separates mutant from original:* the `cgo` fixture. **The kill is machine-dependent and must be recorded as such:** on CI's `ubuntu-latest` a C toolchain is present, so the mutant loads the package and the assertion that its call is unresolved fails. On a machine with no C compiler both versions fail. Record which machine produced the result.
- *Must fail:* `TestACgoPackageDoesNotLoad`
- *Expected (verify and correct):* `the cgo package's call to helper resolved, want unresolved` (on a machine with a C toolchain).
- *Compiles and vets:* yes.

**M7 — the context is replaced by a fresh one (`context.WithTimeout(context.Background(), 2*time.Minute)`).**
- *Why the code exists:* spec:194 — one deadline for the whole job.
- *Fixture that separates mutant from original:* `TestAnExpiredContextResolvesNothingAndSaysWhy`, which hands `Resolve` an already-cancelled context over the `std` fixture. **A fixture with time left cannot see this**, and neither can one whose packages fail to load anyway.
- *Must fail:* `TestAnExpiredContextResolvesNothingAndSaysWhy`
- *Expected (verify and correct):* `resolved 2 calls with reason "ok", want 0 and "deadline"`
- *Compiles and vets:* yes.

**M8 — the `*types.Func` / package-scope guard removed, so a local closure resolves.**
- *Why the code exists:* `Uses` resolves `fn()` to the variable holding the closure, whose position lies inside the enclosing function, producing an edge from a function to itself.
- *Fixture that separates mutant from original:* the `std` fixture's function containing `fn := func() {...}; fn()`. Every fixture without a closure passes under the mutant.
- *Must fail:* `TestACallToALocalClosureIsNotResolved`
- *Expected (verify and correct):* `call to fn resolved to run:12, want no target` — verify whether the object's position is the variable's declaration line or the literal's.
- *Compiles and vets:* yes.

**M9 — a target outside the checkout is returned rather than counted as external.**
- *Why the code exists:* a standard-library function has no `symbols` row, and §3's resolved edge must point at one.
- *Fixture that separates mutant from original:* `TestACallIntoTheStandardLibraryIsExternalNotResolved`, asserting both that the map has no entry **and** that `Stats.External` moved. Asserting only the absence of the entry passes under a mutant that drops the call on the floor without counting it — which is a different bug with the same symptom.
- *Must fail:* `TestACallIntoTheStandardLibraryIsExternalNotResolved`
- *Expected (verify and correct):* `fmt.Println resolved to /usr/local/go/src/fmt/print.go:314, want external`
- *Compiles and vets:* yes.

**M10 — resolution keyed by line instead of offset.**
- *Why the code exists:* the key has to be the same key Task 1 wrote, and two calls on one line are two keys.
- *Fixture that separates mutant from original:* `a(b(), b())` in the `std` fixture. This is the same trap as Task 1's M3, one layer down, and the fixture has to exist in both places.
- *Must fail:* `TestResolutionIsKeyedByOffsetNotByLine`
- *Expected (verify and correct):* `resolved 1 call at line 9, want 2 at offsets 96 and 101` — the offsets are the fixture's; paste the real ones.
- *Compiles and vets:* yes.

**M11 — `Stats.Reason` hard-wired to `"ok"`.**
- *Why the code exists:* it is the only thing that distinguishes "this repository has no cross-package calls" from "the type-checker never ran", and Task 4 turns it into a counter and a log line.
- *Fixture that separates mutant from original:* `nomod` and `absent`, whose reasons differ from each other (`no_module` versus `load_error`) — a test that asserts merely "reason is not empty" cannot tell those apart, and telling them apart is the entire value of the field.
- *Must fail:* `TestARepositoryWithNoGoModRecordsItsReason`
- *Expected (verify and correct):* `reason "ok", want "no_module"`
- *Compiles and vets:* yes.

- [ ] **Step 5: Commit**

**Definition of Done**
- The child's environment is an allowlist, proven by a test in which the *parent* environment is hostile.
- With the default policy, a recording proxy receives **zero** requests while type-checking a module that requires an absent dependency.
- A `go` directive newer than the toolchain does not download a toolchain, proven in proxy mode where the difference is observable.
- A cgo package does not load, recorded with the machine the result came from.
- One package resolving while its sibling does not, asserted by naming both calls.
- An expired context resolves nothing and says `deadline`.
- `Resolve` has no error return, and nothing in the package returns one to a caller.
- M1–M11 recorded with observed output; M3 recorded as a survivor with its reason.

---

### Task 4: The indexer stage — per-edge labelling, one deadline, and a failure that is not a failure

**Files:**
- Modify: `apps/indexer/cmd/main.go` (the `indexer` struct, `runJob`, `limitsFrom`), `packages/shared/metrics/metrics.go`
- Create: `apps/indexer/cmd/graph.go`
- Test: `apps/indexer/cmd/graph_test.go`, additions to `apps/indexer/cmd/index_live_test.go`

**Interfaces:**
- Consumes: `symbols.Parse`, `symbols.Resolve`, `store.PutGraph`, `store.SymbolID`, `store.EdgeID`.
- Produces on the worker: `graph func(ctx context.Context, p symbols.Policy) (map[symbols.Key]symbols.Target, symbols.Stats)` (the seam, so tests substitute a resolver), `putGraph func(ctx context.Context, repoID string, syms []models.Symbol, edges []models.Edge) error`, and `typecheck bool` / `goBin string` on `limits`.
- Produces in `metrics`: `CountGraph(provenance string)`, `CountTypecheck(reason string)`, `ObserveTypecheck(d time.Duration)`.

**Decisions, with their reasoning:**

- **The stage runs after `putSpans` and before `Complete`, on `jobCtx`.** After `putSpans` because `symbols.span_id` references a span row that must already exist and because the span ranges are what the containment link is computed against; before `Complete` because the checkout is removed by `runJob`'s `defer os.RemoveAll(dir)` and the type-checker needs the files. On `jobCtx` because spec:194 says one deadline, and `jobCtx` is the one the clone, the walk, the chunker and the embedder already share.
- **The edge set is built from the AST, then upgraded.** `symbols.Parse` over the same files `index` walked, producing every edge as `syntactic` with a null target; then `symbols.Resolve`'s map is consulted per call site and the ones it names become `resolved`. The two never produce different *sets*, which is the property Task 7 asserts end to end.
- **The label is written per call site, from a map lookup.** Not from `Stats`, not from "did this package load". The temptation is a loop over packages; the code must have no place where a package-level fact is copied into a row.
- **A failed type-check is logged once per job at `warn`, counted by reason, and the job continues.** The existing template is `index`'s aggregate warn carrying `vanished`, `unstrippable` and `tokenless`; this adds a line carrying `symbols`, `edges`, `resolved`, `syntactic`, `external`, `unnameable` and `reason`. One line per job, no per-file logging, and **no repository path, file path or symbol name in a metric label** — `metrics`' package doc forbids it.
- **`TYPECHECK=false` turns the stage off** and the reason becomes `disabled`. A kill switch is the right escape hatch for a stage whose failure is already expected, and it is the honest answer to an operator who cannot ship a Go toolchain: the alternative is a stage that fails in a way that looks like a bug every time.
- **The `go` binary is resolved once at boot with `exec.LookPath`, and its absence is a `warn` line at boot plus `reason=no_toolchain` on every job — not a boot failure.** P3's review round made two unvalidated knobs fail at boot, and the instinct here is the same; it is wrong here for a specific reason. Every other boot check guards something without which the indexer cannot do its job. This one guards a stage whose absence downgrades a label. Refusing to boot would mean an operator with no toolchain cannot index at all, which is a worse product than one that indexes with syntactic edges and says so. **The visible failure mode is the one §8 calls the failure that costs a week — a silent downgrade — so it is not silent: a boot line, a counter, and a `provenance` column a caller can read.**
- **`span_id` links to the containing span, most specific first.** Under `CHUNK_STRATEGY=window` the windows overlap by `WindowOverlap` lines, so a definition's first line can be inside two spans; the rule is the containing span with the **greatest `start_line`, then the smallest `end_line`, then the smallest id**, which is deterministic and picks the window that starts closest to the definition. Under the AST strategy the declaration's own span is the only container and the rule is a no-op — which is exactly why a window-strategy fixture is needed to test it at all.

- [ ] **Step 1: Write the failing tests**

Hermetic, in `graph_test.go`, against a substituted resolver — the seam exists so that the labelling logic is testable without running the go command twice per assertion:

```go
func TestEveryCallIsAnEdgeBeforeAnythingIsResolved(t *testing.T)
func TestOnlyTheCallSitesTheResolverNamedAreResolved(t *testing.T)   // the per-edge claim
func TestAResolverFailureLeavesEveryEdgeSyntacticAndTheJobIntact(t *testing.T)
func TestASymbolLinksToTheSpanThatContainsIt(t *testing.T)
func TestASymbolLinksToTheMostSpecificOfTwoOverlappingWindows(t *testing.T)
func TestADeclarationWithNoSpanStillGetsASymbolWithANullLink(t *testing.T)
func TestTheJobLogNamesWhatTheGraphStageProducedAndWhyItStopped(t *testing.T)
func TestTheGraphCountersMoveExactlyOnce(t *testing.T)               // whole set of series
```

Live, appended to `index_live_test.go` (same `TestMain`, same `codetrail_indexer` prefix):

```go
func TestIndexingWritesAGraphForTheFixtureRepoLive(t *testing.T)
func TestATypecheckFailureStillCompletesTheJobLive(t *testing.T)     // GOPROXY unreachable-by-policy fixture
func TestTheGraphStageSharesTheJobDeadlineLive(t *testing.T)
```

`TestTheGraphCountersMoveExactlyOnce` reads **every** series in `codetrail_graph_edges_total` and `codetrail_typecheck_total` before and after, and asserts the whole delta vector. P3 learned this twice: a test that checks only the counter it expects to move passes under a mutant that increments both.

- [ ] **Step 2: Implement**

Sketch of the stage, showing only the shape the mutations attack:

```go
// Spec §6: the edge set is the AST's; type information only upgrades rows.
// There is deliberately no branch here on whether a package loaded — the label
// is a property of a call site, and a per-package branch is how it stops being
// one.
for _, c := range calls {
	e := models.Edge{ /* … */ ToName: c.Name, Kind: models.EdgeCalls,
		Provenance: models.ProvenanceSyntactic}
	if t, ok := resolved[symbols.Key{Path: c.Path, Offset: c.Offset}]; ok {
		if id, ok := defIndex.at(t); ok {
			e.ToSymbolID, e.Provenance = &id, models.ProvenanceResolved
		}
	}
	edges = append(edges, e)
}
```

- [ ] **Step 3: Commit, then prove the tests discriminate**

**M1 — the label is taken from the package's load status rather than from the call site** (`if stats.Reason == "ok" { e.Provenance = resolved }`).
- *Why the code exists:* spec:190. This is the mutation this whole phase is written against.
- *Fixture that separates mutant from original:* a repository where the resolver names **some** call sites and not others — i.e. any fixture containing a standard-library call beside an in-repo one. **A fixture in which every call resolves cannot see it**, and neither can one in which none do.
- *Must fail:* `TestOnlyTheCallSitesTheResolverNamedAreResolved`
- *Expected (verify and correct):* `edge to Println is resolved with a null target, want syntactic` — verify whether the mutant fails at the assertion or earlier, in `PutGraph`'s pair check; if it is the latter in the live test, the hermetic test is the one that carries the kill.
- *Compiles and vets:* yes.

**M2 — a resolver failure is returned and fails the job.**
- *Why the code exists:* spec:196.
- *Fixture that separates mutant from original:* the substituted resolver returning `Reason: "load_error"` and an empty map. **This is a fake, and M2 is the mutation that proves the fake's error path is wired** — the second of the two survivor shapes P3's sweep found.
- *Must fail:* `TestAResolverFailureLeavesEveryEdgeSyntacticAndTheJobIntact`
- *Expected (verify and correct):* `runJob returned "graph: load_error", want nil and 4 syntactic edges`
- *Compiles and vets:* yes.

**M3 — the stage gets its own deadline (`context.WithTimeout(ctx, 2*time.Minute)`).**
- *Why the code exists:* spec:194, and the sentence names the exact failure: a slow clone buying extra time by failing into the next stage.
- *Fixture that separates mutant from original:* `TestTheGraphStageSharesTheJobDeadlineLive`, which sets `JOB_DEADLINE_SECONDS` low enough that the budget is spent by the time the stage starts, and asserts every edge is syntactic with `reason=deadline`. **A generous deadline cannot see this at all.** Verify the timing is not flaky; if it is, assert on the deadline the stage observed (record it in the log line) rather than on the elapsed time.
- *Must fail:* `TestTheGraphStageSharesTheJobDeadlineLive`
- *Expected (verify and correct):* `12 of 12 edges resolved with reason "ok", want 0 and "deadline"`
- *Compiles and vets:* yes.

**M4 — the stage runs before `putSpans`.**
- *Why the code exists:* `symbols.span_id` references a row that must exist, and the containment link is computed against ranges that are only known once the chunker has run.
- *Fixture that separates mutant from original:* the live fixture repo, asserting a **non-null** `span_id` for a named symbol. A test that only counts symbols passes.
- *Must fail:* `TestIndexingWritesAGraphForTheFixtureRepoLive`
- *Expected (verify and correct):* a foreign-key violation from `PutGraph`, or every `span_id` null — verify which, since the ordering decides it. If it is the FK error, note that the mutant fails the *job*, so the assertion that carries the kill is the job's status, not the row count.
- *Compiles and vets:* yes.

**M5 — the span link is by exact range instead of containment.**
- *Why the code exists:* a declaration longer than `MaxDeclLines` has no span with its range, and those are the declarations most worth asking about.
- *Fixture that separates mutant from original:* a fixture file with a declaration longer than `CHUNK_MAX_DECL_LINES` — the live fixture repo needs one added. Every short declaration matches exactly and passes under the mutant.
- *Must fail:* `TestASymbolLinksToTheSpanThatContainsIt`
- *Expected (verify and correct):* `symbol Big has a null span_id, want the span 3..42`
- *Compiles and vets:* yes.

**M6 — the containing span is chosen by smallest `start_line` instead of greatest.**
- *Why the code exists:* window strategy produces overlapping spans and the link must be deterministic and specific.
- *Fixture that separates mutant from original:* `TestASymbolLinksToTheMostSpecificOfTwoOverlappingWindows`, whose spans overlap by design. **The AST-strategy fixture cannot** — its spans do not overlap, and this is the one rule that is invisible in production configuration.
- *Must fail:* `TestASymbolLinksToTheMostSpecificOfTwoOverlappingWindows`
- *Expected (verify and correct):* `symbol Parse linked to span 1..40, want 31..70`
- *Compiles and vets:* yes.

**M7 — `CountGraph` called once per job with the total instead of once per edge by provenance.**
- *Why the code exists:* the provenance split is the phase's headline claim, and a total tells a dashboard nothing about it.
- *Fixture that separates mutant from original:* `TestTheGraphCountersMoveExactlyOnce` with a fixture producing **both** labels. A fixture producing one label leaves the two series indistinguishable.
- *Must fail:* `TestTheGraphCountersMoveExactlyOnce`
- *Expected (verify and correct):* `{provenance="resolved"} moved by 0, {provenance="syntactic"} by 0, {provenance=""} by 1`
- *Compiles and vets:* **check.** If the label value is validated by a closed set at the call site, the mutant may panic instead — rewrite it as "both increments use `syntactic`" in that case, which is a labelling error rather than a cardinality one.

**M8 — the job log line drops `reason`.**
- *Why the code exists:* it is the only place an operator learns that a repository's edges are all syntactic because the toolchain is missing, rather than because the code has no cross-package calls.
- *Fixture that separates mutant from original:* `TestTheJobLogNamesWhatTheGraphStageProducedAndWhyItStopped`, capturing zerolog's output. **This mutation exists because the log line is a side effect and P3's sweep found side effects with no reader** — the test is the reader.
- *Must fail:* `TestTheJobLogNamesWhatTheGraphStageProducedAndWhyItStopped`
- *Expected (verify and correct):* `log line has no "reason" field: {"level":"warn","symbols":12,…}`
- *Compiles and vets:* yes.

**M9 — the boot probe for the `go` binary removed, so `GoBin` is empty.**
- *Why the code exists:* an indexer with no toolchain must say so once at boot rather than produce a repository of syntactic edges that looks like a property of the code.
- *Fixture that separates mutant from original:* a boot test with `PATH` emptied, asserting the boot log line and `reason=no_toolchain` on the first job. Verify what `Resolve` does with an empty `GoBin` — if `Policy.Validate` refuses it, the failure surfaces as `Reason` and the boot line is the only thing the mutation removes.
- *Must fail:* `TestTheJobLogNamesWhatTheGraphStageProducedAndWhyItStopped`'s no-toolchain subtest
- *Expected (verify and correct):* `reason "load_error", want "no_toolchain"`
- *Compiles and vets:* yes.

**M10 — `TYPECHECK=false` still runs the stage.**
- *Why the code exists:* the kill switch is the operator's answer to a toolchain they cannot ship, and a knob that does nothing is worse than no knob (P3's review round found two).
- *Fixture that separates mutant from original:* a hermetic run with the flag off and a resolver that **panics if called** — the strongest form, since asserting "edges are syntactic" also passes if the resolver ran and failed.
- *Must fail:* `TestAResolverFailureLeavesEveryEdgeSyntacticAndTheJobIntact`'s disabled subtest
- *Expected (verify and correct):* `panic: resolver called with TYPECHECK=false` surfacing as a test failure with `reason "ok", want "disabled"`.
- *Compiles and vets:* yes.

**M11 — `unnameable` dropped from the log line and from the stage's return.**
- *Why the code exists:* a call the AST cannot name is a call that produced no edge; without the count, a repository of `fns[i]()` calls looks like a repository with no calls.
- *Fixture that separates mutant from original:* the hermetic fixture containing `fns[i]()`. **Same shape as M8: a side effect whose reader is a log assertion.**
- *Must fail:* `TestTheJobLogNamesWhatTheGraphStageProducedAndWhyItStopped`
- *Expected (verify and correct):* `log line has no "unnameable" field`
- *Compiles and vets:* yes.

- [ ] **Step 4: Commit**

**Definition of Done**
- Every call site is an edge before resolution; resolution changes labels and targets and never the edge count.
- No line in the stage copies a package-level fact onto a row.
- A resolver failure, a missing toolchain, an expired budget and `TYPECHECK=false` each produce a complete graph of syntactic edges, a distinct reason, a counted outcome and a `done` job.
- The stage runs on `jobCtx`, proven by a live test whose budget is spent before the stage starts.
- Symbols link to the containing span, most specific first, with a window-overlap fixture proving the tie-break.
- The job log names symbols, edges, the provenance split, `external`, `unnameable` and `reason`.
- M1–M11 recorded with observed output.

---

### Task 5: The graph reads — a recursive CTE, and the set it refuses to merge

"Who calls this" is a recursive CTE. That sentence is in `infra/docker-compose.yml`'s first comment as the reason this project has one datastore; this task is where the claim is either true or an excuse.

**Files:**
- Create: `packages/shared/store/graph_read.go`
- Modify: `packages/shared/store/read.go` (`Stats` grows the graph counts)
- Test: `packages/shared/store/graph_read_live_test.go` (`//go:build live`)

**Interfaces:**
- Produces:
  - `type Caller struct { Symbol models.Symbol; Depth int; CallPath string; CallLine int }`
  - `type Approximate struct { Symbol models.Symbol; ToName, CallPath string; CallLine int }`
  - `func (s *Store) Definitions(ctx context.Context, repoID, name string, suffix bool, limit int) ([]models.Symbol, error)`
  - `func (s *Store) Symbol(ctx context.Context, repoID, symbolID string) (models.Symbol, error)`
  - `func (s *Store) CallersOf(ctx context.Context, repoID, symbolID string, depth, limit int) ([]Caller, error)`
  - `func (s *Store) ApproximateCallersOf(ctx context.Context, repoID, name string, limit int) ([]Approximate, error)`
  - `store.Stats` gains `Symbols, Edges, EdgesResolved, EdgesSyntactic int`

**Decisions, with their reasoning:**

- **`CallersOf` traverses `to_symbol_id` and nothing else.** A join from a syntactic edge's `to_name` to a `symbols.name` is the guess spec:84 forbids, made at query time where no column records that it happened. It would also be *invisible* in any corpus with unique names, which is most small fixtures and no real repository.
- **The approximate set is a separate query, returned separately, at depth 1 only.** It answers a different question — "what else in this repository calls something with this name" — and merging the two would produce a "callers" list whose members are partly facts and partly coincidences, with no way for a caller to tell which is which. Depth 1 because traversing *from* a guess compounds it: at depth 2 the result would be "things that call something that might be this".
- **The cycle guard is an explicit `path` array with `NOT from_symbol_id = ANY(path)`, not SQL's `CYCLE` clause.** The array is needed anyway to *report* the chain, and one mechanism doing both is one thing to get wrong. Recursion in a call graph is not an edge case — it is `f` calling `f`, mutual recursion between two helpers, and any interpreter or tree walker in the corpus.
- **The depth bound and the cycle guard are two independent controls and each is separately killable.** With the cycle guard removed, the depth bound still terminates the query and the wrongness shows up as duplicate and self rows; with the depth bound removed, the cycle guard still terminates it and the wrongness shows up as rows deeper than asked for. **Removing both is a query that does not terminate, which rule 6 makes a non-mutation**, so it is excluded rather than attempted.
- **A diamond is deduplicated with `min(depth)`.** Two paths of different lengths to the same caller are one caller, at its shortest distance. Without the aggregation the same symbol appears twice, which is not an infinite loop and not an error — it is a plausible-looking wrong answer, and it is why the fixture has a diamond in it.
- **`Definitions` matches `name` exactly by default.** The corpus spells a method `Store.Get`, P3's lexical arm reaches it through its parts, and a caller who types `Get` should not silently get every `Get` in the repository presented as though they had asked for it. `suffix=true` is the opt-in, and the rows say which match they came from.
- **`Stats` grows the graph counts so the repo view can show the provenance split.** A per-repo *summary* is not a per-repo *label*: the column stays per row, and the summary is an aggregate over it, which is what makes "three packages resolved and two did not" visible to a user at all.

- [ ] **Step 1: Write the failing live tests**

The fixture is the load-bearing part of this task and every rule in "Fixtures that can tell a graph bug" applies at once. Two repositories; in the target repo, one package with:

```
main ──▶ a ──▶ b ──▶ target        (a chain, so depth is observable)
main ──▶ c ──▶ target              (a diamond: two paths to target)
target ──▶ target                  (a direct self-call)
d ──▶ e ──▶ d                      (a two-node cycle, reaching target through e)
Store.Get and Cache.Get            (two definitions sharing a last segment)
one syntactic edge to "Get"        (null target, for the approximate set)
```

and, in the **other** repo, a symbol also named `target` with **more** callers than the target repo's, so a missing repo filter changes the answer rather than adding a row.

```go
func TestCallersOfWalksTheChainAndReportsDepth(t *testing.T)
func TestCallersOfTerminatesOnASelfCall(t *testing.T)
func TestCallersOfTerminatesOnATwoNodeCycle(t *testing.T)
func TestADiamondYieldsOneCallerAtItsShortestDepth(t *testing.T)
func TestCallersOfStopsAtTheRequestedDepth(t *testing.T)
func TestCallersOfNeverIncludesASyntacticEdge(t *testing.T)
func TestCallersOfReportsTheCallSiteOfEachHop(t *testing.T)
func TestApproximateCallersAreNameMatchedAndSeparate(t *testing.T)
func TestApproximateCallersExcludeResolvedEdges(t *testing.T)
func TestDefinitionsMatchesExactlyUnlessSuffixIsAsked(t *testing.T)
func TestDefinitionsIsScopedToOneRepo(t *testing.T)
func TestRepoStatsSplitsEdgesByProvenance(t *testing.T)
```

`TestCallersOfWalksTheChainAndReportsDepth` asserts the **whole ordered set with each row's depth**, not that `a` is present. `TestADiamondYieldsOneCallerAtItsShortestDepth` asserts the count *and* the depth: a mutant that keeps both rows and one that reports the longer depth are different bugs and the message should say which.

- [ ] **Step 2: Implement**

```sql
WITH RECURSIVE callers AS (
    SELECT e.from_symbol_id AS sym, 1 AS depth,
           ARRAY[e.to_symbol_id, e.from_symbol_id] AS path,
           e.path AS call_path, e.line AS call_line
    FROM edges e
    WHERE e.repo_id = $1 AND e.to_symbol_id = $2
  UNION ALL
    SELECT e.from_symbol_id, c.depth + 1,
           c.path || e.from_symbol_id, e.path, e.line
    FROM edges e
    JOIN callers c ON e.to_symbol_id = c.sym
    WHERE e.repo_id = $1
      AND c.depth < $3
      AND NOT e.from_symbol_id = ANY(c.path)
)
SELECT s.id, s.name, s.pkg, s.kind, s.path, s.start_line, s.end_line, s.span_id,
       min(c.depth) AS depth,
       (array_agg(c.call_path ORDER BY c.depth, c.call_path, c.call_line))[1] AS call_path,
       (array_agg(c.call_line ORDER BY c.depth, c.call_path, c.call_line))[1] AS call_line
FROM callers c JOIN symbols s ON s.id = c.sym
GROUP BY s.id, s.name, s.pkg, s.kind, s.path, s.start_line, s.end_line, s.span_id
ORDER BY depth, s.path, s.start_line, s.id
LIMIT $4
```

The approximate query is separate and deliberately dull:

```sql
SELECT s.*, e.to_name, e.path, e.line
FROM edges e JOIN symbols s ON s.id = e.from_symbol_id
WHERE e.repo_id = $1 AND e.to_symbol_id IS NULL AND e.to_name = $2
ORDER BY s.path, s.start_line, s.id
LIMIT $3
```

- [ ] **Step 3: Commit, then prove the tests discriminate**

**M1 — `c.depth < $3` → `c.depth <= $3`.**
- *Why the code exists:* the bound is the caller's, and an off-by-one on a graph traversal is a fan-out, not a row.
- *Fixture that separates mutant from original:* the four-deep chain, queried at `depth=2`. A chain shorter than the bound cannot see it.
- *Must fail:* `TestCallersOfStopsAtTheRequestedDepth`
- *Expected (verify and correct):* `depth=2 returned [a:1 b:2 main:3], want [a:1 b:2]`
- *Compiles and vets:* yes.

**M2 — the cycle guard replaced by `TRUE`.**
- *Why the code exists:* recursion is normal in a call graph, and without the guard a cycle re-enters.
- *Fixture that separates mutant from original:* the `d ──▶ e ──▶ d` cycle and the `target ──▶ target` self-call. **A DAG cannot see this at all** — the guard is dead code on a DAG, which is what makes it the easiest control in the phase to delete without noticing. The depth bound keeps the mutant terminating, so the failure is a wrong row set rather than a hang.
- *Must fail:* `TestCallersOfTerminatesOnATwoNodeCycle`
- *Expected (verify and correct):* `got [e:1 d:2 e:3 target:1 …], want [e:1 d:2 target:1 …]` — verify the exact duplication; `min(depth)` collapses some of it, which is worth knowing since it means the aggregation partially *masks* this mutant.
- *Compiles and vets:* yes.

**M3 — the depth bound replaced by `TRUE`.**
- *Why the code exists:* it bounds the work and it is the caller's parameter.
- *Fixture that separates mutant from original:* the same chain at `depth=1`. The cycle guard keeps it terminating, so this is a finite wrong answer.
- *Must fail:* `TestCallersOfStopsAtTheRequestedDepth`
- *Expected (verify and correct):* `depth=1 returned 6 callers, want 2`
- *Compiles and vets:* yes — and note that `$3` stays bound in the `SELECT` list or the parameter becomes unused, which is a pgx error and a void mutation. Spell it `AND ($3 IS NOT NULL OR TRUE)` if needed, and record which form was used.

**M4 — `min(c.depth)` → `max(c.depth)`.**
- *Why the code exists:* a caller reachable by two paths is one caller, at its shortest distance; the longer path is a fact about the graph, not about the caller.
- *Fixture that separates mutant from original:* the diamond, where `target` is reachable from `main` at depth 2 and depth 3. **A fixture with one path per caller cannot.**
- *Must fail:* `TestADiamondYieldsOneCallerAtItsShortestDepth`
- *Expected (verify and correct):* `main at depth 3, want depth 2`
- *Compiles and vets:* yes.

**M5 — the `GROUP BY` dropped (the aggregate becomes a plain select).**
- *Why the code exists:* the same caller reached twice is one row.
- *Fixture that separates mutant from original:* the diamond again, asserting the **count**. A test that checks membership passes with duplicates present.
- *Must fail:* `TestADiamondYieldsOneCallerAtItsShortestDepth`
- *Expected (verify and correct):* a SQL error, not a row change — `min(c.depth)` without a `GROUP BY` aggregates the whole result. **That makes the naive form void (rule 2).** Spell the mutation as "drop the aggregation *and* the `min`, selecting `c.depth` directly", which is the mutant a person would actually write, and which returns duplicate rows.
- *Compiles and vets:* only in the rewritten form. Record both.

**M6 — the recursive term joins `e.from_symbol_id = c.sym` (direction inverted).**
- *Why the code exists:* callers, not callees. The two queries are one character apart and both return plausible graphs.
- *Fixture that separates mutant from original:* the chain, which is **asymmetric**: `target` has callers and no callees in the fixture. A fixture where every node both calls and is called cannot separate them.
- *Must fail:* `TestCallersOfWalksTheChainAndReportsDepth`
- *Expected (verify and correct):* `got [target:2], want [b:1 a:2]` — verify; the anchor is unchanged, so depth 1 is right and only the deeper rows are wrong, which is the more dangerous shape.
- *Compiles and vets:* yes.

**M7 — the anchor drops `AND e.to_symbol_id = $2` in favour of `e.to_name = (SELECT name FROM symbols WHERE id = $2)`.**
- *Why the code exists:* spec:84 — a syntactic edge's name is not a target, and binding it at read time is the guess the null column exists to refuse.
- *Fixture that separates mutant from original:* the two `Get` methods plus the syntactic edge naming `Get`. **A fixture with unique names cannot see this**, which is rule 4 and the reason the fixture has two.
- *Must fail:* `TestCallersOfNeverIncludesASyntacticEdge`
- *Expected (verify and correct):* `callers of Store.Get include f, which calls something named Get with a null target`
- *Compiles and vets:* yes.

**M8 — `ApproximateCallersOf` drops `AND e.to_symbol_id IS NULL`.**
- *Why the code exists:* the approximate set is what is *not* known; a resolved edge in it double-counts a caller that is already in the precise answer.
- *Fixture that separates mutant from original:* the fixture's resolved edge to `Store.Get` alongside the syntactic one.
- *Must fail:* `TestApproximateCallersExcludeResolvedEdges`
- *Expected (verify and correct):* `approximate callers of Get: [f g], want [f]`
- *Compiles and vets:* yes.

**M9 — `Definitions` matches with `suffix` always on.**
- *Why the code exists:* an exact request that silently widens is a different question answered without saying so.
- *Fixture that separates mutant from original:* `Store.Get` and `Cache.Get`, queried as `Get` with `suffix=false`, which must return **nothing**.
- *Must fail:* `TestDefinitionsMatchesExactlyUnlessSuffixIsAsked`
- *Expected (verify and correct):* `exact match for "Get" returned 2 definitions, want 0`
- *Compiles and vets:* yes.

**M10 — `Definitions` drops the repo filter (`repo_id = $1 OR TRUE`).**
- *Why the code exists:* names collide across repositories by construction; `parseConfig` exists in every second Go repository.
- *Fixture that separates mutant from original:* the second repository's `target`, which the fixture gives **more** callers so the effect is a changed answer and not merely an extra row.
- *Must fail:* `TestDefinitionsIsScopedToOneRepo`
- *Expected (verify and correct):* `definitions of target: 2, want 1 (the extra is from repo B)`
- *Compiles and vets:* yes — the parameter stays bound.

**M11 — `CallersOf` drops the repo filter (`repo_id = $1 OR TRUE`) in both terms.**
- *Why the code exists:* defence in depth.
- *Fixture that separates mutant from original:* **none, and this is a predicted survivor.** `SymbolID = hash(repo, path, start, kind, name)`, so a symbol id never collides across repositories and `to_symbol_id = $2` already pins the repo; the recursive term joins on ids for the same reason. Record it as a survivor with that reasoning, and keep the clause — the day a symbol id becomes anything but a repo-scoped hash, the filter is what stops a cross-repo traversal, and the *reason* it cannot be killed today is the reason it is cheap to keep.
- *Must fail:* nothing. Survivor.
- *Compiles and vets:* yes.

**M12 — the final `ORDER BY` dropped.**
- *Why the code exists:* two runs over an unchanged corpus must be diffable, and the `LIMIT` makes the order decide *which* callers a caller sees.
- *Fixture that separates mutant from original:* the chain queried with a `LIMIT` **smaller than the result**, so the order decides membership rather than presentation. **P3 measured twice that an unordered result is not an insertion-ordered one** — it came back in path order once and in a stable arbitrary order another time — so the test asserts the disagreement between the expected order and path order in its body rather than assuming it.
- *Must fail:* `TestCallersOfWalksTheChainAndReportsDepth`
- *Expected (verify and correct):* paste the observed order; predicting it is what went wrong in P3's M5 and M9.
- *Compiles and vets:* yes.

**M13 — `RepoStats` counts edges without the provenance split (both counts from the same total).**
- *Why the code exists:* the split is the phase's headline claim and the repo view is where a user sees it.
- *Fixture that separates mutant from original:* the target repo, which has **both** kinds. A fixture with only resolved edges makes the two counts equal and the mutant invisible.
- *Must fail:* `TestRepoStatsSplitsEdgesByProvenance`
- *Expected (verify and correct):* `resolved 9, syntactic 9, want resolved 7, syntactic 2`
- *Compiles and vets:* yes.

- [ ] **Step 4: Commit**

**Definition of Done**
- `CallersOf` terminates on a self-call, on a two-node cycle and on a diamond, with the row set, the depths and the call sites all asserted.
- Each of the two termination controls is killed by a mutation while the other holds; removing both is recorded as excluded, not attempted.
- No syntactic edge reaches the traversal, proven against a fixture with two definitions sharing a last segment.
- The approximate set is name-matched, depth-1, separate, and excludes resolved edges.
- `Definitions` is exact by default, scoped to one repo, and says which match it made.
- `RepoStats` reports the provenance split from a repo carrying both.
- M1–M13 recorded with observed output; M5's void form and its rewrite recorded; M11 recorded as a survivor with its reason.

---

### Task 6: The graph endpoints

Spec §8 names `definition_of` and `callers_of` as tools for P7's loop. This task ships the HTTP beneath them, in the shape `getSpan` already established: `lookupRepo` for the 404/410 distinction, a repo-scoped store read, `ErrNotFound` → 404, anything else → `h.fail`, then `h.touch` and a JSON view.

**Files:**
- Create: `apps/gateway/internal/handler/graph.go`
- Modify: `apps/gateway/internal/handler/handler.go` (`Mount`, the `Reader` interface), `apps/gateway/internal/handler/read.go` (the repo view gains the graph counts), `apps/gateway/cmd/store.go`
- Test: `apps/gateway/internal/handler/graph_test.go`, additions to `read_live_test.go`

**Routes:**

| route | answers |
| --- | --- |
| `GET /api/repos/:repo/symbols?name=&pkg=&suffix=&limit=` | definitions matching a name, exact unless `suffix=true` |
| `GET /api/repos/:repo/symbols/:symbol` | one definition with its citation |
| `GET /api/repos/:repo/symbols/:symbol/callers?depth=&limit=` | the resolved callers, and separately the approximate ones |

**Decisions, with their reasoning:**

- **`GET`, not `POST`.** P3 put search and ask behind `POST` because a *question* is prose that must not reach a proxy's access log. A symbol name is an identifier — it is already in the URL of every permalink this product renders — so the rule does not extend here, and a `GET` is cacheable and linkable, which is what a console will want in P5. Recorded because the difference from P3 is deliberate and would otherwise look like an oversight.
- **A missing symbol is `404`, and there is no refusal on these routes.** §10 keeps refusal and error distinct; a graph query has a third outcome — "asked about something that is not here" — which is exactly what `404` means. So `metrics.CountAnswer` and `metrics.CountRefusal` must not be touched by any of these handlers, and the tests assert the whole set of series is unmoved.
- **An empty caller list is `200`, not `404`.** "Nothing calls this" is an answer. The symbol not existing is the `404`.
- **`depth` is bounded 1..5 and out of range is a `400` naming the rule**, not a silent clamp — the same decision P3 made for `limit`, for the same reason: a caller asking for depth 40 has misunderstood the endpoint and quietly serving 5 hides it. The default is **1**, because "who calls this" as a question means direct callers, and a default of 3 would make the common request expensive for a reason nobody asked for.
- **The approximate set is a sibling field with its own count, never merged**, and it carries `matched_on: "name"` so that no client has to know the convention to interpret it. This is the §6 invariant expressed at the API boundary; if a console flattens the two lists, it does so knowingly.
- **Every caller carries the call site and a citation.** A caller without `file:line` fails spec:5's one-sentence description of the product. The citation is built from the *span* the symbol links to, so it carries a digest; when `span_id` is null the response carries the location and **no permalink-backed citation**, and says so with `"citation": null` rather than inventing one. That is P3's rule for an unknown forge, applied to an unknown span.
- **The repo view gains `symbols`, `edges`, `edges_resolved`, `edges_syntactic`.** One place a reader can see how much of a repository's graph is precise. It is an aggregate over a per-row column, not a per-repo label.

- [ ] **Step 1: Write the failing tests**

Hermetic, against a fake `Reader` — and **every error path of that fake is set by a test in this list**, which is the second of P3's two survivor shapes:

```go
func TestDefinitionsReturnsExactMatchesAndSaysWhichMatchItMade(t *testing.T)
func TestSuffixMatchingIsOptInAndLabelled(t *testing.T)
func TestAnUnknownSymbolIsFourOhFour(t *testing.T)
func TestASymbolWithNoCallersIsTwoHundredWithAnEmptyList(t *testing.T)
func TestCallersCarryTheirCallSiteAndACitation(t *testing.T)
func TestASymbolWithNoSpanCarriesANullCitationRatherThanAGuess(t *testing.T)
func TestApproximateCallersAreASeparateFieldWithTheirOwnCount(t *testing.T)
func TestDepthOutsideOneToFiveIsFourHundredNamingTheRule(t *testing.T)
func TestLimitOutsideOneToFiftyIsFourHundredNamingTheRule(t *testing.T)
func TestAnEmptyNameIsFourHundredNamingTheRule(t *testing.T)
func TestAnUnknownRepoIsFourOhFourAndAnEvictedOneIsFourTen(t *testing.T)
func TestAGraphReadWindsTheLRUClock(t *testing.T)
func TestATouchFailureIsLoggedAndNotReturned(t *testing.T)
func TestAStoreErrorIsFiveHundredWithARequestIdAndNoDetail(t *testing.T)
func TestTheGraphRoutesTouchNoAnswerOrRefusalCounter(t *testing.T)
```

- [ ] **Step 2: Implement**

Shapes, fixed here rather than discovered:

```
GET …/symbols        → {"repo_id":…,"count":n,"matched":"exact"|"suffix",
                        "symbols":[{"id","name","pkg","kind","path","start_line","end_line","span_id"}]}
GET …/symbols/:id    → {"symbol":{…},"citation":{…}|null,"staleness":{…}}
GET …/symbols/:id/callers
                     → {"repo_id":…,"symbol":{…},"depth":d,
                        "callers":[{"symbol":{…},"depth":1,"provenance":"resolved",
                                    "call":{"path":"x.go","line":42},"citation":{…}|null}],
                        "approximate":{"matched_on":"name","count":n,
                                       "callers":[{"symbol":{…},"to_name":"Get",
                                                   "provenance":"syntactic","call":{…}}]}}
```

- [ ] **Step 3: Commit, then prove the tests discriminate**

**M1 — an unknown symbol answers `200` with an empty list.**
- *Why the code exists:* §10 — an unknown thing is a `404`, and an empty answer for something that does not exist is indistinguishable from "nothing calls it".
- *Fixture that separates mutant from original:* the fake's `ErrNotFound`, and separately a **real symbol with no callers**. Both are needed: with only the first, a `404`-for-everything mutant also passes.
- *Must fail:* `TestAnUnknownSymbolIsFourOhFour`
- *Expected (verify and correct):* `status 200, want 404`
- *Compiles and vets:* yes.

**M2 — `lookupRepo`'s order inverted, so an evicted repo answers `404`.**
- *Why the code exists:* §10 — "an evicted repo answers `410 Gone`, not `404` — it existed, and that is a different fact".
- *Fixture that separates mutant from original:* the tombstoned repo id in the fake. P3's `read_test.go` already has one; reuse it rather than building a second.
- *Must fail:* `TestAnUnknownRepoIsFourOhFourAndAnEvictedOneIsFourTen`
- *Expected (verify and correct):* `evicted repo answered 404, want 410`
- *Compiles and vets:* yes.

**M3 — `depth` clamped to the range instead of refused.**
- *Why the code exists:* the same rule P3 fixed for `limit`; a clamp hides a caller's misunderstanding.
- *Fixture that separates mutant from original:* `depth=40` and `depth=0`. Only the second separates a clamp-to-max from a clamp-to-min, and a mutant might do either.
- *Must fail:* `TestDepthOutsideOneToFiveIsFourHundredNamingTheRule`
- *Expected (verify and correct):* `depth=40 answered 200 with 5 levels, want 400`
- *Compiles and vets:* yes.

**M4 — the `400` names a different rule (`"q must not be empty"` in place of the depth detail).**
- *Why the code exists:* §10 — "naming **which rule** failed, never a generic refusal".
- *Fixture that separates mutant from original:* the depth case, asserting the response's `error` **string** and `rule` field. A test asserting only the status code passes.
- *Must fail:* `TestDepthOutsideOneToFiveIsFourHundredNamingTheRule`
- *Expected (verify and correct):* `error "q must not be empty", want it to name depth`
- *Compiles and vets:* yes.

**M5 — the approximate callers appended to `callers`.**
- *Why the code exists:* spec:84 and §6. This is the mutation the whole "what §6 decides" section was written for, expressed at the boundary a client sees.
- *Fixture that separates mutant from original:* a fake returning one resolved caller and two approximate ones with a `provenance` a client could otherwise use to tell them apart — so the assertion is on the **field they arrive in**, not on the label they carry.
- *Must fail:* `TestApproximateCallersAreASeparateFieldWithTheirOwnCount`
- *Expected (verify and correct):* `callers has 3 entries and approximate.count is 0, want 1 and 2`
- *Compiles and vets:* yes.

**M6 — `provenance` dropped from the caller view.**
- *Why the code exists:* it is the per-row label §6 exists to make visible; without it the API has the graph and not the honesty.
- *Fixture that separates mutant from original:* any caller list, asserting the field is present and equal to `"resolved"`.
- *Must fail:* `TestCallersCarryTheirCallSiteAndACitation`
- *Expected (verify and correct):* `caller view has no "provenance" field`
- *Compiles and vets:* yes — an unused struct field does not break the build, and `go vet` does not object either. **Verify:** if the field is removed rather than left unmarshalled, its assignment is a build break and the mutation must be spelled as `json:"-"`.

**M7 — a `nil` span link renders a citation with an empty digest.**
- *Why the code exists:* a digest is a claim about text; a citation with an empty one is a claim that cannot be checked, presented as one that can. P3 refused to render a permalink for an unknown forge for the same reason.
- *Fixture that separates mutant from original:* a symbol with a null `span_id` — which is the sub-windowed-declaration case from Task 2 and is otherwise easy to have no fixture for.
- *Must fail:* `TestASymbolWithNoSpanCarriesANullCitationRatherThanAGuess`
- *Expected (verify and correct):* `citation rendered with digest "", want null`
- *Compiles and vets:* yes.

**M8 — `metrics.CountAnswer("answered")` added to the callers handler.**
- *Why the code exists:* §10's answer counters describe the ask path; a graph read is neither an answer nor a refusal, and mixing them would make the answer-outcome ratio meaningless the moment a console starts walking the graph.
- *Fixture that separates mutant from original:* `TestTheGraphRoutesTouchNoAnswerOrRefusalCounter`, which reads **every** series of both vectors before and after. A test that checks one series passes.
- *Must fail:* `TestTheGraphRoutesTouchNoAnswerOrRefusalCounter`
- *Expected (verify and correct):* `codetrail_answer_total{outcome="answered"} moved by 1, want the whole vector unchanged`
- *Compiles and vets:* yes.

**M9 — `h.touch` removed from the graph handlers.**
- *Why the code exists:* `last_queried_at` is the LRU clock, and a repository whose only traffic is graph queries would be evicted as cold while in use.
- *Fixture that separates mutant from original:* `TestAGraphReadWindsTheLRUClock`, asserting the fake's `TouchRepo` was called **with the repo id**. Asserting a call count alone passes under a mutant that touches the wrong repo.
- *Must fail:* `TestAGraphReadWindsTheLRUClock`
- *Expected (verify and correct):* `TouchRepo called 0 times, want once with "abc123"`
- *Compiles and vets:* yes.

**M10 — a `TouchRepo` failure returned to the caller.**
- *Why the code exists:* the touch is a hint for a future eviction, not part of the answer; failing a correct read because a bookkeeping write failed trades a good answer for a `500`.
- *Fixture that separates mutant from original:* the fake's `TouchRepo` error, **which is a fake error path and therefore in the ledger**: `TestATouchFailureIsLoggedAndNotReturned` is the test that sets it, and without that test M10 survives.
- *Must fail:* `TestATouchFailureIsLoggedAndNotReturned`
- *Expected (verify and correct):* `status 500, want 200 with the callers`
- *Compiles and vets:* yes.

**M11 — `h.fail` replaced by returning the store error to the caller.**
- *Why the code exists:* §10 — a `500` is opaque with a request id; a store error names tables, columns and sometimes values.
- *Fixture that separates mutant from original:* the fake's generic error, asserting the body **contains a request id and does not contain the error's text**. Asserting the status alone passes.
- *Must fail:* `TestAStoreErrorIsFiveHundredWithARequestIdAndNoDetail`
- *Expected (verify and correct):* `body was {"error":"graph: relation \"edges\" does not exist"}, want the opaque form`
- *Compiles and vets:* yes.

**M12 — `suffix` ignored, so matching is always exact.**
- *Why the code exists:* the opt-in is what makes exact matching safe to default to.
- *Fixture that separates mutant from original:* the two-`Get` fixture with `suffix=true`, asserting both rows come back **and** that `matched` says `suffix`.
- *Must fail:* `TestSuffixMatchingIsOptInAndLabelled`
- *Expected (verify and correct):* `suffix=true returned 0 symbols and matched "exact", want 2 and "suffix"`
- *Compiles and vets:* yes.

- [ ] **Step 4: Commit**

**Definition of Done**
- Three routes, mounted in the existing group, with `lookupRepo`'s 404/410 distinction and `h.touch` on every successful read.
- The approximate set is a separate field with its own count and `matched_on`.
- Every caller carries `provenance`, a call site, and either a real citation or `null`.
- `depth`, `limit` and `name` are validated at the edge, each `400` naming its own rule.
- The answer and refusal counters are untouched by these routes, asserted over the whole series set.
- M1–M12 recorded with observed output.

---

### Task 7: End to end on a live database, the whole-branch sweep, and the README

**Files:**
- Modify: `README.md`, `.github/workflows/ci.yml` (if the fixture modules need a step), `apps/indexer/cmd/testdata/repo/…`
- Test: `apps/gateway/internal/handler/graph_live_test.go`, additions to `apps/indexer/cmd/index_live_test.go`

- [ ] **Step 1: Index a real repository and record what the graph actually looks like**

Index `rs/zerolog` exactly as P2 and P3's READMEs record it, then run the three endpoints and **paste the numbers into the README and into this plan**:

```
symbols, edges, resolved, syntactic, external, unnameable
packages attempted, packages loaded, packages failed, reason
```

**Do not predict these numbers.** `rs/zerolog` requires third-party modules, so under `GOPROXY=off` some of its packages will not load and some will — which is the per-edge claim demonstrated on a real repository rather than a fixture, whatever the split turns out to be. If it turns out that *no* package loads, that is the honest headline and the README says it: the default policy buys precision only for standard-library-only packages, and an operator who wants more turns the proxy on and accepts the egress. If it turns out that *every* package loads, check why before believing it.

Then verify a citation the graph returned, the way P2 verified 1,303 spans: take a caller's `call.path` and `call.line`, `git show <sha>:<path>`, and confirm the line holds the call. A graph that cites a line nothing calls from is worse than no graph.

- [ ] **Step 2: The end-to-end live tests**

```go
func TestTheGraphAndTheSpansAgreeOnEverySymbolNameLive(t *testing.T)
func TestAResolvedEdgeAndASyntacticEdgeCoexistInOneRepoLive(t *testing.T)
func TestWhoCallsThisAnswersOverTheApiWithACheckableCitationLive(t *testing.T)
func TestReindexingTheSameCommitConvergesLive(t *testing.T)
func TestTheEdgeCountIsTheSameWithAndWithoutTypecheckingLive(t *testing.T)
func TestAnEvictedRepoTakesItsGraphAndAnswersFourTenLive(t *testing.T)
```

`TestTheGraphAndTheSpansAgreeOnEverySymbolNameLive` is the identity assertion this phase owes the rest of the system: for every span in the fixture repo with a non-empty `symbol`, there is a `symbols` row in the same repo with an equal `name`. It is the only test that can catch the two-spellings failure after Task 1's shared function is bypassed by someone in a hurry.

`TestTheEdgeCountIsTheSameWithAndWithoutTypecheckingLive` indexes the same fixture twice, once with `TYPECHECK=false`, and asserts the two runs produce **the same edge ids** and differ only in `provenance` and `to_symbol_id`. That is the per-edge architecture stated as a property.

- [ ] **Step 3: The two ledgers**

P3's per-task rounds each reported no survivors and the whole-branch sweep found about twenty, nearly all of one of two shapes. Fill both tables before running the sweep; a row with no test named against it is the sweep's first finding.

**Side effects, and what reads each one back:**

| side effect | read back by |
| --- | --- |
| `symbols` rows written | `TestPutGraphWritesSymbolsAndEdges`, `TestTheGraphAndTheSpansAgreeOnEverySymbolNameLive` |
| `edges` rows written | `TestPutGraphWritesSymbolsAndEdges`, `TestTheEdgeCountIsTheSameWithAndWithoutTypecheckingLive` |
| `symbols.span_id` link | `TestASymbolLinksToTheSpanThatContainsIt`, `TestASymbolLinksToTheMostSpecificOfTwoOverlappingWindows` |
| the provenance label per row | `TestOnlyTheCallSitesTheResolverNamedAreResolved`, `TestAResolvedEdgeAndASyntacticEdgeCoexistInOneRepoLive` |
| `codetrail_graph_edges_total{provenance}` | `TestTheGraphCountersMoveExactlyOnce` |
| `codetrail_typecheck_total{reason}` | `TestTheGraphCountersMoveExactlyOnce`, the no-toolchain subtest |
| `codetrail_typecheck_seconds` | **name a test or delete the histogram** |
| the per-job graph log line | `TestTheJobLogNamesWhatTheGraphStageProducedAndWhyItStopped` |
| the boot line when no `go` binary is found | `TestTheJobLogNamesWhatTheGraphStageProducedAndWhyItStopped`'s no-toolchain subtest |
| `unnameable` count | same |
| `Stats.External` | `TestACallIntoTheStandardLibraryIsExternalNotResolved` |
| `last_queried_at` wound by a graph read | `TestAGraphReadWindsTheLRUClock` |
| `repos` view's graph counts | `TestRepoStatsSplitsEdgesByProvenance` |
| the go child's environment | `TestTheLoaderNeverReachesTheModuleProxy`, `TestTheOperatorsGoflagsCannotBreakTheLoad` |
| files written under `GOCACHE`/`GOMODCACHE` | **nothing reads this back — see Open question 10** |

**Fakes, and what sets each one's error:**

| fake | error path set by |
| --- | --- |
| the substituted resolver (indexer) | `TestAResolverFailureLeavesEveryEdgeSyntacticAndTheJobIntact` |
| the substituted resolver, panicking | `TestAResolverFailureLeavesEveryEdgeSyntacticAndTheJobIntact`'s disabled subtest |
| `putGraph` on the worker | **name a test that makes it fail, or M-sweep-3 will find it** |
| fake `Reader.Definitions` | `TestAStoreErrorIsFiveHundredWithARequestIdAndNoDetail` |
| fake `Reader.Symbol` | `TestAnUnknownSymbolIsFourOhFour` (ErrNotFound) and the generic-error test |
| fake `Reader.CallersOf` | the generic-error test |
| fake `Reader.ApproximateCallersOf` | **name a test — an error here must not fail a request whose precise half succeeded, or must, and the plan has not decided which** |
| fake `Reader.TouchRepo` | `TestATouchFailureIsLoggedAndNotReturned` |
| fake `Reader.RepoGone` | P3's existing test; verify it still runs on the graph routes |

The one open decision the ledger surfaces: **what a failure of the approximate query alone should do.** Recommendation — return the precise answer with `"approximate": {"error": true, "count": 0}`, because the precise half is the part the caller asked about and the approximate half is a courtesy. Decide it in Task 6 and put the test in the ledger.

- [ ] **Step 4: The whole-branch mutation sweep**

Run **after every task is merged**, against the branch as a whole, with `git status --porcelain` empty. P3's evidence says this round finds what per-task rounds cannot; budget for it rather than treating it as a formality.

Ten to start with, all cross-task:

1. `chunk.Decl`'s receiver dropped — must fail tests in **both** `chunk` and `symbols` (Task 1's M1 and M8 together: this is the sweep that proves the refactor bought something).
2. The provenance label taken from `Stats.Reason` in the indexer — must fail Task 4's hermetic test **and** the end-to-end coexistence test.
3. `putGraph` errors swallowed in the indexer — the job completes with no graph and nothing says so.
4. `GOPROXY=off` dropped — must fail Task 3's recorder test; check whether any live test also changes.
5. The type-check given a fresh context — must fail the live deadline test.
6. `CallersOf`'s cycle guard dropped — must fail the cycle tests; check the end-to-end test is unaffected (it should be, on a real corpus with cycles it may not be, and that is worth knowing).
7. The approximate set merged into the callers list — must fail the handler test **and** should be visible end to end.
8. `PutGraph`'s repo scope defeated in both deletes — must fail the two-repo test; check what the end-to-end test shows.
9. `symbols.span_id` linked by exact range — must fail the long-declaration test.
10. `EdgeID` keyed by line — must fail Task 1's, Task 2's and Task 3's offset tests, which is three layers of the same trap and the check that all three fixtures actually exist.

- [ ] **Step 5: The README**

New section, in the register the existing README uses. It must say, in these terms:

- The graph is **calls only** in P4. `imports` and `references` are in the schema's enum and nothing writes them; why is in Open question 7.
- **Type-checking is off by default in the sense that matters:** `GOPROXY=off`, so only packages whose imports resolve without a module download type-check. State the measured split for `rs/zerolog`.
- **The indexer needs a `go` binary on `PATH`** to resolve anything. Without one, every edge is syntactic and the boot log says so. There is no container image yet (P8), and the image P8 builds must include a toolchain or production will be all-syntactic while CI is not.
- **An operator can turn module fetching on** with `TYPECHECK_GOPROXY`, and what that buys and costs: better resolution, and outbound requests to hosts chosen by a stranger's `go.mod`, which is the thing P1's allowlist otherwise prevents. `direct` is refused.
- **A syntactic edge is not a worse resolved edge; it is a different claim.** It names something and does not say which one. The API keeps the two apart and so should any client.
- **A call into the standard library resolves and is still recorded as syntactic**, because there is no symbol row to point at. This is the one place the spec's two-value label loses information, and the counter keeps it.
- The numbers a run prints are **not quality** — the same banner the eval carries.

- [ ] **Step 6: Commit**

**Definition of Done**
- A real repository indexed, its provenance split measured and recorded, and one of its call-site citations verified against `git show`.
- Span symbols and graph symbols agree on every name in a live corpus.
- Re-indexing the same commit converges; indexing with and without type-checking produces the same edge ids.
- Both ledgers complete, with no row lacking a named test.
- The branch-wide sweep run and every finding recorded — including the ones that survive.
- The README says what is measured, what is off by default, and what a syntactic edge is.

---

## Definition of done for P4

- [ ] Definitions come from the AST, spelled by the same function that spells a span's symbol, with a live assertion that the two agree across a whole corpus.
- [ ] Edges are attempted through `go/packages` with type information; where `types.Info.Uses` resolves a call to an object **that has a symbol row in this repository**, the edge is `resolved` and points at it; everywhere else it is `syntactic` with a null target, enforced by a CHECK constraint rather than by convention.
- [ ] Provenance is per row. One repository carries both labels, and a fixture with two packages — one loadable, one not — pins it by naming an edge in each. No line of code copies a package-level fact onto a row.
- [ ] The type-check runs inside the sandbox, on the job's own remaining budget, with an environment allowlist that a hostile parent environment cannot widen and that makes a recording proxy receive zero requests.
- [ ] A type-check failure — missing toolchain, absent module, expired budget, disabled by knob — produces a complete graph of syntactic edges, a distinct counted reason, a log line, and a `done` job.
- [ ] "Who calls this" is a recursive CTE that terminates on self-calls, cycles and diamonds, reports depth and call sites, and never traverses a name.
- [ ] Approximate, name-matched callers are a separate labelled set at depth 1, with their own count.
- [ ] Graph endpoints answer `404` for an unknown symbol, `410` for an evicted repo, `400` naming the rule for a bad `depth`, `limit` or `name`, and `500` opaquely with a request id; none of them touch the answer or refusal counters.
- [ ] Eviction takes the graph with it, in one `DELETE`.
- [ ] Every mutation recorded with observed output; every survivor recorded as a survivor with what it revealed; the whole-branch sweep run after the last merge.

**Not in P4, deliberately:** no `imports` or `references` edges (Open question 7). No LLM tool loop — §8's `definition_of` and `callers_of` are P7's tools and this phase ships the endpoints beneath them. No console (P5). No eval harness (P6); nothing in this phase touches the two-arm corpus design. No incremental re-index (P7): a new commit is a new repo id and a full graph. No second language (§7). No `/metrics` on the indexer, so this phase's four instruments join P3's three with nowhere to be scraped from.

---

## Open questions

Nothing here is blocking. Each is something the spec does not settle, with the tradeoff and a recommendation, so a reviewer can disagree with a decision rather than discover it.

1. **How deep "who calls this" goes.** §8 names `callers_of` and gives no depth.
   *Recommendation:* a `depth` parameter, **default 1, maximum 5**, out of range refused with a `400`. Direct callers are what the question usually means; the maximum exists because fan-out in a call graph is multiplicative and an unbounded traversal on a real repository is a query that returns a repository. Five is not measured — it is a bound chosen to be obviously finite, and P6/P7 can raise it with a corpus in front of them. The `LIMIT` is the second bound and the response says when it truncated.

2. **What happens to edges when a repo is re-indexed at a new commit.** §3 makes `repo = hash(remote, commit)`, so nothing needs to happen: a new commit is a new repository row with its own graph, and the old one is removed by LRU eviction with its files, spans, symbols and edges.
   *Recommendation:* keep exactly that, and do not add a "same repository, newer commit" link in P4. The alternative — graph diffing across commits — is a feature (what changed about who calls this) and belongs with incremental re-index in P7. What P4 does owe is convergence *within* one repo id, which `PutGraph`'s wholesale replace and deterministic ids provide.

3. **Whether definitions and spans share identity.** §3 gives `symbols` a `span_id` and no line range, implying the span is the definition's location.
   *Recommendation:* **they do not share identity.** Symbols carry their own `start_line`/`end_line` and link to a span by containment, nullable. Three reasons: a declaration longer than `MaxDeclLines` has no span with its range, so an identity-sharing design loses exactly the biggest declarations; span ids are `hash(repo, path, start, end, digest)` and therefore change when a body changes, which would make a symbol's identity change with its implementation; and the eval's window arm chunks the same file differently, so a shared identity would make the graph strategy-dependent. The cost is a deviation from §3's column list, recorded in the migration's comment.

4. **What `go/packages` may reach.** §6 says type-checking "requires module downloads" and says nothing about from where — while §4's whole design is an allowlist of hosts, because the alternative is an SSRF.
   *Recommendation:* **`GOPROXY=off` by default**, with `GOVCS=*:off`, `GOTOOLCHAIN=local`, `GOWORK=off`, `GOENV=off`, `CGO_ENABLED=0`, scratch-local `GOMODCACHE`/`GOCACHE`/`GOPATH`/`GOTMPDIR`, and an environment allowlist rather than an inherited environment. An operator can set `TYPECHECK_GOPROXY` to a proxy they trust; `direct` and any `,direct` fallback are refused, because that is the setting that turns a stranger's `require` line into an outbound `git` to a host of their choosing. The cost is stated plainly: with the default, only standard-library-only packages type-check, and everything else is honestly syntactic.

5. **What `pkg` means.** §3 lists the column and never defines it.
   *Recommendation:* the **package clause name** from the AST (`store`), not the import path. The import path needs a module-aware load, which is the thing that is allowed to fail; a column whose meaning depended on whether the type-check ran would be a per-package fact leaking into a per-row column, which is the failure §6 is written against. Two packages named `store` in one repository are distinguished by `path`. Revisit if a console needs import paths.

6. **`edges` and `symbols` carry columns §3 does not list** — `path` and `line` on an edge, `start_line` and `end_line` on a symbol.
   *Recommendation:* add them, and say so. Without the edge's location, "who calls this" answers with a symbol and no `file:line`, in a product whose first sentence is that its answers cite `file:line`. Without the symbol's range, a definition with no span is uncitable. The alternative — deriving both by joining spans — fails for exactly the rows where the link is null.

7. **`imports` and `references` edges.** §3's `kind` enum has three values; §6 describes only calls.
   *Recommendation:* **calls only in P4**, with the other two values valid and unwritten. `imports` cannot be written under the current schema at all: `from_symbol_id` is `NOT NULL` and an import belongs to a *file*, not to a definition, so writing one means either a nullable tail or a synthetic file-level row in a table §3 defines as "one row per definition". Both are schema changes with consequences for every query in Task 5, and neither is needed to answer "who calls this". `references` is a volume decision — it is roughly every identifier use in the corpus — and should be made with a measured row count, not before.

8. **A call that type-checks to something outside the corpus.** `fmt.Println` resolves to a real object; there is no symbol row to point at.
   *Recommendation:* record it as `syntactic` with a null target, because spec:84's invariant ("a null target is what makes the label mean anything") is the one that cannot be bent, and count it separately as `external` so the fact is not lost. A third enum value would be more informative and §3's enum has two. Revisit if a console wants to distinguish "we could not tell" from "it is not in this repository" — the counter already knows.

9. **The indexer needs a Go toolchain at runtime.** It already forks `git`; this adds `go`, which is a much larger dependency.
   *Recommendation:* probe with `exec.LookPath` at boot, log one line when it is absent, count `no_toolchain` per job, and **do not refuse to boot** — an indexer that cannot type-check still indexes, and refusing would make a missing toolchain worse than a syntactic graph. P8's image must include a toolchain, and the README says so, because the failure mode otherwise is CI resolving and production not, which is the silent downgrade §8 calls the failure that costs a week.

10. **`go/packages` gives no hook to put its subprocess in a process group, and its cache is not covered by any cap.** P1 kills the whole process group on deadline; `packages.Load` runs `go list` through its own `exec.Command` and the plan cannot reach it. Separately, `MAX_REPO_BYTES` is measured on the clone, and `GOCACHE`/`GOMODCACHE` are written afterwards.
    *Recommendation:* accept both for P4 and write them down. The blast radius is bounded in practice — the caches live under the job's scratch directory, which `runJob` removes and which the worker sweeps at boot and exit — but "bounded in practice" is not "enforced", and the honest form is a README line plus the `TYPECHECK=false` kill switch. Revisit by measuring cache growth over a real corpus; if it matters, the fix is a `du` check after the stage, matching the clone's, or a driver we fork ourselves.

11. **Last-segment matching for `definition_of`.** P3's lexical arm reaches `Store.Get` through its parts, so a user who found a method by searching will type `Get`.
    *Recommendation:* exact by default, `suffix=true` to opt in, and the response says which match it made. A silent fallback would answer a different question than the one asked and would be indistinguishable in the payload. Revisit in P5 when the console knows what a user actually types.

12. **Where the graph pass gets its bytes.** The chunker may read *stripped* source; `go/packages` reads from disk.
    *Recommendation:* both, unchanged, with no `ParseFile` hook — because `StripDocs` blanks in place and preserves every byte offset and line number, so the two streams share a coordinate system. This is only true because of how stripping is implemented, so Task 1 pins it with a test and Task 3's comment names that test. If stripping ever deletes bytes, resolution silently collapses.

13. **Whether the repo view should carry graph counts.** §3 says nothing.
    *Recommendation:* yes — `symbols`, `edges`, `edges_resolved`, `edges_syntactic` on `GET /api/repos/:repo`. It is the only place a user can see how much of a repository's graph is precise, and it is an aggregate over a per-row column rather than a per-repo label. The risk to watch is a client treating the aggregate as the label, which is why the per-edge field exists in every caller row too.

14. **What a failure of the approximate query alone should do.** Surfaced by the fakes ledger in Task 7.
    *Recommendation:* return the precise answer with the approximate block marked as failed, rather than failing the whole request. The precise half is what was asked for. Decide it in Task 6 and give it a test, or M-sweep-3 finds it.

---

## What the spec leaves underspecified or in tension

Recorded so a reviewer sees them as findings rather than as choices made quietly.

- **§6's resolution rule and §3's schema disagree about a call into the standard library.** §6 says the edge is `resolved` "where `types.Info.Uses` resolves a call to a real object"; §3 says a resolved edge "points at a symbol row"; spec:84 says a null target is what makes the label mean anything. `fmt.Println` satisfies the first and cannot satisfy the second. This plan resolves it toward the invariant — null target, `syntactic`, counted as `external` — and enforces the pair with a CHECK constraint. A reader of §6 alone would ship the opposite and would have no column left to record what happened.
- **§3's `symbols` row cannot cite a definition.** It has a `span_id` and no line range, and P2's `MaxDeclLines` sub-windowing means the largest declarations have no span of their own. Taken literally, the symbol table is uncitable for exactly the declarations most worth asking about. Task 2 adds the range and records the deviation.
- **§3's `edges` row cannot cite a call site.** No path, no line. "Who calls this" would answer with a symbol name in a product whose first sentence promises `file:line`. Task 2 adds them.
- **§3's `edges.kind` enum names `imports`, which the same row's `NOT NULL from_symbol_id` makes unwritable.** An import belongs to a file, not a definition, and `symbols` is defined as "one row per definition". One of the two has to give; P4 writes neither kind and says so rather than inventing a file-level pseudo-symbol.
- **§6 says type-checking "requires module downloads" and §4 says the sandbox makes no network assumptions and admits only allowlisted hosts.** These are in direct tension, and §6 does not resolve it: the hosts a module download reaches are chosen by a stranger's `go.mod`, which is the exact shape of the SSRF §4's allowlist exists to refuse. This plan resolves it by defaulting the fetch off and making the resulting failure a first-class, counted outcome — which §6 already blesses. It is worth noticing that §6's own sentence ("its failure is expected rather than exceptional") is what makes the safe choice affordable.
- **"It runs inside the same sandbox" describes a sandbox that is git-shaped.** P1's controls are `GIT_*` variables, a process group and a wall-clock deadline; none of them constrain a compiler. Running `go` on a stranger's tree adds cgo directives, `go.work` files, toolchain directives and a build cache to the threat surface, and each needs its own control. §6 assumes a sandbox that is more general than the one that exists.
- **§11's counters still have nowhere to live in the indexer.** P3 recorded this; P4 adds four more instruments to the same gap. The indexer has no HTTP server, so job outcomes, admission rejections, evictions and now type-check outcomes are written and unscrapable. Giving the indexer a probe server is P0-shaped work and belongs in its own task rather than being smuggled in here.
- **§8's tool list implies a graph API shape that §3's data model does not describe.** `definition_of` and `callers_of` are named as *tools*, with no routes, no parameters and no notion of depth or of what happens to a name that matches two definitions. Everything about the endpoint surface in Task 6 is a decision this plan made, and the open questions say which.
- **§13 puts the console in P5 and the LLM loop in P7, so P4's endpoints have no consumer for two phases.** That is a real risk of designing the wrong API: the response shapes here are guesses about what a console needs. They are kept close to P3's existing views to reduce the guess, and Open question 11 and 13 name the two places P5 is most likely to want a change.
- **`Evict`'s doc comment already promises the cascade this phase must join** — "whatever P3's symbol graph adds by declaring the same reference" — and it names the wrong phase. The symbol graph is P4. Fix the comment in Task 2's commit; P2's review round found twelve comments that had drifted, two of them introduced by commits fixing others.
