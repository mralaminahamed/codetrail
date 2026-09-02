-- An evicted repo answers 410 Gone, not 404: it existed, and that is a
-- different fact (spec §10). Eviction is one DELETE with a cascade, so
-- afterwards nothing in the database remembers — hence a tombstone, written by
-- the same statement as the delete so the two cannot disagree.
--
-- No foreign key to repos: the row this describes is the one just deleted, and
-- a reference would take the tombstone with it.
--
-- indexed_at is copied from the repo rather than defaulted, so the tombstone
-- says when the corpus held it, not when it stopped.
CREATE TABLE IF NOT EXISTS evicted_repos (
    id         TEXT PRIMARY KEY,
    remote     TEXT NOT NULL,
    ref        TEXT NOT NULL,
    commit_sha TEXT NOT NULL,
    indexed_at TIMESTAMPTZ NOT NULL,
    evicted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The bound is "the newest KEEP_TOMBSTONES", which is this order. Measured on
-- pg17: at the 600 rows the default bound leaves, the planner ignores it and
-- seq-scans plus sorts in half a millisecond; at 50k it is an Index Scan
-- Backward, with an incremental sort for Evict's id tiebreak. So it earns its
-- place only where the bound is raised or eviction outruns the trim.
--
-- Plain, not CONCURRENTLY: migrate() runs the whole ledger in one transaction
-- holding pg_advisory_xact_lock, and CREATE INDEX CONCURRENTLY cannot run in
-- a transaction block.
CREATE INDEX IF NOT EXISTS evicted_repos_at_idx ON evicted_repos (evicted_at);

-- What a finished job produced. Without it a caller who polls a job to done
-- cannot name the repository it indexed: the repo id is hash(key, commit) and
-- only the indexer ever saw the commit.
--
-- Nullable, because a job that is still running has not got one. Added rather
-- than dropped and re-added, so a re-run does not erase what Complete wrote.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS repo_id TEXT;
