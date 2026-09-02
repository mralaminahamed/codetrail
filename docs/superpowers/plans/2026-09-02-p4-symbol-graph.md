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
- **P1's sandbox is deliberately hostile** and this phase runs a compiler inside it. That is Task 3, and it is the sharpest new risk in the project since P1's clone. *(Landed. The sandbox that resulted is an environment allowlist rather than P1's append-to-`os.Environ`, because git's dangerous settings are the four `clone.Run` sets last and go's are a dozen it would inherit.)*

---

## Task independence

- **Tasks 1, 2 and 5 are independent** and can be worked in parallel from `trunk`.
  - Task 1 creates `packages/shared/symbols` (AST only, no database, no `go/packages`) and moves one function in `chunk`.
  - Task 2 creates the migration, the models and `store.PutGraph`.
  - Task 5 creates `packages/shared/store/graph_read.go` — the reads. (Task 5's own file list says `graph_read.go` and this line said `graph.go`, which is Task 2's writer; corrected once Task 2 landed and took that name.) It depends on Task 2's *schema* but not on its writer; work it against the migration once Task 2's migration file lands, or write the migration in whichever task lands first and rebase the other onto it. *(Landed.)*
- **Task 3** (the sandboxed type-check) depends on Task 1 for the call-site keys.
- **Task 4** (indexer wiring) depends on 1, 2 and 3. *(Landed.)*
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

- [x] **Step 1: Verify the DDL against the pinned image before writing the migration**

Run against `pgvector/pgvector:pg17` before the migration was written, and the output is in the commit message. Two of the four probes behaved as the plan said and **the three-valued-logic check did not**:

```
INSERT INTO e VALUES (NULL, 'resolved');   ERROR: violates check constraint "pair" (null, resolved)
INSERT INTO e VALUES ('x',  'syntactic');  ERROR: violates check constraint "pair" (x, syntactic)
INSERT INTO e VALUES (NULL, 'syntactic');  INSERT 0 1
INSERT INTO e VALUES ('x',  'resolved');   INSERT 0 1
INSERT INTO e VALUES (NULL, NULL);         INSERT 0 1   <- accepted
INSERT INTO e VALUES ('x',  NULL);         INSERT 0 1   <- accepted
```

> **PLAN DEFECT — the hole is real, and it is on the side the plan did not look at.** The plan reasoned that "the constraint's left side is `to_symbol_id IS NOT NULL`, which is never NULL, so there is no three-valued-logic hole". The left side is indeed never NULL. The **right** side is: `provenance = 'resolved'` evaluates to NULL when `provenance` is NULL, the equality is then NULL, and a CHECK evaluating to NULL passes. Both `(NULL, NULL)` and `('x', NULL)` were accepted. `provenance TEXT NOT NULL` is what closes it — verified the same way, both rows then refused with `null value in column "provenance" ... violates not-null constraint` — and `TestTheSchemaRefusesAnEdgeWithNoProvenanceLive` plus M14 pin it. The enum values are a CHECK too (`kind IN (…)`, `provenance IN (…)`), because the pair constraint alone accepts `provenance = 'guessed'` beside a null target.

Also measured, each one a comment in the migration only because the counterfactual was run first:

- **The lock claim is true.** Reading `pg_locks` inside the migration's own transaction: each FK-bearing `CREATE TABLE` takes `ShareRowExclusiveLock` (plus `AccessShareLock`) on `repos`, `files` and `spans`.
- **`ON DELETE SET NULL` on `to_symbol_id` would fail the delete, not orphan it:** `ERROR: new row for relation "eg" violates check constraint "pair" … CONTEXT: SQL statement "UPDATE ONLY … SET "to_symbol_id" = NULL …"`. Cascade is the only option consistent with the invariant, as the plan said, and now for a reason that was executed.
- **The partial index is used:** `Index Only Scan using edges_approx_idx … Index Cond: ((repo_id = 'r1') AND (to_name = 'Get'))` — with `enable_seqscan = off`, since the probe table held two rows.
- **One `DELETE FROM repos` leaves 0 symbols and 0 edges.**

- [x] **Step 2: Write the failing live tests**

As listed, plus four the mutation round earned: `TestADuplicateCallSiteInOneWriteKeepsTheResolvedLabelLive` and `TestADuplicateSymbolInOneWriteKeepsTheLastRowLive` (the only fixtures that reach `ON CONFLICT` at all — see M1), `TestTheSchemaRefusesAnEdgeWithNoProvenanceLive` (the hole above), and `TestTheSchemaRefusesAKindOrProvenanceOutsideTheSetLive` (§3's two closed sets). `TestMigration0010AppliesToAPopulatedDatabaseTwiceLive` follows P3's shape: drop both tables, seed real `repos`/`files`/`spans` rows, run the body twice, and write graph rows *between* the runs so a migration that dropped and re-created its tables would erase them while still applying cleanly. `store_live_test.go`'s `TestSchemaShapeLive` grows both tables' column lists.

- [x] **Step 3: Implement**

`0010_symbol_graph.sql`, `packages/shared/store/graph.go`, and `models.Symbol`/`models.Edge`/`Provenance`/`EdgeKind`. `Evict`'s doc comment said "whatever P3's symbol graph adds" and `store.go`'s package doc said "(from P3) the symbol graph"; the symbol graph is P4 and both were corrected in the same commit.

> **A third deviation from §3, which Open question 6 does not list: `symbols.path`.** It is denormalised beside `file_id` exactly as `spans.path` already is, `SymbolID` hashes it (the plan's own signature says so), and without it Task 5's `Caller` cannot cite a definition without joining `files`. Recorded in the migration header with the other two.

- [x] **Step 4: Commit, then prove the tests discriminate**

Fifteen mutations, all fifteen killed, each with the observed message rather than the prediction. M1 is the plan's own predicted survivor and it survived the fixture the plan named; the fixture that kills it is new.

**M1 — `ON CONFLICT (id) DO UPDATE` → `DO NOTHING` on the edge insert.**
- *Why the code exists:* ids are deterministic, so the second run's rows collide with the first's by design; `DO NOTHING` keeps the older row's `provenance` and `to_symbol_id`.
- *Fixture that separates mutant from original:* **not** `TestARerunUpgradesASyntacticEdgeToResolvedLive` — the plan's warning was right and is now measured: with the wholesale `DELETE` in place the second write has nothing to conflict with, and that test **passes under the mutant**. The fixture that separates them is `TestADuplicateCallSiteInOneWriteKeepsTheResolvedLabelLive`: two rows for one call site *inside one write*, syntactic then resolved, which is the shape a type-check pass produces if it appends its upgrades. The deletes cannot help there.
- *Must fail:* `TestADuplicateCallSiteInOneWriteKeepsTheResolvedLabelLive`
- *Observed — killed:* `edge ee66acefb238092c97e5458881139976 read back as {… ToSymbolID: … Provenance:syntactic …}, want {… ToSymbolID:526348350e3e3e260ac75655c5a8bb80 … Provenance:resolved …}`. The rerun test passed in the same run, which is the survivor recorded as a survivor.
- *Compiles and vets:* yes.

**M2 — the two `DELETE`s dropped (the writer becomes insert-only).**
- *Why the code exists:* re-indexing must converge, and a graph that only grows keeps edges from a call site that has since been deleted.
- *Fixture that separates mutant from original:* `TestPutGraphReplacesAnEarlierGraphForTheSameRepoLive`, whose second write has **fewer** edges than the first.
- *Must fail:* `TestPutGraphReplacesAnEarlierGraphForTheSameRepoLive`
- *Observed — killed:* `unexpected edge to Println at a.go:8 (b085523c…), provenance syntactic` and `unexpected edge to Close at a.go:4 (d8a861d4…), provenance syntactic`. The assertion names *which* edges outlived their call sites; a count would have said "3, want 1" and left a reader guessing.
- *Compiles and vets:* yes.

**M3 — the deletes lose their repo scope (`WHERE repo_id = $1 OR TRUE`).**
- *Why the code exists:* the corpus holds up to `KEEP_REPOS` repositories and each is written independently.
- *Fixture that separates mutant from original:* `TestPutGraphLeavesAnotherReposGraphAloneLive`, which writes repo B **first**, with a symbol of the *same name* as A's and *more* edges than A has.
- *Must fail:* `TestPutGraphLeavesAnotherReposGraphAloneLive`
- *Observed — killed:* `symbol c10b1d31… (Store.Get a.go:3) is not in the table`, `symbol f1183aec… (Caller a.go:7) is not in the table`, and all three of B's edges likewise.
- *Compiles and vets:* yes, and `$1` stays bound.

**M4 — `SymbolID` drops `start` from the hash.**
- *Why the code exists:* a name is not unique in a file — two `init()` functions in one file are legal.
- *Fixture that separates mutant from original:* the plan was right that nothing in its own list could kill this. `TestPutGraphWritesSymbolsAndEdgesLive`'s fixture is **two `init` functions in one file** plus `Store.Get`, and it asserts the two ids are distinct in the test body before it asserts anything about rows.
- *Must fail:* `TestPutGraphWritesSymbolsAndEdgesLive`
- *Observed — killed:* `both init definitions hash to 89570713042d5dc907dd61217cbdf21a: a symbol id that drops the start line cannot tell two same-named declarations in one file apart`. The row-level assertion is a second, independent kill: the surviving row keeps `start_line 3` where the fixture's second `init` wants 7.
- *Compiles and vets:* yes.

**M5 — `EdgeID` no longer separates two calls in one declaration (the offset dropped from the hash).**
- *Why the code exists:* two calls on one line are two edges, and the id is what makes them two rows.
- *Plan defect, minor:* the mutation as written — "`EdgeID` uses the line instead of the offset" — cannot be spelled inside the function under test, because `EdgeID` takes the offset as a parameter and never sees a line. Dropping the offset from the hash is the same behaviour for the fixture that matters and is a one-token edit.
- *Fixture that separates mutant from original:* `TestTwoCallsOnOneLineAreTwoRowsLive`, whose fixture is `a(b(), b())`. Every other fixture has one call per line.
- *Must fail:* `TestTwoCallsOnOneLineAreTwoRowsLive`
- *Observed — killed:* `both calls to b on line 4 hash to e39084ab0dd0ced877d395996e47bd6f: an edge id keyed on the line cannot tell two calls on one line apart`.
- *Compiles and vets:* yes — `strconv` keeps its other use in `SymbolID`, so nothing is left unimported.

**M6 — the Go-side provenance/target check deleted.**
- *Why the code exists:* it turns a constraint violation on an anonymous row into an error naming the edge.
- *Fixture that separates mutant from original:* `TestPutGraphNamesTheEdgeWhoseProvenanceAndTargetDisagreeLive`, which asserts the error **names the edge's `to_name`, its call site and its label**.
- *Must fail:* `TestPutGraphNamesTheEdgeWhoseProvenanceAndTargetDisagreeLive`
- *Observed — killed, six assertions across two sub-tests:* `error "edges 0-0: ERROR: new row for relation \"edges\" violates check constraint \"edges_provenance_target\" (SQLSTATE 23514)" does not name "Get"` / `"a.go:8"` / `"resolved"`, and the same three for the syntactic-with-a-target case. This is the rule in one screenful: `err != nil` passes under the mutant, because the constraint still fires.
- *Compiles and vets:* yes.

**M7 — the CHECK constraint dropped from the migration.**
- *Why the code exists:* it is §3's invariant, made true by construction rather than by convention.
- *Fixture that separates mutant from original:* the direct-`INSERT` tests, which do not go through `PutGraph` at all.
- *Must fail:* `TestTheSchemaRefusesAResolvedEdgeWithNoTargetLive`, `TestTheSchemaRefusesASyntacticEdgeWithATargetLive`, `TestMigration0010AppliesToAPopulatedDatabaseTwiceLive`
- *Observed — killed:* `the INSERT succeeded, want violates check constraint "edges_provenance_target"` twice, and `run 1: a resolved edge with no target was accepted` / `run 2: …` from the migration test.
- *Compiles and vets:* yes. **And the plan's "verify that" is verified:** the mutated migration took effect, so `testdb` does create a database per run and the ledger does re-run from empty — the mutation is not invisible.

**M8 — `symbols.repo_id` declared without `ON DELETE CASCADE`.**
- *Why the code exists:* spec §3 — "Eviction is one `DELETE`".
- *Fixture that separates mutant from original:* `TestEvictingARepoTakesItsGraphWithItLive`, which asserts `Evict` returns no error *and* the row counts, for both the evicted repo and a kept one.
- *Must fail:* `TestEvictingARepoTakesItsGraphWithItLive`
- *Observed — killed:* `Evict returned ERROR: update or delete on table "repos" violates foreign key constraint "symbols_repo_id_fkey" on table "symbols" (SQLSTATE 23503): eviction is one DELETE and the graph has to go with it, not block it`.
- *Prediction that was wrong, recorded:* this task predicted M8 would **survive**, on the reasoning that `symbols.file_id`'s cascade would delete the rows before `symbols.repo_id`'s NO ACTION check ran. It does not: Postgres raises the FK violation regardless of the other cascade. The plan's expected message was right and the prediction against it was wrong.
- *Compiles and vets:* yes.

**M9 — `span_id` declared `ON DELETE CASCADE` instead of `SET NULL`.**
- *Why the code exists:* `PutSpans` deletes a repo's spans on every re-index; a cascade would delete the repo's symbols as a side effect of re-chunking.
- *Fixture that separates mutant from original:* `TestReplacingASpanLeavesTheSymbolWithANullLinkLive`, which writes spans, then the graph, then spans again.
- *Must fail:* `TestReplacingASpanLeavesTheSymbolWithANullLinkLive`
- *Observed — killed:* `symbol c412766b1ac7c58a26e700fb4933b117 (A a.go:3) is not in the table` — and the assertion names *which* symbol went, with the unlinked one still present, so a mutant that deleted everything and one that deleted the linked row read differently.
- *Compiles and vets:* yes.

**M10 — `edges.repo_id` declared without `ON DELETE CASCADE`.** *(new: M8 pins only the symbols half of the cascade)*
- *Why the code exists:* the same `DELETE` has to reach edges, and edges' own cascade from `from_symbol_id` is not the same reference.
- *Fixture that separates mutant from original:* `TestEvictingARepoTakesItsGraphWithItLive`.
- *Observed — killed:* `Evict returned ERROR: update or delete on table "repos" violates foreign key constraint "edges_repo_id_fkey" on table "edges" (SQLSTATE 23503): …`.
- *Compiles and vets:* yes.

**M11 — `nullable` returns `&s` unconditionally, so `""` reaches the column instead of NULL.** *(new: the ""↔NULL mapping is otherwise a write nothing reads back)*
- *Why the code exists:* `models` has no pointers, so a symbol with no span and an edge with no target both carry `""`, and one conversion at the boundary is the trade.
- *Fixture that separates mutant from original:* `TestPutGraphWritesSymbolsAndEdgesLive`, whose claim is stated as a claim — "a symbol with no span and an edge with no target are ordinary rows, not errors" — so the mutant fails an assertion rather than tripping a bare `t.Fatal(err)`.
- *Observed — killed:* `PutGraph returned symbols 0-2: ERROR: insert or update on table "symbols" violates foreign key constraint "symbols_span_id_fkey" (SQLSTATE 23503): a symbol with no span and an edge with no target are ordinary rows, not errors`.
- *Compiles and vets:* yes.

**M12 — the `kind IN ('calls','imports','references')` CHECK dropped.** *(new)*
- *Why the code exists:* §3's enum is a closed set, and a value outside it is a row every later query would need a case for.
- *Fixture:* `TestTheSchemaRefusesAKindOrProvenanceOutsideTheSetLive/kind`, a direct `INSERT` of `kind = 'invokes'`.
- *Observed — killed:* `the INSERT succeeded, want edges_kind_check`.

**M13 — the `provenance IN ('resolved','syntactic')` CHECK dropped.** *(new)*
- *Why the code exists:* the pair constraint alone accepts `('guessed', NULL)` — it only tests equality with `'resolved'`.
- *Fixture:* `TestTheSchemaRefusesAKindOrProvenanceOutsideTheSetLive/provenance`.
- *Observed — killed:* `the INSERT succeeded, want edges_provenance_check`.

**M14 — `provenance` loses its `NOT NULL`.** *(new: this is the defect Step 1 found, made into a test)*
- *Why the code exists:* it is the only thing closing the pair constraint's three-valued-logic hole. With it gone, both CHECKs on the row evaluate to NULL and both pass.
- *Fixture:* `TestTheSchemaRefusesAnEdgeWithNoProvenanceLive`, a direct `INSERT` of a NULL provenance.
- *Observed — killed:* `the INSERT succeeded, want null value in column "provenance"`.

**M15 — the symbol upsert stops updating `span_id` (`DO UPDATE SET … end_line = EXCLUDED.end_line` only).** *(new: the upsert's column list is otherwise unread)*
- *Why the code exists:* the columns the id does not determine are exactly the ones that have to move when one write carries a definition twice — a file walked twice, or a caller that appends its span links.
- *Fixture:* `TestADuplicateSymbolInOneWriteKeepsTheLastRowLive`, whose two entries share an id and differ in `span_id` and `end_line`.
- *Observed — killed:* `symbol 39bc960c… read back as {… SpanID:}, want {… SpanID:72983081845b3c4371fb306609f6aa5e}`.

*Considered and excluded:* a `graphBatchSize` mutation (a different number of round trips is not a behaviour change), and reversing the symbol/edge insert order (an FK violation on the way to every assertion — an incident, not a kill).

- [x] **Step 5: Commit**

**Definition of Done** — all met.
- `symbols` and `edges` exist, cascade from `repos` (M8 and M10, one per table), and the pair invariant is a constraint rather than a convention — proven by two direct `INSERT`s the constraint refuses, plus a third for the NULL-provenance hole the plan had reasoned away.
- `PutGraph` replaces a repo's graph wholesale in one transaction, converges across two identical runs, and touches no other repo.
- A re-index upgrades an edge's provenance rather than keeping the old label — and the mechanism is the wholesale delete, not `ON CONFLICT`, which M1 is what proves.
- Eviction takes the graph with it, asserted by row counts on the evicted repo *and* on a kept one.
- Re-writing spans leaves symbols with a null link, not with no symbols.
- M1–M15 recorded with observed output; M1's survivor status resolved by naming the fixture that reaches `ON CONFLICT` at all.

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

  > **PLAN DEFECT — the rationale is false and the control does less than it says.** Measured: `packages.Load` succeeds with the child's `PATH` set to `/nonexistent-bin` and with it set to empty, because `go` finds its own tools through `GOROOT`, not `PATH`. Two consequences. (a) **The child's `PATH` does not choose the `go` binary.** `go/packages` runs `exec.Command("go", …)`, which resolves the name against the *parent's* `PATH`; `Policy.GoBin` was therefore decorative, and M16 shows a bogus `GoBin` still producing `reason "ok"` and four resolutions. `Validate` now pins `exec.LookPath("go") == GoBin` and refuses with `ErrNoToolchain` otherwise. (b) **`filepath.Dir(GoBin)` is often `/usr/bin`**, where `git` and `cc` also live, so `PATH` is not a containment boundary on its own — measured as M6/M6b: with `go` at `~/.local/bin/go` the child cannot reach a C compiler at all, which *masks* `CGO_ENABLED=0`'s kill; widen `PATH` to include `/usr/bin` and the same mutant loads the cgo package. The settled value is kept, and the honest statement of what keeps `git` and `cc` out of reach is `GOVCS`, `GOPROXY` and `CGO_ENABLED`, each of which is its own lock. **An empty `PATH` is strictly stronger and provably works; recommended for a follow-up.**
- **The git variables from `clone.Run` are set here too** (`GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS=/bin/false`, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`). Redundant under `GOVCS=*:off`, kept because two independent controls against arbitrary outbound `git` is the right number for the one thing in this phase that would be a genuine vulnerability.
- **No `packages.Config.ParseFile` hook — and the reason the plan gave for it was false.**

  > **PLAN DEFECT — the stated measurement said the opposite.** Task 1 measured that `StripDocs` does *not* blank in place: it removes the prose bytes and keeps only their line terminators, so line numbers are invariant and **byte offsets are not** (102 bytes in, 41 out, on `docs.gotxt`). The two streams therefore do **not** share a coordinate system, and the comment this bullet asked for would have been a false comment — the thirteenth in this project.
  >
  > The conclusion survives on a different reason: **both passes parse the same bytes on disk.** `go/packages` reads the file, and Task 4 must hand `symbols.Parse` the raw `body` and never the stripped `src`. A hook cannot enforce that — the mismatch is the caller's, not the loader's — so it is pinned by a test instead. `TestResolutionKeysAreOffsetsIntoTheBytesOnDisk` asserts that four of `Parse`'s keys over the on-disk bytes resolve, that stripping the same file *moves* offsets, and that the stripped keys do not hit as often. `Key`'s doc comment says the same in one sentence. Under M10 the first assertion reads `0 of Parse's keys resolved, want 4`, which is exactly what silently handing over stripped bytes would look like.
- **Only a call resolving to a `*types.Func` that is package-scoped or a method is resolved.** `Uses` will happily resolve `fn()` where `fn` is a local variable holding a closure — to the *variable*, whose position is inside the enclosing function, which would map to an edge from a function to itself. A self-call that is not a self-call is worse than no edge.
- **A call that resolves to an object outside the checkout is `External`, and produces no target.** `fmt.Println` type-checks perfectly and has no `symbols` row to point at. §3 says a resolved edge points at a symbol row and spec:84 says a null target means syntactic; both cannot hold for this call, so the invariant wins and the fact is kept in `Stats.External`. See "what the spec leaves underspecified".
- **`packages.Load(cfg, "./...")` from the checkout root**, `Tests: false`, mode `NeedName|NeedFiles|NeedSyntax|NeedTypes|NeedTypesInfo|NeedImports|NeedDeps`.

  > **PLAN DEFECT — `NeedDeps` is present, and the plan's reason for omitting it was backwards.** `packages.usesExportData` is `Mode&NeedExportFile != 0 || Mode&NeedTypes != 0 && Mode&NeedDeps == 0` (x/tools v0.49.0, `packages.go:1593`). Omitting `NeedDeps` is therefore what makes go/packages run `go list -export=true`, and `-export=true` is what makes the go command **compile** every dependency. Measured with a logging wrapper named `go` on the parent's `PATH`: on the `std` fixture, 2.66s and 31.8MB written under the job's scratch home without `NeedDeps` against 0.38s and 0.69MB with it; on a package importing `net/http` and `encoding/json`, 5.89s and 92.4MB against 0.69s and 1.04MB (peak RSS 274MB against 411MB — the one number that moves the wrong way). With the caches scoped to one job, none of those 92MB is ever reused. `NeedDeps` also means the only subprocess is `go list`, which matters for a phase whose whole risk is a subprocess against untrusted input.
  >
  > **Verified, not trusted:** `go list` is invoked with `-e` and, unasked, with `-buildvcs=false` and `-pgo=off`. The first is the `-e` behaviour the plan asked to be checked; the second means `go list` does not run `git` inside the stranger's checkout.

- [x] **Step 1: Measure what the go command does under this policy, before writing the policy**

Six fixture modules under `testdata/mod/` (`.gotxt` and `go.mod.txt`, copied and renamed into a `t.TempDir()` so neither the repository's own build nor `gofmt -l` walks into a module meant to be broken). Two more than the plan listed, and both were earned by a measurement:

| fixture | shape | what it is for |
| --- | --- | --- |
| `std` | two packages, std imports only, a cross-package call, `fmt.Println`, a local closure, `a(One(), One())` | the resolved path and every per-call classification |
| `absent` | `require example.com/nope v1.0.0` **plus a `go.sum`**, three packages: `good`, `bad`, `blocked` | the expected failure, and the per-edge claim |
| `thirdparty` | `require golang.org/x/mod v0.39.0` with real `go.sum` hashes | *(new)* the module fetch that would succeed if fetching were on |
| `work` | `std` plus a `go.work` naming `/nonexistent-outside-the-checkout` | *(new)* the workspace escape |
| `newgo` | `go 1.99.0` | the toolchain trap |
| `cgo` | `import "C"` with a `#cgo LDFLAGS` line | the execution trap |
| `nomod` | no `go.mod` at all | the shape most single-file repositories have |

Measured with a wrapper named `go` first on the parent's `PATH` logging argv, and an `httptest` recorder as `GOPROXY`. The four questions the plan refused to guess:

1. **Does `packages.Load` return syntax and partial type info for packages that failed?** Yes, and better than the design needed. On `absent`, both `good` and `bad` come back with `Types` and `TypesInfo`; `bad` carries `could not import example.com/nope/thing (invalid package name: "")` and still resolves its own `Helper`. So a single package carries both labels, which is a stronger statement of spec:190 than the plan's "one package loads and its sibling does not".
2. **`nomod`:** `packages.Load` returns *no error* and **one** synthetic package named `./...` carrying `pattern ./...: directory prefix . does not contain main module or its selected dependencies`. `Resolve` therefore stats `Root/go.mod` first and never starts a subprocess for it.
3. **`GOTOOLCHAIN=local` fails `newgo` before any network activity.** `go: go.mod requires go >= 1.99.0 (running go 1.27.0; GOTOOLCHAIN=local)`, recorder count 0. Without it, and with a proxy, two requests for `/golang.org/toolchain/@v/v0.0.1-go1.99.0.linux-amd64.zip`.
4. **With `GOPROXY=off`, is anything attempted over the network?** Nothing, on every fixture.

> **PLAN DEFECT — the `absent` fixture as specified could not prove question 4.** With a `require` line and **no `go.sum`**, the recorder receives **zero** requests *even when the child is pointed at it*: under `-mod=readonly` the go command refuses on the missing `go.sum` entry before it fetches. A test written on that fixture would have asserted zero against a fixture that never wanted a module — the "fails for an unrelated reason" trap. Adding `go.sum.txt` makes the fetch real: the control run receives `/example.com/nope/@v/v1.0.0.zip` and `/example.com/nope/@v/v1.0.0.mod`. **The zero only means something because the control is in the same test.**

Also measured, each one now a comment only because the counterfactual was run first:

- **`GOTMPDIR` must exist.** Every load fails with `go: creating work dir: stat …/tmp: no such file or directory`. The go command creates `GOCACHE` and `GOMODCACHE` and does not create this one. `Resolve` makes all four (M17).
- **A symlinked checkout root needs no `EvalSymlinks`.** Positions come back *under the symlink* (`…/link/lib/lib.go`), because `go/packages` sets `PWD` to the working directory it was given. No code was added for a case that does not exist.
- **`go list` is run with `-buildvcs=false` and `-pgo=off`** without being asked, so it does not run `git` inside the checkout.
- **In proxy mode the module cache is written read-only**, and `os.RemoveAll` over the job scratch directory then fails with `permission denied`. Observed as a `TempDir RemoveAll cleanup` failure during M2a. Harmless under the shipped default, where nothing is ever fetched; **a leak for Task 4 to handle if an operator sets a proxy.** Recorded against Open question 10.

- [x] **Step 2: Write the failing tests**

`policy_test.go` is hermetic; `load_test.go` runs the real go command and needs no network. Every test in the second file is run against a *planted hostile parent environment* where that is the point.

```go
// policy_test.go
func TestTheChildEnvironmentIsAnAllowlist(t *testing.T)          // the whole key set, not one entry
func TestAProxyOfDirectIsRefused(t *testing.T)                   // direct, off,direct, https://x,direct, https://x|direct, http://, file://
func TestPolicyRefusesAnEmptyGoBinOrRoot(t *testing.T)
func TestAGoBinaryThePathDoesNotResolveIsRefused(t *testing.T)   // new: see the PATH defect

// load_test.go
func TestCallsWithinTheModuleResolveToTheirDefinitions(t *testing.T)
func TestACallIntoTheStandardLibraryIsExternalNotResolved(t *testing.T)
func TestACallToALocalClosureIsNotResolved(t *testing.T)
func TestATargetsLineIsTheDeclarationNotItsDocComment(t *testing.T)   // new: a Task 4 carry-forward
func TestResolutionIsKeyedByOffsetNotByLine(t *testing.T)
func TestResolutionKeysAreOffsetsIntoTheBytesOnDisk(t *testing.T)     // new: replaces the false rationale
func TestOneCallResolvesWhileAnotherInTheSamePackageDoesNot(t *testing.T)
func TestAPackageThatCannotLoadLeavesItsCallsUnresolved(t *testing.T)
func TestTheLoaderNeverReachesTheModuleProxy(t *testing.T)            // + its control
func TestARequiredThirdPartyModuleDoesNotTypeCheck(t *testing.T)      // new: the network-free half of the same claim
func TestAHostileParentEnvironmentCannotBreakTheLoad(t *testing.T)    // nine variables
func TestAnExternalPackagesDriverOnThePathIsNotRun(t *testing.T)      // new: see below
func TestAGoWorkFileInTheCheckoutIsIgnored(t *testing.T)              // new
func TestAGoEnvFileUnderTheScratchHomeIsIgnored(t *testing.T)         // new
func TestAToolchainDirectiveDoesNotFetchAToolchain(t *testing.T)      // + its proxy-mode control
func TestACgoPackageDoesNotLoad(t *testing.T)
func TestARepositoryWithNoGoModRecordsItsReason(t *testing.T)
func TestAnExpiredContextResolvesNothingAndSaysWhy(t *testing.T)      // + its control
func TestAMissingGoBinaryDegradesRatherThanFailing(t *testing.T)      // new: Open question 9
```

> **PLAN DEFECT — `TestTheOperatorsGoflagsCannotBreakTheLoad` on the `std` fixture is void.** Measured: `go list -e ./...` with `GOFLAGS=-mod=vendor` and no vendor directory **succeeds** on a module with no requirements — `example.test/std` and `example.test/std/lib`, exit 0. The mutant passes. The variable only bites on a module with a `require` line, where it produces `go: inconsistent vendoring in …`. The test is kept, moved to the `absent` fixture, and generalised: nine parent variables, each of which is closed **by omission** rather than by an entry setting it to something safe, so the append-to-`os.Environ` mutant inherits it. `GOFLAGS`, `GOROOT`, `GOEXPERIMENT` and `GODEBUG` are the four that kill M1; `GOPRIVATE`, `GOTOOLCHAIN`, `CGO_ENABLED`, `GOWORK` and `GOPACKAGESDRIVER` do not, because `os/exec` de-duplicates the child's environment keeping the *last* occurrence and the allowlist is appended last. **That is also why the allowlist must not spell `GOFLAGS=`**: an entry naming it would be safe under the mutant too, and the mutation would prove nothing.

> **FINDING — `GOPACKAGESDRIVER=off`, which the plan does not mention, is a hole nothing else closes.** `packages.findExternalDriver` reads `GOPACKAGESDRIVER` from `cfg.Env` and, when it is unset, falls back to `exec.LookPath("gopackagesdriver")` **against the parent's `PATH`** and runs whatever it finds *in place of the go command*, handing it the config and the checkout. Measured with a script named `gopackagesdriver` on the parent's `PATH`: with the entry, it does not run and the load resolves four calls; without it, the marker file appears and `packages.Load` returns 0 packages in 1ms. This is arbitrary program execution selected by the operator's `PATH`, and no value in the child's environment can close it — only naming it `off` can.

- [x] **Step 3: Implement**

`policy.go` and `load.go`. Two shapes are worth stating because they are the ones the mutations attack:

- `Env()` is the whole environment and it is built from nothing. What is *absent* is the control: `GOFLAGS`, `GOPRIVATE`, `GOINSECURE`, `GOEXPERIMENT`, `GOROOT`, `GODEBUG`, `LD_PRELOAD`, `HTTPS_PROXY`, `XDG_CONFIG_HOME` and `CGO_LDFLAGS` are closed by omission. What is *present* is only what has an unsafe default: `GOPROXY`, `GOVCS`, `GOTOOLCHAIN`, `GOWORK`, `GOENV`, `GOPACKAGESDRIVER`, `CGO_ENABLED`, the four caches, `PATH`, `HOME`, and `clone.Run`'s four git variables.
- `Resolve` has no error return and a closed reason set. **A sixth reason was added: `policy`**, for a `Validate` failure that is not a missing toolchain — a refused `GOPROXY`, a relative `Root`. The plan's five could only have expressed it as `disabled`, which would be a lie about which knob was turned. Recorded as a deviation.

- [x] **Step 4: Commit, then prove the tests discriminate**

Twenty mutations. **Eighteen killed, two recorded as survivors**, each with the observed message rather than the prediction.

**M1 — `Env()` returns `append(os.Environ(), …)` instead of the allowlist.**
- *Why the code exists:* the operator's environment can re-open every control in this file.
- *Fixture that separates mutant from original:* `TestAHostileParentEnvironmentCannotBreakTheLoad` on the **`absent`** fixture (see the defect above: on `std` it cannot), plus `TestTheChildEnvironmentIsAnAllowlist`.
- *Observed — killed, four subtests plus the allowlist test:* `TestAHostileParentEnvironmentCannotBreakTheLoad/{GOFLAGS,GOROOT,GOEXPERIMENT,GODEBUG}`, each `the call at good/good.go offset 70 resolved to nothing, want good/good.go:3`. `GOPRIVATE`, `GOTOOLCHAIN`, `CGO_ENABLED`, `GOWORK` and `GOPACKAGESDRIVER` survive this mutant, because the appended entry wins — which is the measured reason the omitted variables are the ones that matter.
- *Also observed, and worth its own line:* the allowlist test's output under M1 names what an inherited environment actually carries into a subprocess running on a stranger's source tree — `SSH_AUTH_SOCK`, `GITHUB_PERSONAL_ACCESS_TOKEN`, `RAZORPAY_KEY`, `XAUTHORITY`, and fifty more.
- *Compiles and vets:* yes, with `os` imported.

**M2a — the `GOPROXY` entry dropped from the allowlist.**
- *Why the code exists:* it is the control that decides whether this process fetches anything at all.
- *Fixture that separates mutant from original:* **not the recorder** — this is the trap the plan walked into. With the entry gone the child falls back to the go command's default `https://proxy.golang.org,direct`, which the recorder in the parent cannot see; a recorder-only test records zero and the mutant survives. The fixture that separates them is `thirdparty`, whose `require golang.org/x/mod v0.39.0` **succeeds** the moment fetching is allowed.
- *Must fail:* `TestARequiredThirdPartyModuleDoesNotTypeCheck`
- *Observed — killed:* `stats {Packages:1 Loaded:1 Failed:0 Resolved:0 External:1 Unresolved:0 Reason:ok}, want one failed package and none loaded`, `External is 1, want 0`, `reason "ok", want "load_error"` — the mutant went to the real network and type-checked a third-party module. `TestTheLoaderNeverReachesTheModuleProxy`'s control also fails (`the control run reached the proxy 0 times`), because there is no `GOPROXY=` entry left to override.
- *Compiles and vets:* yes.

**M2b — `Env()` falls back to the inherited `GOPROXY` when the policy's is empty.**
- *Why the code exists:* this, not deletion, is the shape the bug takes in real life — "respect the operator's setting".
- *Fixture:* `absent` with its `go.sum`, and the recorder as the parent's `GOPROXY`.
- *Observed — killed:* `the module proxy received 2 requests [/example.com/nope/@v/v1.0.0.zip /example.com/nope/@v/v1.0.0.mod], want 0`.
- *Compiles and vets:* yes.

**M3 — `GOVCS=*:off` dropped.**
- *Observed — survivor, as predicted.* Whole suite `ok`. With `GOPROXY=off` no fetch of any kind is attempted, so `GOVCS` is never consulted; the only configuration that would consult it is a `direct` proxy, which `Validate` refuses (M4). Kept as defence in depth with the reason it cannot be killed.

**M4 — `Validate` accepts anything but a bare `direct` (`strings.TrimSpace(p.Proxy) == "direct"`).**
- *Why the code exists:* `GOPROXY=direct` turns a stranger's `require` line into an outbound connection to a host of their choosing.
- *Observed — killed, five subtests:* `Validate("off,direct") returned nil, want an error naming the setting`, and the same for `https://proxy.example,direct`, `https://proxy.example|direct`, `http://proxy.example` and `file:///tmp/proxy`. The `|` row is the one the plan did not have: `GOPROXY` separates fallbacks with `,` **and** `|`.
- *Compiles and vets:* yes — `strings` keeps a use, which the naive `p.Proxy == "direct"` spelling did not (that version is a build break, and void).

**M5 — `GOTOOLCHAIN=local` dropped.**
- *Fixture that separates mutant from original:* `newgo` **in proxy mode**. In the default mode both versions fail without a fetch, because `GOPROXY=off` blocks the toolchain download too. The proxy-mode control overrides *only* `GOPROXY` and reads `GOTOOLCHAIN` from production's `Env()`, which is what makes it a kill rather than a tautology.
- *Observed — killed:* `the proxy received /golang.org/toolchain/@v/v0.0.1-go1.99.0.linux-amd64.zip: a toolchain was fetched for a stranger's go directive` (twice — the go command retries).
- *Compiles and vets:* yes.

**M6 — `CGO_ENABLED=0` dropped.**
- *Observed — survivor on this machine, and the reason is the finding.* `go test -run Cgo` is `ok`. `go` is at `~/.local/bin/go`, so the child's `PATH` holds one directory with no C compiler in it, and cgo is unavailable whatever `CGO_ENABLED` says. **`PATH` masked the control.**
- **M6b — `CGO_ENABLED=0` dropped *and* `/usr/bin` added to the child's `PATH`**, modelling the Debian/Ubuntu and `ubuntu-latest` layout where `go` lives beside `gcc`. *Observed — killed:* `Packages is 1, want 0: a cgo package must not load at all` and `reason "ok", want "load_error"`. Machine: Linux 7.0.0-30-generic, `/usr/bin/gcc` present, go 1.27.0 at `/home/alamin/.local/bin/go`.
- **M6c — `CGO_ENABLED=0` kept, `/usr/bin` added to the child's `PATH`.** *Observed — survivor:* `ok`. So the two controls are independent and each is sufficient alone, which is the empirical case for keeping both rather than the plan's assumption that one is redundant.
- *Compiles and vets:* yes.

**M7 — the context is replaced by a fresh one (`context.WithTimeout(context.Background(), 2*time.Minute)`).**
- *Why the code exists:* spec:194 — one deadline for the whole job.
- *Fixture:* an already-cancelled context over `std`, with a control sub-test proving the same fixture resolves four calls when the budget is intact.
- *Observed — killed:* `reason "ok", want "deadline"` and `resolved 4 calls with reason "ok", want 0 and "deadline"`.
- *Compiles and vets:* yes.

**M8 — the `*types.Func` / package-scope guard removed, so any object with a position resolves.**
- *Observed — killed:* `the call at app.go offset 348 resolved to app.go:14, want no target: Uses names the variable holding the closure, whose position is inside Run itself`, plus `Unresolved is 0, want 1` and the whole-`Stats` comparison `Resolved:5 … Unresolved:0` against `Resolved:4 … Unresolved:1`. The plan predicted the target would be the enclosing function's line 12; it is **14**, the variable's declaration — still inside `Run`, still an edge from a function to itself.
- *Compiles and vets:* yes, with `go/types` unimported.

**M9 — a target outside the checkout is returned rather than counted as external.**
- *Observed — killed:* `the call at app.go offset 284 resolved to :306, want no target: fmt.Println has no symbols row` and `External is 0, want 1`. The empty path in `:306` is what the row would have carried. Asserting only the absence of the map entry would have passed under a mutant that dropped the call without counting it, which is why `Stats.External` is asserted beside it.
- *Compiles and vets:* yes.

**M10 — resolution keyed by line instead of offset.**
- *Observed — killed:* `the call at offset 374 did not resolve; a line key would have kept only one of the two` and the same for 385, plus `0 of Parse's keys resolved, want 4: the loader is not keying the bytes on disk`. The two calls to `One` are at offsets 374 and 385 on line 16 — the fixture's real numbers.
- *Compiles and vets:* yes.

**M11 — `Stats.Reason` hard-wired to `"ok"`.**
- *Observed — killed, four tests:* `reason "ok", want "load_error"` from `absent`, `thirdparty` and `cgo`, and `reason "ok", want "no_module": it is what tells this apart from a load that failed` from `nomod`. Four fixtures with three distinct reasons is what makes the field worth having; "reason is not empty" would have passed.
- *Compiles and vets:* yes.

**M12 — `GOWORK=off` dropped.** *(new)*
- *Why the code exists:* a `go.work` inside the checkout can name directories outside it, and the go command will try to load them.
- *Fixture:* `work`, whose `go.work` says `use /nonexistent-outside-the-checkout`.
- *Observed — killed:* `the call at app.go offset 296 resolved to nothing, want lib/lib.go:3`; the go command's own message is `cannot load module /nonexistent-outside-the-checkout listed in go.work file`.

**M13 — `GOENV=off` dropped.** *(new)*
- *Why the code exists:* `~/.config/go/env` is a second copy of every setting the allowlist closes, and it is read by a child with an otherwise empty environment.
- *Fixture:* a planted `$Home/.config/go/env` holding `GOFLAGS=-mod=vendor`.
- *Observed — killed:* `the call at good/good.go offset 70 resolved to nothing, want good/good.go:3`.

**M14 — `GOPACKAGESDRIVER=off` dropped.** *(new — the hole the plan did not have)*
- *Observed — killed:* `a gopackagesdriver on the operator's PATH was executed against the checkout`, and `the call at app.go offset 296 resolved to nothing`.

**M15 — `cfg.Env` set to `nil`.** *(new — the other half of M1, and the one that is a one-token edit)*
- *Why the code exists:* `golist.cfgInvocation` sets `CleanEnv: cfg.Env != nil`. A nil `Env` is not "no environment", it is **the parent's**.
- *Observed — killed, four tests:* `the module proxy received 2 requests …, want 0`; six subtests of the hostile-parent table; `a gopackagesdriver on the operator's PATH was executed`; and the `go.work` test.

**M16 — `Validate` stops pinning `GoBin` to what the parent's `PATH` resolves.** *(new — see the PATH defect)*
- *Observed — killed:* `Validate returned nil for a GoBin the parent's PATH does not resolve to`, and `reason "ok", want "no_toolchain"` with `4 resolutions, want none` — a policy naming `/usr/bin/definitely-not-the-go-on-path` happily type-checking with a binary it never named.

**M17 — the cache directories are not created.** *(new — the measured `GOTMPDIR` requirement)*
- *Observed — killed:* `reason "load_error", want "ok"; stats {Packages:1 Loaded:0 Failed:1 …}` and `the call at good/good.go offset 70 resolved to nothing`.

**M18 — the `go.mod` pre-check dropped, so `nomod` reaches the loader.** *(new)*
- *Observed — killed:* `reason "load_error", want "no_module": it is what tells this apart from a load that failed`. The distinction is the whole value of the field: `no_module` is a repository shape, `load_error` is a repository that tried.

**M19 — `st.Failed > 0` dropped from the reason switch.** *(new: the per-package failure count is otherwise a write nothing reads back)*
- *Observed — killed:* `reason "ok", want "load_error"; stats {Packages:2 Loaded:1 Failed:1 Resolved:2 …}` and the same on `thirdparty`.

*Considered and excluded:* mutating `loadMode` (a different mode is a performance and subprocess change that the assertions cannot see as a behaviour change on these fixtures — it is measured in Step 1 instead); reversing `Loaded`/`Failed` (an incident, not a kill, since every stats comparison is whole-struct already); and dropping the git variables, which is M3's survivor with a different name.

- [x] **Step 5: Commit**

**Definition of Done** — all met.
- The child's environment is an allowlist, proven by tests whose *parent* is hostile: nine variables, four of which kill the append-to-`os.Environ` mutant, and a planted `gopackagesdriver`, `go.work` and `~/.config/go/env` besides.
- A recording proxy receives **zero** requests while type-checking a module that requires an absent dependency — **and a control in the same test proves the same fixture makes two requests when the child is allowed to fetch**, which is the only thing that makes the zero evidence. The network-free half of the claim is `thirdparty`, where a real module simply does not type-check.
- A `go` directive newer than the toolchain does not download a toolchain, proven in proxy mode where the difference is observable, with the exact path recorded.
- A cgo package does not load. Machine recorded, and the survivor/kill pair M6/M6b/M6c shows `PATH` and `CGO_ENABLED` are independently sufficient rather than redundant.
- One call resolving while another in the same package does not, asserted by naming both — and the fixture turned out to carry the stronger form of the claim: `bad/` loads with an error, resolves its own `Helper`, and leaves `Missing` unresolved. `blocked/` never loads at all.
- An expired context resolves nothing and says `deadline`, with a control proving the fixture resolves when the budget is intact.
- `Resolve` has no error return, and nothing in the package returns one to a caller.
- A missing `go` binary is `no_toolchain`, not a boot failure and not a job failure (Open question 9).
- M1–M19 recorded with observed output; M3 and M6 recorded as survivors with their reasons, M6 resolved by the M6b variant.

**Carry-forwards this task owes Task 4**, each pinned by a test here rather than left as prose:

1. **Hand `symbols.Parse` the raw `body`, never the stripped `src`** (`apps/indexer/cmd/main.go:486`). `Key` is a byte offset into the bytes on disk; stripping moves offsets and the failure is a silent collapse to zero resolutions. `TestResolutionKeysAreOffsetsIntoTheBytesOnDisk`.
2. **Match a `Target` to a `Def` by containment, not by `StartLine`.** A target's line is the declaration's own token; a `Def`'s `StartLine` begins at its doc comment. `lib.One` in the `std` fixture is defined at 7..9 and resolves to line 9. `TestATargetsLineIsTheDeclarationNotItsDocComment`.
3. **Boot with `exec.LookPath("go")` and pass the result as `Policy.GoBin`.** `Validate` refuses any other value with `ErrNoToolchain`, because the loader runs whatever the parent's `PATH` resolves.
4. **`Stats.Reason` has six values, not five.** `policy` was added for a `Validate` failure that is not a missing toolchain; the counter's label set has to include it.
5. **In proxy mode the module cache is written read-only and `os.RemoveAll` over the scratch directory fails.** Not reachable under the shipped default. Open question 10.

---

### Task 4: The indexer stage — per-edge labelling, one deadline, and a failure that is not a failure

**Files:**
- Modify: `apps/indexer/cmd/main.go` (the `indexer` struct, `runJob`, `limitsFrom`, `index`), `packages/shared/metrics/metrics.go`
- Create: `apps/indexer/cmd/graph.go`
- Test: `apps/indexer/cmd/graph_test.go`, additions to `apps/indexer/cmd/index_live_test.go`
- Also modified, and none of the three was on the plan's list: `packages/shared/symbols/load.go` gains `Reasons`, the exported closed set the counter's label vocabulary is pinned against; `packages/shared/symbols/policy.go` gains `ValidateProxy`, so `TYPECHECK_GOPROXY` is refused at boot rather than once per job; and the live fixture repository gains `use.go` and `calc/use.go`, without which it holds no in-repo call at all. The gateway's live suite reads the same fixture tree, so its `astPaths`/`windowPaths` follow.

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

- [x] **Step 1: Write the failing tests**

Hermetic, in `graph_test.go`, against a substituted resolver — the seam exists so that the labelling logic is testable without running the go command twice per assertion. The eight the plan listed, plus six the debts and the mutation round earned:

```go
func TestEveryCallIsAnEdgeBeforeAnythingIsResolved(t *testing.T)
func TestOnlyTheCallSitesTheResolverNamedAreResolved(t *testing.T)   // the per-edge claim, both directions
func TestAResolverFailureLeavesEveryEdgeSyntacticAndTheJobIntact(t *testing.T)  // five reasons
func TestASymbolLinksToTheSpanThatContainsIt(t *testing.T)
func TestASymbolLinksToTheMostSpecificOfTwoOverlappingWindows(t *testing.T)
func TestADeclarationWithNoSpanStillGetsASymbolWithANullLink(t *testing.T)
func TestTheJobLogNamesWhatTheGraphStageProducedAndWhyItStopped(t *testing.T)
func TestTheGraphCountersMoveExactlyOnce(t *testing.T)
func TestATargetIsMatchedToTheDefinitionThatContainsIt(t *testing.T)          // new: Task 3's debt 2
func TestTheEdgeTailIsTheDeclarationTheCallIsIn(t *testing.T)                 // new
func TestTheGraphPassParsesTheRawBodyAndNotTheStrippedSource(t *testing.T)    // new: Task 3's debt 1
func TestAScratchTreeTheGoCommandLeftReadOnlyIsStillRemoved(t *testing.T)     // new: Task 3's debt 5
func TestTheGraphCounterVocabulariesAreTheClosedSetsTheyName(t *testing.T)    // new: Task 3's debt 4
func TestTheBootLineSaysWhetherThisWorkerCanTypeCheck(t *testing.T)           // new: open question 9
func TestTheGraphKnobsAreRefusedAtBoot(t *testing.T)                          // new
func TestAGraphWriteThatFailsFailsTheJob(t *testing.T)                        // new: the fake's error path
func TestAFileThatDoesNotParseIsCountedRatherThanLosingTheGraph(t *testing.T) // new
func TestTheTypeCheckerIsGivenTheCheckoutAndTheJobsOwnCaches(t *testing.T)    // new
```

Plus two existing tests that grew rather than being duplicated: `TestEveryStageSharesTheOneJobDeadline` gains `graph` to its stage list **and** the assertion that `putGraph` is *not* on that deadline, and `TestFilesAreWrittenBeforeSpans` becomes `…AndBothBeforeTheGraph`.

Live, appended to `index_live_test.go`: `TestIndexingWritesAGraphForTheFixtureRepoLive`, `TestATypecheckFailureStillCompletesTheJobLive`, `TestTheGraphStageSharesTheJobDeadlineLive`, and `TestEvictionRemovesSpansEndToEndLive` extended to the graph.

> **The live fixture repository gained two files, and they are what make it a graph fixture.** `use.go` in the root package and `calc/use.go` in `calc/`. Measured: `packages.Load` reports **2 packages, 1 loaded, 1 failed** — `calc/` does not type-check because `calc/broken.go` is not parseable Go — and **three of the six edges still resolve, two of them inside the failing package**. So the repository's reason is `load_error` while half its edges are `resolved`, which is the shape no single-package fixture can have. The unresolvable half is real too: `fmt.Sprintf` twice (external) and `len` (a builtin, so `Uses` has no `*types.Func`).
>
> Also measured, and it is where the containment rule earns its place: `calc/use.go@153 → calc/calc.go:20` while `Machine.Push`'s definition is **19..23**, and `use.go@243 → use.go:6` while `Count` is **5..8**. Every resolved target in the fixture lands on a line that is *not* its definition's first line.

- [x] **Step 2: Implement**

`apps/indexer/cmd/graph.go`, with the labelling loop as sketched. Four shapes are worth stating because they are what the mutations attack, and three of them are deviations from the plan as written:

- **The stage takes two contexts.** `runGraph(jobCtx, writeCtx, …)`. The type-check runs on `jobCtx` — spec:194's one budget, shared with the clone and the chunker. The *write* runs on the same process-derived budget as `Complete`. **This is a deviation and the plan implied the opposite.** Measured: with the write on `jobCtx`, `TestTheGraphStageSharesTheJobDeadlineLive` fails the job with `context deadline exceeded` — the type-check spends the job's remaining time by design, so writing its result on that context is the type-check's slowness failing the job, which spec:196 forbids. M18 is the mutation and the live test is its kill.
- **The go command's caches live *beside* the checkout, at `<jobdir>.gohome`.** Task 3 says "under the job's scratch directory"; the job's scratch directory **is** the checkout (`clone.Run` clones into `dir` itself), so caches under it are directories `go list ./...` walks — and in proxy mode a fetched module brings a `go.mod` of its own into the tree being type-checked. Both trees are removed on every path.
- **`index` returns a fourth value, `[]symbols.File`, and counts `unparsed`.** The parse happens in `index`, where the raw `body` is in hand and *before* `src := body` is stripped — the debt is honoured structurally rather than by a comment. `unparsed` joins `vanished`/`unstrippable`/`tokenless` on the existing per-job line, because it is the same kind of soft loss.
- **`mostSpecific` is one function with two callers**, spans and definitions, for the reason `chunk.Decl` is one function with two callers: the rule is "the container holding this line, greatest start, then smallest end, then smallest id", and stating it twice is how the two drift.

- [x] **Step 3: Commit, then prove the tests discriminate**

**Twenty-four mutations. Twenty-three killed, one recorded as a survivor**, each with the observed message rather than the prediction. Two of them killed nothing until the fixture was fixed, and both fixes are recorded below as findings rather than as edits.

**M1 — the label is taken from the package's load status rather than from the call site** (`if g.stats.Reason == symbols.ReasonOK { e.Provenance = resolved }`).
- *Why the code exists:* spec:190. This is the mutation this whole phase is written against.
- *Fixture that separates mutant from original:* `TestOnlyTheCallSitesTheResolverNamedAreResolved`, **and it needs both of its subtests**. With `reason=ok` the mutant labels everything resolved; with `reason=load_error` it labels everything syntactic. Either half alone passes one of the two.
- *Observed — killed, hermetic and live:* `got F calls Println-> at a.go:7 (resolved) … want (syntactic)` in the first subtest and `F calls G-> at a.go:7 (syntactic) … want F calls G->G at a.go:7 (resolved)` in the second. Live: `Total calls Push-> at calc/use.go:7 (syntactic), want Total calls Push->Machine.Push … (resolved)` — the two edges inside the package that failed to load are exactly the ones the mutant loses.
- *Compiles and vets:* yes, with `_ = resolved`.

**M2 — a resolver failure is returned and fails the job.**
- *Why the code exists:* spec:196.
- *Fixture that separates mutant from original:* the substituted resolver's error path — **the fake whose failure mode a test has to set**, the second of P3's two survivor shapes.
- *Observed — killed, five subtests:* `the job failed: graph: load_error`, and the same for `deadline`, `no_module`, `disabled` and `no_toolchain`.
- *Compiles and vets:* yes.

**M3 — the stage gets its own deadline (`context.WithTimeout(jobCtx, 2*time.Minute)`).**
- **PLAN DEFECT — the mutation as the plan spells it is void.** `WithTimeout` takes the *earlier* of the parent's deadline and its own, so a two-minute child of a thirty-second parent has the parent's deadline exactly. Whole suite `ok`. It is not a survivor, it is a no-op: there is no behaviour to detect.
- **M3a — `context.WithTimeout(context.Background(), 2*time.Minute)`**, the failure spec:194's sentence was written against. *Observed — killed:* `graph was given 2m0.000496483s, more than the job's 30s` and `graph got deadline …m=+120.0024, the clone got …m=+30.0019: that is a fresh budget per stage`. Live: `the job log says reason load_error, want "deadline"` — the mutant type-checks happily on a job whose budget is gone.
- **M3b — `context.WithTimeout(jobCtx, 2*time.Second)`**, the smaller second budget the plan calls "defensible and still wrong". *Observed — killed:* `graph got deadline …m=+2.0022, the clone got …m=+30.0017: that is a fresh budget per stage`.
- *Fixture that separates mutant from original:* the hermetic stage-deadline recorder, and the live test whose budget is spent in `putSpans` before the stage starts. **The live one is deterministic rather than timed:** the wrapped `putSpans` writes and then waits for `ctx.Done()`, so the stage starts on an expired context at whatever speed the machine runs.
- *Compiles and vets:* yes.

**M4 — the stage runs before `putSpans`.**
- *Why the code exists:* `symbols.span_id` references a row that must exist, and the containment link is computed against ranges only the chunker knows.
- *Observed — killed, and the plan's "verify which" is answered:* it is the **foreign key**, and it fails the *job* — `the job failed instead of indexing: symbols 0-8: ERROR: insert or update on table "symbols" violates foreign key constraint "symbols_span_id_fkey" (SQLSTATE 23503)` — plus the hermetic `call order [put putGraph putSpans], want [put putSpans putGraph]`. So the assertion carrying the live kill is the job's status, as the plan suspected.
- *Compiles and vets:* yes.

**M5 — the span link is by exact range instead of containment.**
- *Fixture that separates mutant from original:* a declaration longer than `CHUNK_MAX_DECL_LINES` — generated in the hermetic test, and `big.go`'s `Table` (3..208) live. Every short declaration matches exactly and passes.
- *Observed — killed:* `Table links to "", want the span 3..42 (3e7d6f68…)` and live `big.go|var|Table|3|208 links to span "", want big.go|file||3|42 (090b8aaf…)`.
- *Compiles and vets:* yes.

**M6 — the containing span is chosen by smallest `start_line` instead of greatest.**
- *Fixture that separates mutant from original:* `TestASymbolLinksToTheMostSpecificOfTwoOverlappingWindows`, a window-strategy file whose declaration begins at line 34, inside both 1..40 and 31..70. **The AST fixture cannot see it**: there the declaration's own span is the only container.
- *Observed — killed:* `Parse links to "d09db356…", want the window that starts closest to it, 31..70 (e05ecc76…) and not 1..40 (d09db356…)`.
- *Compiles and vets:* yes.

**M7a — `CountGraph` called once per job with the edge count as its label.**
- *Observed — killed, and the plan's "check whether it compiles" is answered:* it compiles and does not panic — a `CounterVec` accepts any label value — and the kill comes from the **series set**, not the delta: `read 10 graph series, want 9`. Every series is initialised at startup, so a new one is a label outside the closed set.
- **M7b — every edge counted as `syntactic`**, the labelling-error form the plan asked for as a fallback. *Observed — killed, with the cleaner message:* `counters moved map[…{provenance="syntactic"}:4], want map[…{provenance="resolved"}:2 …{provenance="syntactic"}:2 …]`.
- *Compiles and vets:* both, the second with `_ = e`.

**M8 — the job log line drops `reason`.**
- *Why the code exists:* it is the only place an operator learns that a repository's edges are all syntactic because the toolchain is missing rather than because the code has no in-repo calls.
- *Observed — killed, six assertions:* `the log line has no "reason" field: map[edges:5 external:1 … unnameable:1]`, plus `the job log says reason <nil>, want "load_error"` and the same for the other four reasons.
- *Compiles and vets:* yes.

**M9 — the boot probe for the `go` binary removed, so `GoBin` is empty.**
- *Observed — killed:* `want {… goBin:/home/alamin/.local/go/bin/go …}, got {… goBin: …}` from `TestLimitsAreWiredToTheCapsTheyName`. And the plan's "verify what `Resolve` does with an empty `GoBin`" is answered: `Validate` refuses it with `ErrNoToolchain` and **no subprocess starts**, which is what `TestAResolverFailureLeavesEveryEdgeSyntacticAndTheJobIntact/there_is_no_toolchain` asserts with the *real* loader rather than the fake.
- **M9b — the `ix.logToolchain()` call removed from `main`.** *Observed — survivor.* Whole suite `ok`. `main` has no test harness in this binary; the function's three branches are covered, its one call site is not. Recorded rather than papered over.
- *Compiles and vets:* yes.

**M10 — `TYPECHECK=false` still runs the stage.**
- *Fixture that separates mutant from original:* a resolver that **fails the test if it is called at all** — asserting that the edges are syntactic also passes when the resolver ran and failed. It reports rather than panics, because a panic ends the binary and is an incident rather than a kill (rule 7).
- *Observed — killed, three assertions:* `the type-checker ran with TYPECHECK=false`, `the job log says reason load_error, want "disabled"`, and `counters moved map[… reason="load_error"]:1], want map[… reason="disabled"]:1]`.
- *Compiles and vets:* yes.

**M11 — `unnameable` dropped from the log line.**
- *Observed — killed:* `the log line has no "unnameable" field: map[edges:5 external:1 job:job1 level:info … reason:ok …]`.
- *Compiles and vets:* yes.

**M12 — the graph pass parses the stripped `src` rather than the raw `body`.** *(Task 3's debt 1)*
- *Why the code exists:* `Key` is a byte offset into the bytes on disk, and `StripDocs` removes the prose bytes while keeping their line terminators.
- *Fixture that separates mutant from original:* `TestTheGraphPassParsesTheRawBodyAndNotTheStrippedSource`, the **only** test in the package that runs with `STRIP_DOC_COMMENTS=true` and a resolver. Every other test has `src == body` and cannot tell the two apart. The test asserts first that stripping still moves offsets at all, so it cannot become vacuous.
- *Observed — killed:* `0 of the two calls to G resolved under STRIP_DOC_COMMENTS=true: the keys are offsets into bytes nothing else holds`. Nothing else in the suite moved — which is the whole danger: under the eval's configuration the resolution rate goes to zero with no error anywhere.
- *Compiles and vets:* yes.

**M13 — a `Target` is matched to a definition by equality with its `StartLine`.** *(Task 3's debt 2)*
- *Why the code exists:* a target is the declaration's own line; a `Def`'s range begins at its doc comment.
- *Fixture that separates mutant from original:* any documented declaration — `G` at 11..16 resolving to line 12 hermetically, and every one of the live fixture's three resolutions.
- *Observed — killed:* `the edge to G at a.go:7 points at "", want G's own id "187cb795…": the target's line 12 is inside G's range and is not its first line`, and live, all three resolutions collapse to syntactic.
- *Compiles and vets:* yes.

**M14 — `removeScratch` returns the first error instead of restoring permissions and retrying.** *(Task 3's debt 5)*
- *Why the code exists:* in proxy mode the go command writes the module cache read-only, and `os.RemoveAll` then leaks the whole job tree, not merely the cache.
- *Fixture that separates mutant from original:* a tree with a `0555` directory holding a `0400` file — **with the control in the same test**: `os.RemoveAll` must fail on it first, or a passing `removeScratch` would prove only that removing a directory works.
- *Observed — killed:* `removeScratch: unlinkat …/go/modcache/example.com/mod@v1.0.0/go.mod: permission denied`.
- *Plan defect, minor:* deleting the retry outright is a **build break** (`errors`, `io/fs` and `path/filepath` lose their only uses) and therefore void; returning the first error is the same behaviour and a one-token edit.
- *Compiles and vets:* yes, as `return err`.

**M15 — the counter's reason set loses `policy`.** *(Task 3's debt 4)*
- *Why the code exists:* `metrics` cannot import `symbols` — that would link `go/packages` into the gateway — so the vocabulary is written out twice and a test is what keeps the copy honest.
- *Observed — killed:* `metrics.TypecheckReasons is [deadline disabled load_error no_module no_toolchain ok], want symbols' own set [deadline disabled load_error no_module no_toolchain ok policy]`.
- *Compiles and vets:* yes.

**M16 — the go caches are placed inside the checkout (`filepath.Join(dir, "gohome")`).**
- *Why the code exists:* `go list ./...` walks the checkout, and in proxy mode the module cache holds `go.mod` files of its own.
- *Observed — killed:* `policy {… Home:…/job1/gohome …}, want {… Home:…/job1.gohome …}`.
- *Compiles and vets:* yes.

**M17 — the edge's tail is the file's first declaration rather than the enclosing one.**
- *Why the code exists:* a wrong tail is a wrong answer to "who calls this" that looks exactly like a right one.
- **FINDING — the hermetic fixture could not kill this, and the live one could.** Every call in the first version of `graphRepo` was inside its file's *first* declaration, so "the enclosing declaration" and "the first declaration" were the same string and `TestTheEdgeTailIsTheDeclarationTheCallIsIn` passed under the mutant. The live fixture killed it — `Base calls Sprintf-> at calc/calc.go:27`, `Count calls Count->Count at use.go:12` (a self-call that is not one). The fixture now puts a call inside `G`, the second declaration.
- *Observed — killed, after the fixture was fixed:* `F calls Print-> at a.go:14 (syntactic): the edge leaves F, want G`.
- *Compiles and vets:* yes.

**M18 — the graph write runs on `jobCtx` rather than on the record budget.**
- *Why the code exists:* the deviation above. The type-check is *supposed* to spend the job's remaining time; a write on that context turns the outcome spec:196 blesses into a failed job.
- *Observed — killed:* live, `the job failed instead of indexing: context deadline exceeded`; hermetically, `the graph write ran on the job's own budget (…): the type-check spends it`.
- *Compiles and vets:* yes.

**M19 — `CountTypecheck` is not called.**
- *Observed — killed, six assertions:* `counters moved map[…{provenance="syntactic"}:5], want map[…{provenance="syntactic"}:5 codetrail_typecheck_total{reason="load_error"}:1]` and one per reason.
- *Compiles and vets:* yes.

**M20 — the stage stops timing itself (`ObserveTypecheck` dropped).**
- *Why the code exists:* it is the only instrument that says what the precision costs.
- *Fixture:* the counter vector, which carries `codetrail_typecheck_seconds_count` **and asserts the case where it must not move** — with `TYPECHECK=false` nothing ran, so there is nothing to time.
- *Observed — killed:* `counters moved map[… resolved:2 … syntactic:3 … reason="ok":1], want map[… codetrail_typecheck_seconds_count:1 …]`.
- *Compiles and vets:* yes, with `_ = start`.

**M21 — `unparsed` dropped from the per-job line.**
- *Observed — killed:* `the unparsed count is not in the log: {"level":"warn",…,"vanished":0,"unstrippable":0,"tokenless":0,"message":"some of this repository produced no spans"}`.
- *Compiles and vets:* yes.

**M22 — the containment check loses its upper bound (`line > c.end` dropped).**
- **FINDING — a survivor turned into a kill by a fixture, and the bug it hides is a wrong citation.** With the bound gone, a definition whose first line is inside *no* span links to the nearest span that starts below it — which belongs to a **different declaration**. It survived the whole suite at first, because the greatest-start rule usually picks the right container anyway. The shape that separates them is the stripped corpus, where a span starts at the `func` keyword and the definition starts at the doc comment above it.
- *Fixture:* `TestADeclarationWithNoSpanStillGetsASymbolWithANullLink`, extended to name **`G`** as well as `F`. `F` alone cannot see it — `F` is the first declaration in the file and has nothing above it to be wrongly linked to.
- *Observed — killed, after the fixture named G:* `G links to span c9a215dd…; with the doc comment stripped its first line is inside no span, and the nearest one below it belongs to another declaration`.
- *Compiles and vets:* yes.

**M23 — every indexable file is parsed as Go (`f.Lang != ""` instead of `== "go"`).**
- *Observed — killed:* `the unparsed count is not in the log: …"unparsed":2…` — the markdown and the YAML join the count, and a repository of prose would report itself as unparseable Go.
- *Compiles and vets:* yes.

*Considered and excluded:* dropping the "first definition wins" rule in the tail map (no fixture — it needs two declarations sharing a start line, which is legal and absent from every fixture here; the same gap Task 2's M4 records for `SymbolID`); mutating `loadMode` (Task 3's, measured there); and reversing `resolved`/`syntactic` in the stage's own counters, which the whole-vector assertion already covers as a swap.

- [x] **Step 4: Commit**

**Definition of Done** — all met.
- Every call site is an edge before resolution; the edge *set* is asserted equal with and without a resolver, not merely the count (M1, and `TestEveryCallIsAnEdgeBeforeAnythingIsResolved`).
- No line in the stage copies a package-level fact onto a row, and the fixture that proves it is a repository whose type-check **failed** while three of its six edges resolved.
- A resolver failure, a missing toolchain, an expired budget, `no_module` and `TYPECHECK=false` each produce a complete graph of syntactic edges, a distinct reason in the log, a counted outcome and a `done` job — five subtests, each asserting the whole counter delta.
- The stage runs on `jobCtx`, proven hermetically by the deadline it was handed and live by a job whose budget is spent in `putSpans` before it starts, with the control in the same test. **The write does not**, which is a recorded deviation with M18 as its proof.
- Symbols link to the containing span, most specific first, with a window-overlap fixture for the tie-break and a stripped-corpus fixture for the upper bound.
- The job log names symbols, edges, the provenance split, `external`, `unnameable` and `reason`; `unparsed` joins the existing soft-loss line.
- M1–M23 recorded with observed output, M9b recorded as a survivor with its reason, M3 recorded as a void mutation with the two variants that are not.

---

### Task 5: The graph reads — a recursive CTE, and the set it refuses to merge

"Who calls this" is a recursive CTE. That sentence is in `infra/docker-compose.yml`'s first comment as the reason this project has one datastore; this task is where the claim is either true or an excuse.

**Files:**
- Create: `packages/shared/store/graph_read.go`
- Modify: `packages/shared/store/read.go` (`Stats` grows the graph counts)
- Test: `packages/shared/store/graph_read_live_test.go` (`//go:build live`)
- Not modified, and it is Task 6's: `apps/gateway/internal/handler/read.go` still serves three of `Stats`'s seven numbers. The four new ones have no consumer until the endpoints land, which is where open question 13's decision belongs.

**Interfaces:**
- Produces:
  - `type Caller struct { Symbol models.Symbol; Depth int; CallPath string; CallLine int }`
  - `type Approximate struct { Symbol models.Symbol; ToName, CallPath string; CallLine int }`
  - `func (s *Store) Definitions(ctx context.Context, repoID, name string, suffix bool, limit int) ([]models.Symbol, error)`
  - `func (s *Store) Symbol(ctx context.Context, repoID, symbolID string) (models.Symbol, error)`
  - `func (s *Store) CallersOf(ctx context.Context, repoID, symbolID string, depth, limit int) ([]Caller, error)`
  - `func (s *Store) ApproximateCallersOf(ctx context.Context, repoID, name string, limit int) ([]Approximate, error)`
  - `store.Stats` gains `Symbols, Edges, EdgesResolved, EdgesSyntactic int`
- Also produced, because four readers select the same ten columns and two spellings of that list would swap `path` and `name` silently — both are text and the scan would still succeed: one `symbolCols` constant and one `scanSymbol`, which is also where nullable `span_id` becomes the empty string once instead of at four call sites.

**Decisions, with their reasoning:**

- **`CallersOf` traverses `to_symbol_id` and nothing else.** A join from a syntactic edge's `to_name` to a `symbols.name` is the guess spec:84 forbids, made at query time where no column records that it happened. It would also be *invisible* in any corpus with unique names, which is most small fixtures and no real repository.
- **The approximate set is a separate query, returned separately, at depth 1 only.** It answers a different question — "what else in this repository calls something with this name" — and merging the two would produce a "callers" list whose members are partly facts and partly coincidences, with no way for a caller to tell which is which. Depth 1 because traversing *from* a guess compounds it: at depth 2 the result would be "things that call something that might be this".
- **The cycle guard is an explicit `path` array with `NOT from_symbol_id = ANY(path)`, not SQL's `CYCLE` clause.** ~~The array is needed anyway to *report* the chain, and one mechanism doing both is one thing to get wrong.~~ **False rationale, and this task's own interface is the proof: `Caller` has no field for the chain, so nothing reports it and the array's only job is the guard.** The array is still the right mechanism — `CYCLE` needs a `SET`/`USING` pair that carries the same array under another name — but the reason is that it is one expression rather than that it is two features. Recursion in a call graph is not an edge case: it is `f` calling `f`, mutual recursion between two helpers, and any interpreter or tree walker in the corpus.
- ~~**The depth bound and the cycle guard are two independent controls and each is separately killable.**~~ **False rationale, measured. The cycle guard cannot change this query's rows at all.** `min(depth)` is the BFS distance, and a walk that revisits a node is never shorter than the simple path that does not, so the guard changes reachability-within-depth by nothing and the aggregate by nothing. Confirmed: under M2 every row assertion in the suite passed. What the guard changes is how much work the traversal does — 19 CTE rows guarded against 46 unguarded, on eight definitions at depth 5 — so what reads it back is `TestTheCycleGuardBoundsTheTraversalItselfLive`, which `EXPLAIN (ANALYZE)`s the shipped statement and asserts the `Recursive Union`'s actual row count. **Removing both is a query that does not terminate, which rule 6 makes a non-mutation**, so it is excluded rather than attempted — and it happened by accident anyway, which is recorded under the defects below.
- **A diamond is deduplicated with `min(depth)`.** Two paths of different lengths to the same caller are one caller, at its shortest distance. Without the aggregation the same symbol appears twice, which is not an infinite loop and not an error — it is a plausible-looking wrong answer, and it is why the fixture has a diamond in it.
- **`Definitions` matches `name` exactly by default.** The corpus spells a method `Store.Get`, P3's lexical arm reaches it through its parts, and a caller who types `Get` should not silently get every `Get` in the repository presented as though they had asked for it. `suffix=true` is the opt-in. The suffix arm is `right(name, char_length($2) + 1) = '.' || $2`, not `LIKE '%.' || $2`: a name carrying `%` or `_` would turn the pattern into a wildcard, which is the silent widening the signature exists to refuse.
- **`Stats` grows the graph counts so the repo view can show the provenance split.** A per-repo *summary* is not a per-repo *label*: the column stays per row, and the summary is an aggregate over it, which is what makes "three packages resolved and two did not" visible to a user at all. Each label is counted by its own predicate rather than one as the total minus the other, because open question 8 wants a third label for a call that resolves outside the corpus and a subtraction would file it under `syntactic` with nothing looking wrong.
- **The store does not validate `depth`.** The anchor is unconditional, so `depth=0` still returns the direct callers; out of range is a `400` at the endpoint (open question 1), and a store that clamped would hide the endpoint's bug rather than fix it.

- [x] **Step 1: Write the failing live tests**

The fixture is the load-bearing part of this task and every rule in "Fixtures that can tell a graph bug" applies at once. Two repositories; in the target repo, one package with:

```
outer ─▶ main ─▶ a ─▶ b ─▶ target   a chain, so depth is observable
         main ─▶ c ─▶ target        a diamond: two routes to target
                target ─▶ target    a direct self-call
         d ─▶ e ─▶ d, e ─▶ target   a two-node cycle reaching target
Store.Get and Cache.Get             two definitions sharing a last segment
f ─▶ "Get" twice, f ─▶ "target"     syntactic edges, null target
g ─▶ Store.Get, h ─▶ Cache.Get      resolved edges naming Get
```

and, in the **other** repo, a symbol also named `target` with **more** callers than the target repo's (six to four), so a missing repo filter changes the answer rather than adding a row. Four differences from the fixture as the plan drew it, each because the drawn one could not fail under a bug it was named for:

- **`outer` is new, and without it M1 survives at every depth above 1.** The diamond puts `main` at depth 2 by the short route, so in the plan's fixture nothing is reachable *only* at depth 3 — and `min(depth)` then hides the extra level an off-by-one adds. Measured: with the plan's fixture, `<` → `<=` at `depth=2` returns the identical row set. `outer → main` is the one node whose only route is the long one.
- **The callers are split across `z.go` (the near ones) and `a.go` (the far ones)**, so the expected `(depth, path, start_line)` order disagrees with path order. In one file the two orders are the same and M12 proves nothing.
- **`main`'s two hops are on two lines** (`a.go:8` to `a`, `a.go:9` to `c`), so the diamond's call-site assertion can say *which* hop was cited. On one line it cannot.
- **`f` calls something named `Get` twice.** `ApproximateCallersOf`'s `ORDER BY` carries the call site precisely because one caller can hold two sites; with one site in the fixture that clause was a side effect nothing read back.

The syntactic edges are named after a **plain function** (`f → "target"`, null target) as well as after a method. That is what makes M7 dangerous rather than merely wrong: `chunk.classify` spells a method `Store.Get` while an edge names the callee's last identifier `Get`, so a name join against a *method* matches nothing and returns an empty answer, while a name join against a *function* silently admits a caller that guessed.

```go
func TestCallersOfWalksTheChainAndReportsDepthLive(t *testing.T)
func TestCallersOfTerminatesOnASelfCallLive(t *testing.T)
func TestCallersOfTerminatesOnATwoNodeCycleLive(t *testing.T)
func TestTheCycleGuardBoundsTheTraversalItselfLive(t *testing.T)   // new: nothing in the row set can read the guard back
func TestADiamondYieldsOneCallerAtItsShortestDepthLive(t *testing.T)
func TestCallersOfStopsAtTheRequestedDepthLive(t *testing.T)
func TestCallersOfNeverIncludesASyntacticEdgeLive(t *testing.T)
func TestCallersOfReportsTheCallSiteOfEachHopLive(t *testing.T)
func TestApproximateCallersAreNameMatchedAndSeparateLive(t *testing.T)
func TestApproximateCallersExcludeResolvedEdgesLive(t *testing.T)
func TestDefinitionsMatchesExactlyUnlessSuffixIsAskedLive(t *testing.T)
func TestDefinitionsIsScopedToOneRepoLive(t *testing.T)
func TestSymbolIsScopedToItsRepositoryLive(t *testing.T)           // new: Task 6's 404 reads this
func TestRepoStatsSplitsEdgesByProvenanceLive(t *testing.T)
```

(The `Live` suffix is this package's convention, not the plan's spelling.) `TestCallersOfWalksTheChainAndReportsDepth` asserts the **whole ordered set with each row's depth** — `[b:1 c:1 e:1 target:1 a:2 main:2 d:2 outer:3]` — plus that this order is neither path order nor id order, plus one caller's whole `models.Symbol`, since a row Task 6 serialises half-filled is uncitable. `TestADiamondYieldsOneCallerAtItsShortestDepth` asserts the count, the depth *and* the cited line.

- [x] **Step 2: Implement**

As sketched, with three changes. `s.repo_id` and `s.file_id` join the select list, because the sketch's ten columns build a `models.Symbol` with two empty fields. The `GROUP BY` is `s.id` alone — Postgres's functional-dependency rule covers the rest, and the long list is a list to keep in agreement with the select. And `ApproximateCallersOf` orders by the call site after the symbol, because two sites in one function are two rows whose order must not be the planner's.

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
SELECT s.id, s.repo_id, s.file_id, s.path, s.name, s.pkg, s.kind,
       s.start_line, s.end_line, s.span_id,
       min(c.depth) AS depth,
       (array_agg(c.call_path ORDER BY c.depth, c.call_path, c.call_line))[1] AS call_path,
       (array_agg(c.call_line ORDER BY c.depth, c.call_path, c.call_line))[1] AS call_line
FROM callers c JOIN symbols s ON s.id = c.sym
GROUP BY s.id
ORDER BY depth, s.path, s.start_line, s.id
LIMIT $4
```

The approximate query is separate and deliberately dull:

```sql
SELECT s.*, e.to_name, e.path, e.line
FROM edges e JOIN symbols s ON s.id = e.from_symbol_id
WHERE e.repo_id = $1 AND e.to_symbol_id IS NULL AND e.to_name = $2
ORDER BY s.path, s.start_line, s.id, e.path, e.line, e.id
LIMIT $3
```

- [x] **Step 3: Commit, then prove the tests discriminate**

Sixteen mutations. **Thirteen killed, two recorded as survivors, one void in the form the plan spells it and killed in the form it names as the rewrite.**

**M1 — `c.depth < $3` → `c.depth <= $3`.** Killed.
- *Why the code exists:* the bound is the caller's, and an off-by-one on a graph traversal is a fan-out, not a row.
- *Fixture that separates mutant from original:* the chain queried at `depth=1` **and** at `depth=2`. The plan said "the four-deep chain at `depth=2`, a chain shorter than the bound cannot see it", and with the plan's own fixture that is wrong: `min(depth)` collapses the extra level wherever the added node is already reachable sooner, and only `outer` is not.
- *Must fail:* `TestCallersOfStopsAtTheRequestedDepthLive`
- *Observed:* `depth=1 returned [b:1 c:1 e:1 target:1 a:2 main:2 d:2], want [b:1 c:1 e:1 target:1]` and `depth=2 returned [b:1 c:1 e:1 target:1 a:2 main:2 d:2 outer:3], want [b:1 c:1 e:1 target:1 a:2 main:2 d:2]`
- *Compiles and vets:* yes.

**M2 — the cycle guard replaced by `TRUE`.** Killed, by the cost assertion only.
- *Why the code exists:* recursion is normal in a call graph, and without the guard the traversal explores every *walk* rather than every *path*, which is exponential in the fan-in.
- *Fixture that separates mutant from original:* the `d ─▶ e ─▶ d` cycle and the `target ─▶ target` self-call — but **not through any row the query returns.** Every row assertion in the suite passed under this mutant, including the two the plan named. `min(depth)` does not "partially mask" it, it masks it completely, for the reason under Decisions. `TestTheCycleGuardBoundsTheTraversalItselfLive` is what separates them.
- *Must fail:* `TestTheCycleGuardBoundsTheTraversalItselfLive` — **not** `TestCallersOfTerminatesOnATwoNodeCycleLive`, which passes.
- *Observed:* `the traversal produced 46 rows at depth 5, want 19` (and, second, `the counterfactual rewrote nothing: callersSQL no longer contains "AND NOT e.from_symbol_id = ANY(c.path)"`).
- *Compiles and vets:* yes.

**M3 — the depth bound replaced by `($3 >= 0 OR TRUE)`.** Killed.
- *Why the code exists:* it bounds the work and it is the caller's parameter.
- *Fixture that separates mutant from original:* the chain at `depth=1`. The cycle guard keeps it terminating, so this is a finite wrong answer.
- *Must fail:* `TestCallersOfStopsAtTheRequestedDepthLive`
- *Observed:* `depth=1 returned [b:1 c:1 e:1 target:1 a:2 main:2 d:2 outer:3], want [b:1 c:1 e:1 target:1]`
- *Compiles and vets:* yes. The plan's suggested spelling `($3 IS NOT NULL OR TRUE)` was not needed; `$3 >= 0` keeps the parameter bound *and* typed, where a bare `IS NOT NULL` leaves pgx nothing to infer `int4` from.
- *This is the mutation that produced the phase's one accidental non-terminating query*, recorded under the defects.

**M4 — `min(c.depth)` → `max(c.depth)`.** Killed.
- *Why the code exists:* a caller reachable by two paths is one caller, at its shortest distance; the longer path is a fact about the graph, not about the caller.
- *Fixture that separates mutant from original:* the diamond, where `main` is reachable at depth 2 and, once the self-call lengthens a route, at depth 4.
- *Must fail:* `TestADiamondYieldsOneCallerAtItsShortestDepthLive`
- *Observed:* `main at depth 4, want depth 2 (main → c → target, not main → a → b → target)`; the whole-set assertion reads `[target:1 b:2 c:2 e:2 a:3 d:3 main:4 outer:5]`. The predicted `depth 3` was one short: the self-call `target → target` adds a hop to every long route.
- *Compiles and vets:* yes.

**M5 — the `GROUP BY` dropped (the aggregate becomes a plain select).** Void as the plan spells it; killed as the rewrite.
- *Why the code exists:* the same caller reached twice is one row.
- *M5a, dropping `GROUP BY s.id` alone:* **void, confirmed.** `ERROR: column "s.id" must appear in the GROUP BY clause or be used in an aggregate function (SQLSTATE 42803)`, which arrives as a `t.Fatal` in the test's helper — a SQL error, not a row change (rule 2).
- *M5b, dropping the aggregation and the `min` together and selecting `c.depth`, `c.call_path`, `c.call_line` directly:* killed, and it is the mutant a person would write.
- *Must fail:* `TestADiamondYieldsOneCallerAtItsShortestDepthLive`
- *Observed:* `main is in the answer 4 times, at depths [main:2 main:3 main:3 main:4]`, and the whole set becomes the CTE's 19 rows: `[b:1 c:1 e:1 target:1 a:2 main:2 d:2 b:2 c:2 e:2 a:3 main:3 main:3 d:3 outer:3 main:4 outer:4 outer:4 outer:5]`. Four other tests fail with it, including `main cites "a.go:8", want "a.go:9"`.
- *Compiles and vets:* both forms compile; only M5b is a behaviour change.

**M6 — the recursive term joins `e.from_symbol_id = c.sym` (direction inverted).** Killed.
- *Why the code exists:* callers, not callees. The two queries are one character apart and both return plausible graphs.
- *Fixture that separates mutant from original:* the chain, whose transitive callers `a`, `main`, `d` and `outer` all disappear. The plan's stated reason — "`target` has callers and no callees" — is **wrong about the plan's own fixture**, which gives `target` a self-call and therefore a callee.
- *Must fail:* `TestCallersOfWalksTheChainAndReportsDepthLive`
- *Observed:* `callers of target: [b:1 c:1 e:1 target:1], want [b:1 c:1 e:1 target:1 a:2 main:2 d:2 outer:3]` — the recursion returns **nothing at all**, not the callee walk the plan predicted, because the inverted term selects `e.from_symbol_id` where `e.from_symbol_id = c.sym`, so every candidate row repeats `c.sym` and the cycle guard rejects it. Six tests fail; the sharpest is `d cites "", want "a.go:12"`.
- *Compiles and vets:* yes.

**M7 — the anchor drops `AND e.to_symbol_id = $2` in favour of `e.to_name = (SELECT name FROM symbols WHERE id = $2)`.** Killed.
- *Why the code exists:* spec:84 — a syntactic edge's name is not a target, and binding it at read time is the guess the null column exists to refuse.
- *Fixture that separates mutant from original:* `f`, whose syntactic edge names `target`; and the two `Get` methods, which show the same mutation's other half. The plan's rule-4 fixture alone would not have caught the dangerous direction: an edge names the callee's *last identifier*, so a name join against `Store.Get` matches nothing.
- *Must fail:* `TestCallersOfNeverIncludesASyntacticEdgeLive`
- *Observed:* `callers of target include f, which calls something named target with a null target (cache.go:9)` and `callers of Store.Get: [], want [g:1] — f calls a null target named Get and h calls Cache.Get`. Both halves fired, and the traversal row count moved 19 → 20.
- *Compiles and vets:* yes.

**M8 — `ApproximateCallersOf` drops `AND e.to_symbol_id IS NULL`.** Killed.
- *Why the code exists:* the approximate set is what is *not* known; a resolved edge in it double-counts a caller that is already in the precise answer.
- *Fixture that separates mutant from original:* `g → Store.Get` and `h → Cache.Get`, both resolved and both naming `Get`, beside `f`'s two syntactic ones.
- *Must fail:* `TestApproximateCallersExcludeResolvedEdgesLive`
- *Observed:* `approximate callers of Get: [f f h g], want [f f] (g and h resolve)`, and in the other test `[f@cache.go:8 f@cache.go:10 h@cache.go:12 g@store.go:8], want [f@cache.go:8 f@cache.go:10]`. The scoping half fires too — `approximate callers of target in repo A: [f b c e target], want [f]` — because every resolved edge into `target` also *names* it.
- *Compiles and vets:* yes.

**M9 — `Definitions` matches with `suffix` always on.** Killed.
- *Why the code exists:* an exact request that silently widens is a different question answered without saying so.
- *Fixture that separates mutant from original:* `Store.Get` and `Cache.Get`, queried as `Get` with `suffix=false`, which must return **nothing**.
- *Must fail:* `TestDefinitionsMatchesExactlyUnlessSuffixIsAskedLive`
- *Observed:* `exact match for "Get" returned 2 definitions [Cache.Get Store.Get], want 0`
- *Compiles and vets:* yes.

**M10 — `Definitions` drops the repo filter (`repo_id = $1 OR TRUE`).** Killed.
- *Why the code exists:* names collide across repositories by construction; `parseConfig` exists in every second Go repository.
- *Fixture that separates mutant from original:* the second repository's `target`.
- *Must fail:* `TestDefinitionsIsScopedToOneRepoLive`
- *Observed:* `definitions of target: 2, want 1 (the extra is from repo B)`, with both rows printed so the message names which repo each came from.
- *Compiles and vets:* yes — the parameter stays bound.

**M11 — `CallersOf` drops the repo filter (`repo_id = $1 OR TRUE`) in both terms.** **Survivor, as predicted.**
- *Why the code exists:* defence in depth.
- *Observed:* the whole suite passes. `SymbolID = hash(repo, path, start, kind, name)`, so a symbol id never collides across repositories, `to_symbol_id = $2` already pins the repo, and the recursive term joins on ids for the same reason. Keep the clause: the day a symbol id becomes anything but a repo-scoped hash, the filter is what stops a cross-repo traversal, and the *reason* it cannot be killed today is the reason it is cheap to keep.
- *Compiles and vets:* yes.

**M12 — the final `ORDER BY` dropped.** Killed.
- *Why the code exists:* two runs over an unchanged corpus must be diffable, and the `LIMIT` makes the order decide *which* callers a caller sees.
- *Fixture that separates mutant from original:* the callers split across two files so depth order and path order disagree — asserted in the test body, not assumed.
- *Must fail:* `TestCallersOfWalksTheChainAndReportsDepthLive`
- *Observed:* `callers of target: [outer:3 target:1 d:2 c:1 main:2 e:1 b:1 a:2]`. **A third measurement to add to P3's two:** the unordered result is neither path order nor insertion order nor id order — it is hash-aggregate order, which resembles nothing a test could have predicted. The `LIMIT`-below-the-result assertion never ran, since the whole-set one fails first.
- *Compiles and vets:* yes.

**M13 — `RepoStats` counts edges without the provenance split (both counts from the same total).** Killed.
- *Why the code exists:* the split is the phase's headline claim and the repo view is where a user sees it.
- *Fixture that separates mutant from original:* both repositories carry both kinds.
- *Must fail:* `TestRepoStatsSplitsEdgesByProvenanceLive`
- *Observed:* `repo A stats {Files:4 Spans:0 FilesWithSpans:0 Symbols:13 Edges:15 EdgesResolved:15 EdgesSyntactic:15}, want {… EdgesResolved:12 EdgesSyntactic:3}`, and repo B at 9/9/9 against 6/3.
- *Compiles and vets:* yes.

Three more from the whole-reader sweep, since every survivor P3 found late was either a side effect nothing read back or an unexercised branch:

**M14 — `Symbol` drops the repo filter.** Killed. `repo A's target read from repo B returned <nil>, want ErrNotFound` (`TestSymbolIsScopedToItsRepositoryLive`).

**M15 — `symbolCols` swaps `s.repo_id` and `s.file_id`.** Killed. Both are text, so the scan succeeds and the values land in each other's fields: `read back {… RepoID:280bc6f… FileID:c334f9… }, want {… RepoID:c334f9… FileID:280bc6… }`. This is the mutation that proves the shared column list is read back **in full** rather than through the four fields the caller assertions name — `Pkg`, `SpanID`, `FileID` and `RepoID` are observed only by the whole-struct compares in `TestSymbolIsScopedToItsRepositoryLive` and `TestCallersOfWalksTheChainAndReportsDepthLive`.

**M16 — `ApproximateCallersOf` drops the `e.path, e.line, e.id` tiebreak.** **Survivor.** With two rows the planner returns them in the same order either way, and forcing it to do otherwise would need a corpus rather than a fixture — the same thing P3 measured about its unordered results. The clause is a determinism guarantee about a repository, not about this fixture; kept, and recorded as unkillable at this size rather than as covered.

- [x] **Step 4: Commit**

**Definition of Done**
- [x] `CallersOf` terminates on a self-call, on a two-node cycle and on a diamond, with the row set, the depths and the call sites all asserted.
- [x] Each of the two termination controls is killed by a mutation while the other holds — **the cycle guard only through the traversal's row count, because it cannot change the answer**; removing both is recorded as excluded, and the one time it happened by accident is recorded too.
- [x] No syntactic edge reaches the traversal, proven against a fixture with two definitions sharing a last segment **and** a syntactic edge naming a plain function, which is the half that can actually admit one.
- [x] The approximate set is name-matched, depth-1, separate, and excludes resolved edges.
- [x] `Definitions` is exact by default and scoped to one repo. **It does not say which match it made:** the interface returns `[]models.Symbol`, which has no room for it, and open question 11 puts that on the response — so it is Task 6's to echo, per row if a suffix search is to distinguish an exact hit from a suffix hit.
- [x] `RepoStats` reports the provenance split from a repo carrying both.
- [x] M1–M16 recorded with observed output; M5's void form and its rewrite recorded; M11 and M16 recorded as survivors with their reasons.

**Plan defects found by this task**

1. **Two false rationales**, both under Decisions above: the `path` array is not "needed anyway to report the chain" (nothing reports it), and the two termination controls are not "each separately killable" in the answer (the guard cannot change a row).
2. **M2's fixture claim is wrong in the direction that matters.** "`min(depth)` collapses some of it" understates it: the aggregation masks the mutant entirely, and both tests the plan names for it pass. Without the cost assertion this task would have shipped an unkillable cycle guard and called it covered.
3. **M1's fixture cannot fail at the depth the plan names.** The plan's own diamond puts `main` at depth 2, so at `depth=2` the off-by-one adds no row. Fixed by adding `outer`.
4. **M6's stated reason is wrong about the plan's own fixture** — `target ─▶ target` is a callee — and its predicted output is wrong about the mechanism: the inverted join returns nothing, because the cycle guard rejects the row it produces.
5. **M4's predicted depth was one short** (3, observed 4): the self-call lengthens every route through `target`.
6. **M7's mutant is harmless against a method and dangerous against a function.** `chunk.classify` spells `Store.Get` and an edge names `Get`, so rule 4's two-methods fixture proves the *weaker* half. A syntactic edge naming a plain function is what the rule should ask for.
7. **The counterfactual in the cycle-guard test can build the query the plan excludes.** It strips the guard from whatever `callersSQL` currently is, so with M3 applied it had neither control and ran for two minutes with nothing to report. Now under `SET LOCAL statement_timeout`, where an unbounded traversal is a message. Rule 6 is about the tests a plan ships; it applies to the counterfactuals inside them too.
8. **The sketch builds a half-filled `models.Symbol`** — no `repo_id`, no `file_id` — which Task 6 would have serialised.
9. **`Stats` grew four fields with no consumer.** Task 5's file list excludes the gateway and open question 13 wants them on `GET /api/repos/:repo`; Task 6 owns closing that, or they are four numbers nothing reads.

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
- [x] Edges are attempted through `go/packages` with type information; where `types.Info.Uses` resolves a call to an object **that has a symbol row in this repository**, the edge is `resolved` and points at it; everywhere else it is `syntactic` with a null target, enforced by a CHECK constraint rather than by convention. *(Tasks 2, 3 and 4.)*
- [x] Provenance is per row. One repository carries both labels, and a fixture with two packages — one loadable, one not — pins it by naming an edge in each. No line of code copies a package-level fact onto a row. *(Task 4. The live fixture is stronger than the plan asked for: its type-check **fails**, and two of its three resolved edges are inside the package that failed.)*
- [x] The type-check runs inside the sandbox, on the job's own remaining budget, with an environment allowlist that a hostile parent environment cannot widen and that makes a recording proxy receive zero requests. *(Task 3. The zero is paired with a control that fetches, so it is evidence rather than an absence.)*
- [x] A type-check failure — missing toolchain, absent module, expired budget, disabled by knob — produces a complete graph of syntactic edges, a distinct counted reason, a log line, and a `done` job. *(Task 4, five reasons, each asserting the whole counter delta.)*
- [x] "Who calls this" is a recursive CTE that terminates on self-calls, cycles and diamonds, reports depth and call sites, and never traverses a name. *(Task 5. The cycle guard turned out to be unobservable in the answer — `min(depth)` is the BFS distance — so what pins it is the traversal's own row count under `EXPLAIN (ANALYZE)`: 19 guarded, 46 unguarded.)*
- [ ] Approximate, name-matched callers are a separate labelled set at depth 1, with their own count. *(Task 5 ships the set, proven separate and free of resolved edges; the count is a field in a response and belongs to Task 6.)*
- [ ] Graph endpoints answer `404` for an unknown symbol, `410` for an evicted repo, `400` naming the rule for a bad `depth`, `limit` or `name`, and `500` opaquely with a request id; none of them touch the answer or refusal counters.
- [x] Eviction takes the graph with it, in one `DELETE`. *(Task 2 at the schema, Task 4 end to end through a real job.)*
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
   *Recommendation:* **`GOPROXY=off` by default**, with `GOVCS=*:off`, `GOTOOLCHAIN=local`, `GOWORK=off`, `GOENV=off`, **`GOPACKAGESDRIVER=off`**, `CGO_ENABLED=0`, scratch-local `GOMODCACHE`/`GOCACHE`/`GOPATH`/`GOTMPDIR`, and an environment allowlist rather than an inherited environment. (`GOPACKAGESDRIVER=off` was added in Task 3: without it `go/packages` runs a binary named `gopackagesdriver` found on the *operator's* `PATH` in place of the go command. Task 3 also found that the settings closed **by omission** — `GOFLAGS`, `GOPRIVATE`, `GOROOT`, `GOEXPERIMENT`, `GODEBUG`, `LD_PRELOAD`, `HTTPS_PROXY` — are the ones a mutation can be built against, and that naming them with a safe value would make the allowlist untestable.) An operator can set `TYPECHECK_GOPROXY` to a proxy they trust — wired in Task 4 and refused **at boot**, which is the line between the two failures: a missing toolchain is the operator's environment and downgrades a label, while a proxy naming `direct` is a setting that would turn a stranger's `go.mod` into an outbound connection of their choosing, so the first must not stop a worker and the second must not start one. `direct` and any `,direct` fallback are refused, because that is the setting that turns a stranger's `require` line into an outbound `git` to a host of their choosing. The cost is stated plainly: with the default, only standard-library-only packages type-check, and everything else is honestly syntactic.

5. **What `pkg` means.** §3 lists the column and never defines it.
   *Recommendation:* the **package clause name** from the AST (`store`), not the import path. The import path needs a module-aware load, which is the thing that is allowed to fail; a column whose meaning depended on whether the type-check ran would be a per-package fact leaking into a per-row column, which is the failure §6 is written against. Two packages named `store` in one repository are distinguished by `path`. Revisit if a console needs import paths.

6. **`edges` and `symbols` carry columns §3 does not list** — `path` and `line` on an edge, `start_line`, `end_line` **and `path`** on a symbol. (`symbols.path` was missing from this list while the plan's own `SymbolID` signature hashed it; Task 2 added it and recorded it. It is denormalised beside `file_id` exactly as `spans.path` already is, and without it a caller row cannot cite a definition without joining `files`.)
   *Recommendation:* add them, and say so. Without the edge's location, "who calls this" answers with a symbol and no `file:line`, in a product whose first sentence is that its answers cite `file:line`. Without the symbol's range, a definition with no span is uncitable. The alternative — deriving both by joining spans — fails for exactly the rows where the link is null.

7. **`imports` and `references` edges.** §3's `kind` enum has three values; §6 describes only calls.
   *Recommendation:* **calls only in P4**, with the other two values valid and unwritten. `imports` cannot be written under the current schema at all: `from_symbol_id` is `NOT NULL` and an import belongs to a *file*, not to a definition, so writing one means either a nullable tail or a synthetic file-level row in a table §3 defines as "one row per definition". Both are schema changes with consequences for every query in Task 5, and neither is needed to answer "who calls this". `references` is a volume decision — it is roughly every identifier use in the corpus — and should be made with a measured row count, not before.

8. **A call that type-checks to something outside the corpus.** `fmt.Println` resolves to a real object; there is no symbol row to point at.
   *Recommendation:* record it as `syntactic` with a null target, because spec:84's invariant ("a null target is what makes the label mean anything") is the one that cannot be bent, and count it separately as `external` so the fact is not lost. A third enum value would be more informative and §3's enum has two. Revisit if a console wants to distinguish "we could not tell" from "it is not in this repository" — the counter already knows.

9. **The indexer needs a Go toolchain at runtime.** It already forks `git`; this adds `go`, which is a much larger dependency.
   *Recommendation:* probe with `exec.LookPath` at boot, log one line when it is absent, count `no_toolchain` per job, and **do not refuse to boot** — an indexer that cannot type-check still indexes, and refusing would make a missing toolchain worse than a syntactic graph. P8's image must include a toolchain, and the README says so, because the failure mode otherwise is CI resolving and production not, which is the silent downgrade §8 calls the failure that costs a week.

10. **`go/packages` gives no hook to put its subprocess in a process group, and its cache is not covered by any cap.** P1 kills the whole process group on deadline; `packages.Load` runs `go list` through its own `exec.Command` and the plan cannot reach it. Separately, `MAX_REPO_BYTES` is measured on the clone, and `GOCACHE`/`GOMODCACHE` are written afterwards.
    *Answered in Task 4 for the third item, and it needed code rather than a note:* `removeScratch` restores the directories' write permission and retries, because `os.RemoveAll` over a read-only module cache leaks **the whole job tree**, not merely the cache. The caches also moved *beside* the checkout rather than under it — the job's scratch directory **is** the checkout, and `go list ./...` walks whatever is inside it.
    *Recommendation:* accept both for P4 and write them down. **Measured in Task 3 and it changes the size of the second half:** with `NeedDeps` the only subprocess is `go list`, and the caches a job leaves behind are ~0.7MB rather than the ~92MB `-export=true` writes for a package importing `net/http`. A third item joins the list: in proxy mode the module cache is written **read-only**, so `runJob`'s `defer os.RemoveAll(dir)` fails with `permission denied` and the scratch directory leaks. Not reachable under the shipped default, where nothing is fetched. The blast radius is bounded in practice — the caches live under the job's scratch directory, which `runJob` removes and which the worker sweeps at boot and exit — but "bounded in practice" is not "enforced", and the honest form is a README line plus the `TYPECHECK=false` kill switch. Revisit by measuring cache growth over a real corpus; if it matters, the fix is a `du` check after the stage, matching the clone's, or a driver we fork ourselves.

11. **Last-segment matching for `definition_of`.** P3's lexical arm reaches `Store.Get` through its parts, so a user who found a method by searching will type `Get`.
    *Recommendation:* exact by default, `suffix=true` to opt in, and the response says which match it made. A silent fallback would answer a different question than the one asked and would be indistinguishable in the payload. Revisit in P5 when the console knows what a user actually types.

12. **Where the graph pass gets its bytes.** The chunker may read *stripped* source; `go/packages` reads from disk.
    *Recommendation as written was wrong and Task 1 measured it so.* `StripDocs` removes the prose bytes and keeps only their line terminators: line numbers are invariant, **byte offsets are not**. The answer is still "both, unchanged, with no `ParseFile` hook", for a different reason — **the graph pass must parse the same bytes `go/packages` reads, which are the ones on disk**. That is a constraint on the *caller*, so a hook cannot enforce it; Task 3 pins it with `TestResolutionKeysAreOffsetsIntoTheBytesOnDisk`, which asserts that raw offsets hit and stripped ones do not, and Task 4 must pass the indexer's raw `body`.

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
- **§6 says type-checking "requires module downloads" and §4 says the sandbox makes no network assumptions and admits only allowlisted hosts.** These are in direct tension, and §6 does not resolve it: the hosts a module download reaches are chosen by a stranger's `go.mod`, which is the exact shape of the SSRF §4's allowlist exists to refuse. This plan resolves it by defaulting the fetch off and making the resulting failure a first-class, counted outcome — which §6 already blesses. It is worth noticing that §6's own sentence ("its failure is expected rather than exceptional") is what makes the safe choice affordable. **Task 3 measured the cost of the other choice:** with the entry removed from the allowlist, the go command falls back to `https://proxy.golang.org,direct` and a fixture requiring `golang.org/x/mod` type-checks — the SSRF, executed.
- **"It runs inside the same sandbox" describes a sandbox that is git-shaped.** P1's controls are `GIT_*` variables, a process group and a wall-clock deadline; none of them constrain a compiler. Running `go` on a stranger's tree adds cgo directives, `go.work` files, toolchain directives and a build cache to the threat surface, and each needs its own control. §6 assumes a sandbox that is more general than the one that exists.
- **§11's counters still have nowhere to live in the indexer.** P3 recorded this; P4 adds four more instruments to the same gap. The indexer has no HTTP server, so job outcomes, admission rejections, evictions and now type-check outcomes are written and unscrapable. Giving the indexer a probe server is P0-shaped work and belongs in its own task rather than being smuggled in here.
- **§8's tool list implies a graph API shape that §3's data model does not describe.** `definition_of` and `callers_of` are named as *tools*, with no routes, no parameters and no notion of depth or of what happens to a name that matches two definitions. Everything about the endpoint surface in Task 6 is a decision this plan made, and the open questions say which.
- **§13 puts the console in P5 and the LLM loop in P7, so P4's endpoints have no consumer for two phases.** That is a real risk of designing the wrong API: the response shapes here are guesses about what a console needs. They are kept close to P3's existing views to reduce the guess, and Open question 11 and 13 name the two places P5 is most likely to want a change.
- **`Evict`'s doc comment already promises the cascade this phase must join** — "whatever P3's symbol graph adds by declaring the same reference" — and it names the wrong phase. The symbol graph is P4. Fix the comment in Task 2's commit; P2's review round found twelve comments that had drifted, two of them introduced by commits fixing others.
