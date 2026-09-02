-- The symbol graph (spec §3, §6): one row per definition, one row per call site.
--
-- Three columns §3 does not list, each because without it the row cannot do the
-- job the product's first sentence promises — an answer that cites file:line.
--   symbols.start_line/end_line: P2 sub-windows a declaration longer than
--     MaxDeclLines into kind=file spans, so the largest declarations have no
--     span of their own and a symbol locatable only through span_id would be
--     uncitable for exactly the definitions most worth asking about.
--   edges.path/line: "who calls this" would otherwise answer with a name and no
--     location.
--   symbols.path: denormalised beside file_id exactly as spans.path is, so a
--     definition is citable without a join, and because SymbolID hashes it.
--
-- span_id is nullable and linked by containment rather than by an exact range
-- match, for the same sub-windowing reason. Measured on symbols/testdata/
-- long.gotxt: the hole opens at WindowLines, not at MaxDeclLines — a 27-line
-- declaration over MaxDeclLines but inside one window yields one span whose
-- range equals the declaration's. Containment is right on both sides of that.
--
-- Every statement is IF NOT EXISTS-shaped and none is CONCURRENTLY: migrate()
-- runs the whole ledger in one transaction holding pg_advisory_xact_lock, and
-- CREATE INDEX CONCURRENTLY cannot run in a transaction block.
--
-- What "safe on a populated database" costs here, measured on pg17 by reading
-- pg_locks inside the transaction: each FK-bearing CREATE TABLE takes
-- ShareRowExclusiveLock (and AccessShareLock) on repos, files and spans, held
-- until the ledger commits. Reads are unaffected; concurrent writes to those
-- three tables block for the length of the migration.

CREATE TABLE IF NOT EXISTS symbols (
    id      TEXT PRIMARY KEY,
    repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    file_id TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    path    TEXT NOT NULL,
    name    TEXT NOT NULL,
    -- The package clause name, not the import path: the import path needs the
    -- module-aware load spec §6 allows to fail, and a column whose meaning
    -- depended on whether the type-check ran would be a per-package fact
    -- leaking into a per-row column.
    pkg     TEXT NOT NULL,
    kind    TEXT NOT NULL,
    -- 1-based and inclusive, as spans are (spec §3).
    start_line INTEGER NOT NULL CHECK (start_line >= 1),
    end_line   INTEGER NOT NULL CHECK (end_line >= start_line),
    -- SET NULL, not CASCADE: PutSpans deletes a repo's spans on every
    -- re-index, so a cascade would delete the repo's symbols as a side effect
    -- of re-chunking and the graph would vanish between two writes with no
    -- error. A definition came from the AST, not from the chunker.
    span_id TEXT REFERENCES spans(id) ON DELETE SET NULL
);
-- Definitions are looked up by name within one repo.
CREATE INDEX IF NOT EXISTS symbols_repo_name_idx ON symbols (repo_id, name);
-- Not for a query: the ON DELETE SET NULL above is executed as an UPDATE
-- against this column on every PutSpans, and without an index that is a
-- sequential scan of symbols per deleted span.
CREATE INDEX IF NOT EXISTS symbols_span_idx ON symbols (span_id);

CREATE TABLE IF NOT EXISTS edges (
    id             TEXT PRIMARY KEY,
    repo_id        TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    from_symbol_id TEXT NOT NULL REFERENCES symbols(id) ON DELETE CASCADE,
    -- CASCADE rather than SET NULL: nulling it would leave provenance =
    -- 'resolved' beside a null target, which the pair constraint forbids, so
    -- the delete would fail instead. Measured on pg17 — the counterfactual
    -- errors with "new row for relation ... violates check constraint" from
    -- inside the FK's own UPDATE.
    to_symbol_id   TEXT REFERENCES symbols(id) ON DELETE CASCADE,
    -- spec:84: a syntactic edge knows it calls something named Close.
    to_name        TEXT NOT NULL,
    kind           TEXT NOT NULL CHECK (kind IN ('calls', 'imports', 'references')),
    provenance     TEXT NOT NULL CHECK (provenance IN ('resolved', 'syntactic')),
    -- The call site. Without it "who calls this" has no file:line to cite.
    path           TEXT NOT NULL,
    line           INTEGER NOT NULL CHECK (line >= 1),
    -- §3's invariant made true by construction rather than by convention: a
    -- syntactic edge has a null target (spec:84) and a resolved one points at a
    -- symbol row (spec:71). In Go alone, a future writer or a hand-run UPDATE
    -- could produce a row claiming a precision nothing records.
    --
    -- provenance's NOT NULL is load-bearing, and the plan's reasoning for why
    -- there is no three-valued-logic hole was wrong: the left side is indeed
    -- never NULL, but the RIGHT side is NULL when provenance is, the equality
    -- is then NULL, and a CHECK evaluating to NULL passes. Measured on pg17:
    -- without NOT NULL both (NULL, NULL) and ('x', NULL) are accepted.
    CONSTRAINT edges_provenance_target
        CHECK ((to_symbol_id IS NOT NULL) = (provenance = 'resolved'))
);
CREATE INDEX IF NOT EXISTS edges_repo_idx ON edges (repo_id);
-- CallersOf walks edges backwards from a target, so this is the traversal's
-- index; edges_from_idx is what the FK's own cascade uses.
CREATE INDEX IF NOT EXISTS edges_to_idx ON edges (to_symbol_id);
CREATE INDEX IF NOT EXISTS edges_from_idx ON edges (from_symbol_id);
-- The approximate reader's set: name-matched callers, which by definition have
-- no target. Partial, because a resolved edge is never in that answer.
CREATE INDEX IF NOT EXISTS edges_approx_idx ON edges (repo_id, to_name)
    WHERE to_symbol_id IS NULL;
