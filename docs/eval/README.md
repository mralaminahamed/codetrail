# Eval artefacts

Spec §9's harness writes one JSON file per run. This directory is the index.

## How to read a file

`quality` is the **first key** and `banner` the second. A run on `embed.Fake`
has `"quality": false`, a banner naming spec §9, and the model name
`fake-hashed-bow` in its filename — **its figures are not quality**. Those runs
land in `mechanics/`, which is gitignored: nothing here is a measurement unless
it says so on all four surfaces.

Every file carries its whole configuration and every per-case record, so the
floor sweep is recomputable from the committed file with no embedder in the
loop. That is what "recorded with the numbers that produced it" (spec:249) has
to mean if it is to mean anything.

Two figures qualify every metric and are printed beside it:

- **the gold-set sizes.** Gold is range overlap, reported under two rules —
  `lenient` (every span overlapping the declaration) and `strict` (the largest
  overlap). Neither is neutral between the arms: a 100-line declaration is one
  AST span and three windows, so the lenient rule hands the window arm three
  chances and the strict rule penalises it for tiling.
- **the span and file counts.** The two arms do not rank the same number of
  documents. A smaller haystack is easier and a more coherent document is
  better; both directions are real.

`unscoreable` counts cases with no gold span in that arm. They are in no
denominator: a case with no right answer here cannot be got wrong here.

## Runs

Real runs only, newest first. A mechanics run never appears in this table.
`docs/eval/corpora.md` accounts for every named corpus, including the two the
leakage probe refused.

| Date | Repository | Commit | Embedder | Mode | File |
| --- | --- | --- | --- | --- | --- |
| 2026-09-03 | `google/uuid` | `2d3c2a9` | `nomic-embed-text`/768 | hybrid | [`runs/2026-09-03-google-uuid-hybrid-nomic-embed-text.json`](runs/2026-09-03-google-uuid-hybrid-nomic-embed-text.json) |
| 2026-09-03 | `google/uuid` | `2d3c2a9` | `nomic-embed-text`/768 | vector | [`runs/2026-09-03-google-uuid-vector-nomic-embed-text.json`](runs/2026-09-03-google-uuid-vector-nomic-embed-text.json) |
| 2026-09-03 | `google/uuid` | `2d3c2a9` | `nomic-embed-text`/768 | lexical | [`runs/2026-09-03-google-uuid-lexical-nomic-embed-text.json`](runs/2026-09-03-google-uuid-lexical-nomic-embed-text.json) |

## What the one published corpus says

`google/uuid` at `2d3c2a9`, 74 generated cases, 5 duplicate questions, **0
unscoreable** in either arm, and **74 of 74** cases whose answer range moved
under stripping — the corpus-scale version of P2's "506 of 1,303". Read every
figure with the qualifiers beside it; one repository is not a distribution.

MRR, both gold rules, at the shipped defaults (`k=60`, `candidates=40`,
`split=true`, `MaxSpans=5`):

| Arm | Spans | Mean gold set | vector | hybrid | lexical |
| --- | --- | --- | --- | --- | --- |
| AST | 189 | 1.000 | **0.749** / 0.749 | 0.402 / 0.402 | 0.164 / 0.164 |
| window | 110 | 1.500 | **0.606** / 0.510 | 0.484 / 0.422 | 0.325 / 0.297 |

hit@1 / hit@5 / hit@10, lenient rule:

| Arm | vector | hybrid | lexical |
| --- | --- | --- | --- |
| AST | 0.622 / 0.905 / 0.946 | 0.243 / 0.635 / 0.757 | 0.068 / 0.270 / 0.378 |
| window | 0.432 / 0.851 / 0.946 | 0.351 / 0.676 / 0.811 | 0.216 / 0.500 / 0.608 |

**Spec:316 is answered, and the answer is no.** Hybrid does not beat the vector
arm on this golden set — it is beaten by it, on both arms and at every k. In
the AST arm's hybrid run the gold span was found by the vector arm alone in 1
case, by the lexical arm alone in **0**, and by both in 55: the lexical arm
contributed no unique find and its ranking dragged the fusion down. This is one
repository and one question distribution, and the distribution is handicapped
for the lexical arm by construction — a doc-comment question tokenises to
mostly stopwords and the `simple` text-search configuration has no stopword
list — so the honest statement is that **on this question distribution fusion
costs accuracy**, not that fusion is worthless.

**The two arms are not comparable on gold-set size alone.** The window arm's
mean lenient gold set is 1.5 spans against the AST arm's 1.0, so the lenient
rule hands it more chances; under the strict rule its hit@1 falls from 0.351 to
0.257 in hybrid and from 0.432 to 0.324 in vector, while the AST arm's figures
are identical under both rules. Its haystack is also smaller — 110 spans
against 189. Both directions are real and neither is corrected for.

Top cosine similarity, the quantity `rag.Decide` compares:

| Arm | min | p10 | median | p90 | max |
| --- | --- | --- | --- | --- | --- |
| AST | 0.6239 | 0.6598 | 0.7479 | 0.8227 | 0.8507 |
| window | 0.5384 | 0.5866 | 0.7190 | 0.8079 | 0.8386 |

P3 measured 0.664 to 0.744 on `rs/zerolog` over seven hand-written questions
and called it "a range, not a recommendation". This generated distribution is
wider on both sides.

**The shipped-floor confusion is the null row, and it is reported as one.**
`DefaultFloor()` is `{-1, false}` and `Decide` refuses on `top < f.Value`;
cosine similarity is bounded below by -1, so `below_floor` is unreachable.
Measured: at floor -1 every run above shows 0 refusals and an empty reason
tally. §9's "refusal behaviour" is measured by the *sweep*, across floors the
product does not run at, and every artefact carries the whole curve.

**The floor is not calibrated.** That needs three repositories and this run
produced one — see `corpora.md`. A floor read off one library's documentation
style is the thing the "three, named before the run" discipline exists to
prevent, so `rag.DefaultFloor` is unchanged. Every shipped sentence about the
floor now branches on `Floor.Calibrated` rather than asserting a state or
naming a phase, and `TestNoShippedSurfaceAssertsTheFloorsCalibrationState`
fails the build if one starts asserting again.

## Recipe

Build both arms from one commit, into **two databases** — one arm per database,
because `RepoID = hash(key, commit)` has no room for a strategy and a second
arm indexed into the first arm's database silently replaces it (P2 measured
this live).

```bash
# arm A — AST spans, doc comments stripped
CHUNK_STRATEGY=ast    STRIP_DOC_COMMENTS=true DATABASE_URL=<ast dsn>    ./bin/indexer
# arm B — fixed windows, doc comments stripped, the same commit
CHUNK_STRATEGY=window STRIP_DOC_COMMENTS=true DATABASE_URL=<window dsn> ./bin/indexer
```

**Capture each job's log line.** `vanished`, `unstrippable`, `tokenless` and
`unparsed` are in no table and in no instrument — the indexer's `/metrics`
endpoint exports the job counters, not these four — and the artefact cannot get
them any other way. Paste them into `-ast-counters` and
`-window-counters`. P2 measured `tokenless=3` on `rs/zerolog`'s window arm; a
corpus can shrink by exactly that much without anyone noticing.

Then run the harness against a checkout of the same commit:

```bash
./bin/evalrunner \
  -ast-dsn <ast dsn> -window-dsn <window dsn> \
  -repo <repo id> -commit <sha> -src <checkout> \
  -ast-counters vanished=0,unstrippable=0,tokenless=0,unparsed=0 \
  -window-counters vanished=0,unstrippable=0,tokenless=3,unparsed=0
```

The harness refuses before it retrieves. It checks that the two arms are in
different databases, at one commit, over byte-identical files, with different
span sets — and that the checkout `-src` names hashes to what both arms
indexed. It then probes every indexed span for the prose of every question and
refuses a corpus that still holds any of it, which is how "the corpus is
stripped" is checked by its effect rather than by a flag nothing reads back.

`make eval-corpus` is the two indexer passes as one command.
