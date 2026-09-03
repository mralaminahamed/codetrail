# The corpora, named before the run

Three repositories, commit-pinned, chosen and written down **before any of them
was measured**, so the calibration cannot be the repository that gave the
nicest number. The commit that added this file predates every number in
`runs/`.

**Every entry here is accounted for.** A repository that produced no numbers
says so, with the evidence, and is never simply absent —
`TestEveryNamedCorpusHasACommittedRun` reads this file, not the `runs/`
directory: a test that reads the directory agrees with whatever is there,
which is the definition of no test at all. It also refuses the other
direction: a corpus recorded as refused must have no published run.

A floor calibrated on one repository is a floor calibrated on one library's
documentation style. Three is not statistics either, and the README says so —
the honest claim is the per-repository spread, not a confidence interval nobody
computed.

| Repository | Commit | Why this one | Result |
| --- | --- | --- | --- |
| `https://github.com/google/uuid` | `2d3c2a9cc518326daf99a383f07c4d3c44317e4d` | One single package, almost all exported API, almost all of it documented — the generator's best case, and the shape that says what the golden set looks like when documentation is not the limiting factor. Picked from the plan's shortlist (`julienschmidt/httprouter`, `google/uuid`, `pkg/errors`). | **published** — three modes, both arms |
| `https://github.com/rs/zerolog` | `dfd11cca1143ba03ba0fc0ff14e5dbb4d61f6f0a` | Continuity. P2 and P3 both measured this commit, so its span counts, file counts and `tokenless=3` are already recorded, and a disagreement is a signal rather than a new number with no baseline. | **refused by the leakage probe** — two genuine leaks, below |
| `https://github.com/sirupsen/logrus` | `6d6a132bc03324d4ceb78e1b927f995d014cda20` | Several packages (the root plus `hooks/…` and `internal/…`) and a documentation rate expected to be materially lower, so the golden set is sparse relative to the corpus. | **refused by the leakage probe** — one false positive, below |

SHAs resolved with `git ls-remote <remote> HEAD` on 2026-09-03, before the
first indexer pass. No SHA was written here from memory: an invented one is
worse than none.

## The corpora, as the shipped indexer built them

Both arms of every repository, `STRIP_DOC_COMMENTS=true`, at the pinned commit.
The four counters come from each job's log line and are in no table.

| Repository | Files | AST spans | Window spans | Files with spans (AST / window) | vanished / unstrippable / tokenless / unparsed |
| --- | --- | --- | --- | --- | --- |
| `google/uuid` | 33 | 189 | 110 | 28 / 29 | 0 / 0 / 0 / 0 (both arms) |
| `rs/zerolog` | 99 | 1,303 | 768 | 87 / 88 | 0 / 0 / 0 / 0 (AST); 0 / 0 / **3** / 0 (window) |
| `sirupsen/logrus` | 64 | 628 | 349 | 59 / 60 | 0 / 0 / 0 / 0 (both arms) |

**`rs/zerolog` agrees with P2 exactly** — 99 files, 1,303 AST spans, 768 window
spans, `tokenless=3` on the window arm. Nothing changed since P2 measured it.
The stripped window arm's "files with spans" is **88 of 99**, measured here
rather than quoted from P2's production table, and it happens to equal the
production figure. The AST arm is 87 — `doc.go` and `tools.go` produce no AST
spans, so the two arms do not cover the same files, and the asymmetry lands in
the haystack rather than in the question set.

## Why two corpora were refused

The leakage probe scans every indexed span's text for every golden question's
prose and refuses a corpus that still holds any of it. That is how "the corpus
is stripped" is checked by its effect rather than by a flag nothing reads back
(spec:177). It refused two of the three.

### `rs/zerolog` — two genuine leaks, both classes stripping cannot close

514 cases, and the probe names two of them in **both arms**:

```
case "binary_test.go:Users.MarshalZerologArray:decl"  ->  binary_test.go:260-277
case "log/log_example_test.go:Example:decl"           ->  README.md:121-160
```

1. `binary_test.go:236` carries the doc comment `// User implements
   LogObjectMarshaler`. `binary_test.go:283` carries **the same sentence again,
   inside a function body**. `StripDocs` blanks doc comments and keeps every
   comment inside a body — measured, and stated in the plan — so the prose is
   still in the corpus.
2. `log/log_example_test.go`'s `Example` doc comment is repeated verbatim in
   `README.md`. `StripDocs` returns a non-Go file unchanged — verified — so no
   amount of Go stripping removes it. This is exactly the leak class the plan
   predicted for "a repository whose `README.md` repeats a doc comment".

Both leaks land in spans that are **not** the case's gold span, so they add a
distractor rather than making the eval easier. That is a fact about these two
cases, not a reason to overrule the guard: the rule is that a leaking corpus
produces numbers nobody can trust, and 2 of 514 cases is still a corpus whose
questions are partly in its own haystack.

### `sirupsen/logrus` — one false positive, and a defect in the probe

191 cases, and the probe names one, in the **window arm only**:

```
case "logrus.go:Level:decl"  ->  hooks/slog/handler.go:1-40
```

The doc comment on `type Level uint32` is the two words `Level type`. The named
window holds `LevelMapper func(slog.Level) logrus.Level` followed by a closing
brace and `type Handler struct`, which normalises to `… logrus level type
handler …` — and `level type` is a whole-phrase match inside it. The window
quotes nothing.

This is a **defect in the probe, not in the corpus**: `Normalise` collapses
every run of non-alphanumerics to one space, so it cannot tell the space
between two prose words from the `)` and newline between two code tokens, and a
two-word question is too short to be evidence either way. A first fix — match
whole phrases rather than raw substrings — removed an earlier and even weaker
false positive (`level type` inside `debuglevel type`) and is committed; it
does not close this one.

**Measured across all three corpora**, with the whole-phrase matcher:

| Repository | Cases | Distinct leaking cases | Genuine | False |
| --- | --- | --- | --- | --- |
| `google/uuid` | 74 | 0 | — | — |
| `rs/zerolog` | 514 | 2 (3 and 16 words) | 2 | 0 |
| `sirupsen/logrus` | 191 | 1 (2 words) | 0 | 1 |

Question-length distribution, in normalised words: `uuid` has none under four;
`zerolog` has one of three and four of four, 458 of 514 over eight; `logrus` has
one of two and one of four, 162 of 191 over eight. So short questions are rare
and long ones dominate — which is why an unstripped corpus still fails the
probe loudly (5 of 5 cases on the fixture repository) whatever is done about
the short ones.

**No threshold was applied.** Choosing a minimum question length after seeing
which length produced the false positive is the author choosing what the system
is asked, which is what spec:244's "generated, not hand-picked" forbids, and
the two genuine `zerolog` leaks are 3 and 16 words — a threshold that removed
the 2-word false positive would sit one word from a real finding. The defect is
recorded for review rather than patched to fit this data.

## What is *not* varied

One embedder (`nomic-embed-text` at 768 dimensions), one chunk configuration,
one candidate depth. Those are the shipped defaults, and a headline measured at
a depth production does not run is a headline about a different product.
