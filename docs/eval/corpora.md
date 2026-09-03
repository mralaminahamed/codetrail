# The corpora, named before the run

Three repositories, commit-pinned, chosen and written down **before any of them
was measured**, so the calibration cannot be the repository that gave the
nicest number.

**Every entry here gets a published run.** If one produces a distribution that
argues against the calibration the other two suggest, that is the finding and
it is recorded as the finding — not dropped.
`TestEveryNamedCorpusHasACommittedRun` reads this file, not the `runs/`
directory: a test that reads the directory agrees with whatever is there,
which is the definition of no test at all.

A floor calibrated on one repository is a floor calibrated on one library's
documentation style. Three is not statistics either, and the README says so —
the honest claim is the per-repository spread, not a confidence interval nobody
computed.

| Repository | Commit | Why this one |
| --- | --- | --- |
| `https://github.com/rs/zerolog` | `dfd11cca1143ba03ba0fc0ff14e5dbb4d61f6f0a` | Continuity. P2 and P3 both measured this commit, so its span counts (1,303 AST / 768 window, stripped), its file counts (87/88 of 99, production) and its `tokenless=3` are already recorded. A disagreement is a signal that something changed rather than a new number with no baseline. |
| `https://github.com/google/uuid` | `2d3c2a9cc518326daf99a383f07c4d3c44317e4d` | One single package, almost all exported API, almost all of it documented — the generator's best case, and the shape that says what the golden set looks like when documentation is not the limiting factor. Picked from the plan's shortlist (`julienschmidt/httprouter`, `google/uuid`, `pkg/errors`). |
| `https://github.com/sirupsen/logrus` | `6d6a132bc03324d4ceb78e1b927f995d014cda20` | Several packages (the root plus `hooks/…` and `internal/…`) and a documentation rate that is expected to be materially lower, so the golden set is sparse relative to the corpus. Whether the rate really is lower is a hypothesis this run tests rather than a fact assumed. |

SHAs resolved with `git ls-remote <remote> HEAD` on 2026-09-03, before the
first indexer pass. No SHA was written here from memory: an invented one is
worse than none.

## What is *not* varied

One embedder (`nomic-embed-text` at 768 dimensions), one chunk configuration,
one candidate depth. Those are the shipped defaults, and a headline measured at
a depth production does not run is a headline about a different product.
