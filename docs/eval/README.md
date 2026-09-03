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

| Date | Repository | Commit | Embedder | Mode | File |
| --- | --- | --- | --- | --- | --- |
| _(none yet)_ | | | | | |

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
`unparsed` are in no table — the indexer has no `/metrics` endpoint — and the
artefact cannot get them any other way. Paste them into `-ast-counters` and
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
