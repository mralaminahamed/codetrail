-- The lexical arm (spec §8): a weighted tsvector over a span's symbol and its
-- text, with a GIN index.
--
-- A stored generated column rather than an expression index. Measured on pg17,
-- because the obvious reason is false: an expression index repeated verbatim in
-- the WHERE does give a Bitmap Index Scan, so an expression index is not a
-- sequential scan. What it costs is that the expression has to be repeated
-- character for character in every query — changing one setweight letter turned
-- the same plan into a Seq Scan with no error, only a slower answer — and that
-- ts_rank_cd then recomputes the vector for every row it ranks. A column named
-- lex cannot drift from itself.
--
-- 'simple', not 'english': a stemmer collapses Files and filing, and Get, New
-- and Do are near-stopwords in Go. Code is not English. The two-argument
-- to_tsvector is also the only immutable form — the one-argument one reads
-- default_text_search_config — so a generated column has to name the
-- configuration anyway.
--
-- symbol at weight A, text at weight B. A span's identifiers are its text's
-- tokens, so indexing the text is how "identifiers" get indexed at all; the
-- weight is what keeps a definition above a mention. It also indexes keywords,
-- string literals and comment prose. Narrowing it to identifiers means writing
-- them from the AST at index time, which is a new column and a full re-index.
--
-- The dots in a method's symbol become spaces first. Measured: to_tsvector(
-- 'simple', 'Store.Get') is the single token 'store.get' — the parser calls it
-- a host — which no query term built from letters and digits can match, so
-- without this the A weight is dead for every method in the corpus.
ALTER TABLE spans ADD COLUMN IF NOT EXISTS lex tsvector
    GENERATED ALWAYS AS (
        setweight(to_tsvector('simple', replace(symbol, '.', ' ')), 'A') ||
        setweight(to_tsvector('simple', text), 'B')
    ) STORED;

-- Plain, not CONCURRENTLY: migrate() runs the whole ledger inside one
-- transaction holding pg_advisory_xact_lock, and CREATE INDEX CONCURRENTLY
-- cannot run in a transaction block.
CREATE INDEX IF NOT EXISTS spans_lex_idx ON spans USING gin (lex);
